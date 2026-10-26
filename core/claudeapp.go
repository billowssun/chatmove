package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The Claude desktop app keeps one metadata file per conversation
// (local_<app id>.json, holding the transcript's cliSessionId) and leaves a
// deleted_<id> marker when a conversation is removed. The transcript itself
// can outlive the removal, so exports consult this list to leave removed
// conversations behind.

type claudeSessionMeta struct {
	Cwd      string
	Archived bool
	File     string // the local_*.json file
	Rel      string // <account>/<organization> it sits under
}

type claudeAppState struct {
	hasApp  bool
	live    map[string]claudeSessionMeta // by cliSessionId
	deleted map[string]bool              // app ids and cliSessionIds
}

// claudeMetaRoots lists where desktop builds keep claude-code-sessions: the
// regular installer under %APPDATA%\Claude, the Microsoft Store build under
// %LOCALAPPDATA%\Claude-Data\<package>.
func claudeMetaRoots() []string {
	var roots []string
	if a := os.Getenv("APPDATA"); a != "" {
		roots = append(roots, filepath.Join(a, "Claude", "claude-code-sessions"))
	}
	if l := os.Getenv("LOCALAPPDATA"); l != "" {
		m, _ := filepath.Glob(filepath.Join(l, "Claude-Data", "*", "claude-code-sessions"))
		roots = append(roots, m...)
	}
	return roots
}

func loadClaudeApp() claudeAppState {
	st := claudeAppState{live: map[string]claudeSessionMeta{}, deleted: map[string]bool{}}
	for _, root := range claudeMetaRoots() {
		// <root>/<account>/<organization>/{local_*.json, deleted_*}
		files, _ := filepath.Glob(filepath.Join(root, "*", "*", "*"))
		for _, f := range files {
			name := filepath.Base(f)
			switch {
			case strings.HasPrefix(name, "deleted_"):
				st.deleted[strings.TrimPrefix(name, "deleted_")] = true
			case strings.HasPrefix(name, "local_") && strings.HasSuffix(name, ".json"):
				b, err := os.ReadFile(f)
				if err != nil {
					continue
				}
				var m struct {
					CliSessionID string `json:"cliSessionId"`
					Cwd          string `json:"cwd"`
					IsArchived   bool   `json:"isArchived"`
				}
				if json.Unmarshal(b, &m) != nil || m.CliSessionID == "" {
					continue
				}
				st.hasApp = true
				rel, _ := filepath.Rel(root, filepath.Dir(f))
				st.live[m.CliSessionID] = claudeSessionMeta{m.Cwd, m.IsArchived, f, filepath.ToSlash(rel)}
			}
		}
	}
	return st
}

// removed reports whether the user took this conversation out of the app's
// list: deleted, archived, or (when the desktop app is in use at all) no
// longer listed. Without the desktop app every transcript counts as live.
func (s claudeAppState) removed(sid string) bool {
	if s.deleted[sid] {
		return true
	}
	if !s.hasApp {
		return false
	}
	m, ok := s.live[sid]
	return !ok || m.Archived
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// claudeSplit breaks claude/projects/<project>/<rest> into its parts.
func claudeSplit(rel string) (project, rest string, ok bool) {
	r, found := strings.CutPrefix(rel, "claude/projects/")
	if !found {
		return "", "", false
	}
	project, rest, ok = strings.Cut(r, "/")
	return
}

// claudeSessionOf returns the conversation a file under a project belongs
// to: <uuid>.jsonl, the <uuid>/ folder beside it (titles, sub-agents, tool
// output) and <uuid>.desktop-released.json. Project-wide files such as
// memory/*.md return "".
func claudeSessionOf(rel string) string {
	_, rest, ok := claudeSplit(rel)
	if !ok {
		return ""
	}
	first, _, _ := strings.Cut(rest, "/")
	return uuidRe.FindString(first)
}

var cwdRe = regexp.MustCompile(`"cwd":"((?:[^"\\]|\\.)*)"`)

// firstCwd finds the first "cwd" value in a transcript, reading at most the
// first 32 MB (lines carrying screenshots can be several MB each).
func firstCwd(r interface{ Read([]byte) (int, error) }) string {
	buf := make([]byte, 0, 1<<20)
	chunk := make([]byte, 1<<20)
	for len(buf) < 32<<20 {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if m := cwdRe.FindSubmatch(buf); m != nil {
			var s string
			if json.Unmarshal(append(append([]byte{'"'}, m[1]...), '"'), &s) == nil {
				return s
			}
		}
		if err != nil {
			break
		}
	}
	return ""
}

const claudeMetaPrefix = "claude/app-sessions/"

// claudeMetaImport is one desktop-app metadata file from a pack, with the
// working folder it should point at on this machine.
type claudeMetaImport struct {
	rel string         // <account>/<organization>
	obj map[string]any // the original local_*.json
	sid string         // cliSessionId
	cwd string         // working folder on this machine
}

// writeClaudeMeta adds the desktop app's list entries for imported
// conversations. It skips conversations already listed or removed here, and
// returns how many it wrote.
func writeClaudeMeta(metas []claudeMetaImport, local claudeAppState) (int, error) {
	var roots []string
	for _, r := range claudeMetaRoots() {
		if dirExists(r) {
			roots = append(roots, r)
		}
	}
	if len(roots) == 0 || len(metas) == 0 {
		return 0, nil // no desktop app here; the CLI finds transcripts on its own
	}
	// Prefer the same account/organization folder; fall back to the only one
	// this machine has.
	var only string
	if dirs, _ := filepath.Glob(filepath.Join(roots[0], "*", "*")); len(dirs) == 1 {
		only = dirs[0]
	}
	written := 0
	for _, m := range metas {
		if _, listed := local.live[m.sid]; listed || local.deleted[m.sid] {
			continue
		}
		dir := ""
		for _, r := range roots {
			if d := filepath.Join(r, filepath.FromSlash(m.rel)); dirExists(d) {
				dir = d
				break
			}
		}
		if dir == "" {
			dir = only
		}
		if dir == "" {
			dir = filepath.Join(roots[0], filepath.FromSlash(m.rel))
		}
		id := str(m.obj["sessionId"])
		if !strings.HasPrefix(id, "local_") {
			continue
		}
		dst := filepath.Join(dir, id+".json")
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		for _, k := range []string{"cwd", "originCwd", "gitAnchorsFolderRealpath"} {
			if _, ok := m.obj[k]; ok {
				m.obj[k] = m.cwd
			}
		}
		b, err := json.Marshal(m.obj)
		if err != nil {
			return written, err
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			return written, err
		}
		if err := writeAtomic(dst, b); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}
