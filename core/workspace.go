package core

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Picking up a conversation on another computer needs more than the
// transcript: the project folder it worked in, the files it generated and
// what was attached to it. Those travel in the pack too:
//
//	workspace/~/<path under the home folder>   e.g. Documents/Codex/2026-10-01/x
//	workspace/<drive>/<path>                   e.g. D/vibe motion/src/app.tsx
//	claude/file-history/<session>/...          Claude's undo snapshots
//	codex/generated_images/<thread>/...        images Codex generated
//	codex/attachments/<thread>/..., codex/codex-remote-attachments/<thread>/...

const workspacePrefix = "workspace/"

// skipDirs are dependency and cache folders that are rebuilt by installing
// the project again, and are often far larger than the project itself.
var skipDirs = map[string]bool{
	"node_modules": true, ".venv": true, "venv": true, "__pycache__": true,
	".next": true, ".turbo": true, ".cache": true, ".pnpm-store": true,
	".gradle": true, ".parcel-cache": true,
}

// broadFolder reports folders too general to pack whole: a drive root, the
// home folder, or one of its standard top-level folders.
func broadFolder(p string) bool {
	home, _ := os.UserHomeDir()
	return broadFolderIn(p, home)
}

func broadFolderIn(p, home string) bool {
	p = filepath.Clean(strings.TrimPrefix(p, `\\?\`))
	if len(p) <= 3 { // C:\
		return true
	}
	if strings.EqualFold(p, home) {
		return true
	}
	for _, d := range []string{"Desktop", "Documents", "Downloads", "Pictures", "Videos", "Music", "OneDrive", "AppData"} {
		if strings.EqualFold(p, filepath.Join(home, d)) {
			return true
		}
	}
	return false
}

// workspaceKey turns an absolute folder into its place in the pack.
func workspaceKey(p string) (string, bool) {
	p = filepath.Clean(strings.TrimPrefix(p, `\\?\`))
	home, _ := os.UserHomeDir()
	if rest, ok := cutPrefixFold(p, home+`\`); ok {
		return workspacePrefix + "~/" + filepath.ToSlash(rest), true
	}
	vol := filepath.VolumeName(p)
	if len(vol) != 2 || vol[1] != ':' {
		return "", false // network shares and the like stay where they are
	}
	return workspacePrefix + strings.ToUpper(vol[:1]) + "/" + filepath.ToSlash(strings.TrimPrefix(p[2:], `\`)), true
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// workspaceOrigin turns a pack path back into the file's path on the
// exporting machine; the relocator decides where it lives now.
func workspaceOrigin(rel, oldHome string) string {
	rest := strings.TrimPrefix(rel, workspacePrefix)
	if r, ok := strings.CutPrefix(rest, "~/"); ok {
		return filepath.Join(oldHome, filepath.FromSlash(r))
	}
	drive, r, _ := strings.Cut(rest, "/")
	return filepath.Join(drive+`:\`, filepath.FromSlash(r))
}

type workspaceSet struct {
	folders []string // packed, outermost first
	skipped []string // too broad, or not on this machine
}

// collectWorkspaces de-duplicates the conversations' working folders, keeping
// only the outermost of nested ones.
func collectWorkspaces(cwds []string, appDirs ...string) workspaceSet {
	var ws workspaceSet
	seen := map[string]bool{}
	var cands []string
	for _, c := range cwds {
		c = filepath.Clean(strings.TrimPrefix(c, `\\?\`))
		k := strings.ToLower(c)
		if c == "." || seen[k] {
			continue
		}
		seen[k] = true
		switch {
		case broadFolder(c):
			ws.skipped = append(ws.skipped, c)
		case !dirExists(c):
		default:
			inside := false
			for _, a := range appDirs { // never pack ~/.claude or ~/.codex wholesale
				if _, ok := cutPrefixFold(c+`\`, a+`\`); ok {
					inside = true
				}
			}
			if !inside {
				cands = append(cands, c)
			}
		}
	}
	sort.Slice(cands, func(i, j int) bool { return len(cands[i]) < len(cands[j]) })
	for _, c := range cands {
		nested := false
		for _, f := range ws.folders {
			if _, ok := cutPrefixFold(c+`\`, f+`\`); ok {
				nested = true
				break
			}
		}
		if !nested {
			ws.folders = append(ws.folders, c)
		}
	}
	sort.Strings(ws.skipped)
	return ws
}

// addTree lists every file under dir into files, keyed by prefix + relative
// path, leaving out dependency folders and anything unreadable.
func addTree(files map[string]localFile, dir, prefix string) {
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != dir && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasSuffix(p, tmpSuffix) {
			return nil // links, devices, half-written temp files
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		files[prefix+filepath.ToSlash(rel)] = localFile{p, info.Size(), info.ModTime().UnixNano()}
		return nil
	})
}

// perConversationDirs lists the app folders that hold one subfolder per
// conversation, as (pack prefix, local folder).
func perConversationDirs(app App, p Paths) [][2]string {
	if app == AppClaude {
		return [][2]string{{"claude/file-history/", filepath.Join(p.ClaudeDir, "file-history")}}
	}
	return [][2]string{
		{"codex/generated_images/", filepath.Join(p.CodexDir, "generated_images")},
		{"codex/attachments/", filepath.Join(p.CodexDir, "attachments")},
		{"codex/codex-remote-attachments/", filepath.Join(p.CodexDir, "codex-remote-attachments")},
	}
}

// addConversationFiles adds the per-conversation folders of the kept
// conversations.
func addConversationFiles(files map[string]localFile, app App, p Paths, keep map[string]bool) {
	for _, pd := range perConversationDirs(app, p) {
		ents, _ := os.ReadDir(pd[1])
		for _, e := range ents {
			if e.IsDir() && keep[e.Name()] {
				addTree(files, filepath.Join(pd[1], e.Name()), pd[0]+e.Name()+"/")
			}
		}
	}
}

// conversationFileTarget maps a per-conversation pack path to this machine.
func conversationFileTarget(app App, p Paths, rel string) (string, bool) {
	for _, pd := range perConversationDirs(app, p) {
		if rest, ok := strings.CutPrefix(rel, pd[0]); ok {
			return filepath.Join(pd[1], filepath.FromSlash(rest)), true
		}
	}
	return "", false
}
