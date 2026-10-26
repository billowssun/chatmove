package core

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Files a conversation wrote outside its working folder (a render saved to
// the desktop, a report in another folder) are found in the transcript: the
// file-writing tool calls record their paths. Files produced indirectly, by a
// script the assistant ran, are not recorded and cannot be found this way.

const outputPrefix = "external/"

var claudeWriteTools = map[string]bool{"Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true}

// claudeWrittenPaths lists files a Claude transcript wrote or edited.
func claudeWrittenPaths(r io.Reader) []string {
	var out []string
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if bytes.Contains(line, []byte(`"tool_use"`)) && bytes.Contains(line, []byte(`_path"`)) {
			var v struct {
				Message struct {
					Content []struct {
						Type  string         `json:"type"`
						Name  string         `json:"name"`
						Input map[string]any `json:"input"`
					} `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &v) == nil {
				for _, c := range v.Message.Content {
					if c.Type == "tool_use" && claudeWriteTools[c.Name] {
						for _, k := range []string{"file_path", "notebook_path"} {
							if s := str(c.Input[k]); s != "" {
								out = append(out, s)
							}
						}
					}
				}
			}
		}
		if err != nil {
			return out
		}
	}
}

// codexWrittenPaths lists files a Codex rollout created, edited or moved,
// from apply_patch calls and file-change records.
func codexWrittenPaths(r io.Reader) []string {
	var out []string
	var walk func(v any, inChange bool)
	patch := func(s string) {
		for _, l := range strings.Split(s, "\n") {
			for _, pre := range []string{"*** Add File: ", "*** Update File: ", "*** Move to: "} {
				if p, ok := strings.CutPrefix(strings.TrimRight(l, "\r"), pre); ok {
					out = append(out, strings.TrimSpace(p))
				}
			}
		}
	}
	walk = func(v any, inChange bool) {
		switch t := v.(type) {
		case string:
			if strings.Contains(t, "*** Begin Patch") {
				patch(t)
			} else if strings.HasPrefix(t, "{") && strings.Contains(t, "*** ") {
				var inner any // tool arguments are JSON inside a string
				if json.Unmarshal([]byte(t), &inner) == nil {
					walk(inner, inChange)
				}
			}
		case []any:
			for _, x := range t {
				walk(x, inChange)
			}
		case map[string]any:
			kind := strings.ToLower(str(t["type"]))
			change := inChange || kind == "filechange" || kind == "file_change"
			for k, x := range t {
				if change && k == "path" {
					if s := str(x); s != "" {
						out = append(out, s)
					}
				}
				walk(x, change)
			}
		}
	}
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if bytes.Contains(line, []byte("*** ")) || bytes.Contains(line, []byte("ileChange")) || bytes.Contains(line, []byte("ile_change")) {
			var v any
			if json.Unmarshal(line, &v) == nil {
				walk(v, false)
			}
		}
		if err != nil {
			return out
		}
	}
}

// outputKey turns an absolute file path into its place in the pack.
func outputKey(p string) (string, bool) {
	k, ok := workspaceKey(p)
	if !ok {
		return "", false
	}
	return outputPrefix + strings.TrimPrefix(k, workspacePrefix), true
}

// addOutputs adds files the kept conversations wrote outside the packed
// working folders. transcripts maps each transcript file to the folder its
// relative paths are resolved against.
func addOutputs(files map[string]localFile, app App, transcripts map[string]string, packed []string, appDirs ...string) (count int, bytes int64) {
	seen := map[string]bool{}
	for path, cwd := range transcripts {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		var paths []string
		if app == AppClaude {
			paths = claudeWrittenPaths(f)
		} else if r, done, err := openRollout(path, f); err == nil {
			paths = codexWrittenPaths(r)
			done()
		}
		f.Close()
		for _, p := range paths {
			p = strings.TrimPrefix(p, `\\?\`)
			if !filepath.IsAbs(p) {
				if cwd == "" {
					continue
				}
				p = filepath.Join(strings.TrimPrefix(cwd, `\\?\`), p)
			}
			p = filepath.Clean(p)
			k := strings.ToLower(p)
			if seen[k] {
				continue
			}
			seen[k] = true
			if insideAny(p, packed) || insideAny(p, appDirs) || inSkipDir(p) {
				continue
			}
			info, err := os.Stat(p)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			key, ok := outputKey(p)
			if !ok {
				continue
			}
			files[key] = localFile{p, info.Size(), info.ModTime().UnixNano()}
			count++
			bytes += info.Size()
		}
	}
	return
}

func insideAny(p string, dirs []string) bool {
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if _, ok := cutPrefixFold(p, filepath.Clean(d)+`\`); ok {
			return true
		}
	}
	return false
}

func inSkipDir(p string) bool {
	for _, part := range strings.Split(filepath.Dir(p), `\`) {
		if skipDirs[part] {
			return true
		}
	}
	return false
}
