package core

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Codex keeps its thread list in state_5.sqlite, which chatsync never touches.
// After pulling rollouts we ask Codex itself (via `codex app-server`) to
// rescan them into the DB, then restore custom titles from session_index.jsonl
// because Codex only reads titles from the DB's threads.name column.

var ErrCodexRunning = errors.New("codex is running")

// runningCodexCheck is swapped out in tests.
var runningCodexCheck = RunningCodex

// RunningCodex returns any codex.exe processes: the desktop app, the CLI and
// the VS Code extension's app-server all run under that name. Helpers such as
// codex-windows-sandbox-service.exe can outlive the app and are ignored.
func RunningCodex() ([]string, error) {
	out, err := exec.Command("tasklist", "/FO", "CSV", "/NH").Output()
	if err != nil {
		return nil, err
	}
	rows, err := csv.NewReader(bytes.NewReader(out)).ReadAll()
	if err != nil {
		return nil, err
	}
	var found []string
	for _, r := range rows {
		if len(r) > 1 && strings.EqualFold(r[0], "codex.exe") {
			found = append(found, r[0]+" (pid "+r[1]+")")
		}
	}
	return found, nil
}

type rpcClient struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	msgs chan map[string]json.RawMessage
	next int
}

func startAppServer(cfg *Config) (*rpcClient, error) {
	bin := cfg.CodexCommand
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("codex"); err != nil {
			return nil, fmt.Errorf("codex CLI not found in PATH (set codex_command in the config)")
		}
	}
	cmd := exec.Command(bin, "app-server")
	cmd.Env = append(os.Environ(), "CODEX_HOME="+cfg.CodexDir)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &rpcClient{cmd: cmd, in: in, msgs: make(chan map[string]json.RawMessage, 64)}
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 1<<20), 256<<20)
		for sc.Scan() {
			var m map[string]json.RawMessage
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				c.msgs <- m
			}
		}
		close(c.msgs)
	}()
	return c, nil
}

func (c *rpcClient) send(v any) error {
	b, _ := json.Marshal(v)
	_, err := c.in.Write(append(b, '\n'))
	return err
}

// call sends a request and waits for its response, skipping notifications.
func (c *rpcClient) call(method string, params any, out any) error {
	c.next++
	id := c.next
	if err := c.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return err
	}
	timeout := time.After(5 * time.Minute)
	for {
		select {
		case m, ok := <-c.msgs:
			if !ok {
				return fmt.Errorf("%s: codex app-server exited", method)
			}
			var got int
			if json.Unmarshal(m["id"], &got) != nil || got != id || m["method"] != nil {
				continue
			}
			if e := m["error"]; e != nil {
				return fmt.Errorf("%s: %s", method, e)
			}
			if out != nil {
				return json.Unmarshal(m["result"], out)
			}
			return nil
		case <-timeout:
			return fmt.Errorf("%s: timed out", method)
		}
	}
}

func (c *rpcClient) close() {
	c.in.Close()
	done := make(chan struct{})
	go func() { c.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		c.cmd.Process.Kill()
		<-done
	}
}

type codexThread struct {
	ID   string  `json:"id"`
	Name *string `json:"name"`
}

// indexNames returns the newest thread_name per id from session_index.jsonl.
func indexNames(codexDir string) map[string]string {
	b, err := os.ReadFile(filepath.Join(codexDir, "session_index.jsonl"))
	if err != nil {
		return nil
	}
	names, newest := map[string]string{}, map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		var v struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
			UpdatedAt  string `json:"updated_at"`
		}
		if json.Unmarshal([]byte(line), &v) != nil || v.ID == "" || v.ThreadName == "" {
			continue
		}
		if v.UpdatedAt >= newest[v.ID] {
			names[v.ID], newest[v.ID] = v.ThreadName, v.UpdatedAt
		}
	}
	return names
}

// CodexReindex rescans rollouts into Codex's DB and restores titles.
// Unless force is set it refuses to run while any Codex process is alive.
func CodexReindex(cfg *Config, log io.Writer, force bool) error {
	if !force {
		procs, err := runningCodexCheck()
		if err != nil {
			return fmt.Errorf("check running processes: %w", err)
		}
		if len(procs) > 0 {
			return fmt.Errorf("%w: %s", ErrCodexRunning, strings.Join(procs, ", "))
		}
	}
	c, err := startAppServer(cfg)
	if err != nil {
		return err
	}
	defer c.close()
	if err := c.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "chatsync", "version": "1"}}, nil); err != nil {
		return err
	}
	if err := c.send(map[string]any{"method": "initialized"}); err != nil {
		return err
	}

	// A full (non state-DB-only) listing makes Codex scan rollouts and add the
	// ones its DB doesn't know yet. Archived threads are listed separately.
	var threads []codexThread
	for _, archived := range []bool{false, true} {
		var cursor *string
		for {
			var resp struct {
				Data       []codexThread `json:"data"`
				NextCursor *string       `json:"nextCursor"`
			}
			params := map[string]any{"limit": 100, "archived": archived}
			if cursor != nil {
				params["cursor"] = *cursor
			}
			if err := c.call("thread/list", params, &resp); err != nil {
				return err
			}
			threads = append(threads, resp.Data...)
			if resp.NextCursor == nil || *resp.NextCursor == "" || len(resp.Data) == 0 {
				break
			}
			cursor = resp.NextCursor
		}
	}

	names := indexNames(cfg.CodexDir)
	restored := 0
	for _, t := range threads {
		want := names[t.ID]
		if want == "" || (t.Name != nil && *t.Name == want) {
			continue
		}
		if err := c.call("thread/name/set", map[string]string{"threadId": t.ID, "name": want}, nil); err != nil {
			fmt.Fprintf(log, "  WARN rename %s: %v\n", t.ID, err)
			continue
		}
		restored++
	}
	fmt.Fprintf(log, "codex reindex: %d threads in Codex, %d titles restored\n", len(threads), restored)
	return nil
}
