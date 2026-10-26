package core

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// One-click backup/restore of a single app's history as a zip, used by the
// desktop app. The zip layout matches the WebDAV layout (claude/projects/...,
// codex/sessions/...) plus a small chatmove.json describing the pack.

type App string

const (
	AppClaude App = "claude"
	AppCodex  App = "codex"
)

const packInfoName = "chatmove.json"

type Paths struct {
	ClaudeDir string
	CodexDir  string
}

func DefaultPaths() Paths {
	home, _ := os.UserHomeDir()
	p := Paths{filepath.Join(home, ".claude"), filepath.Join(home, ".codex")}
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		p.ClaudeDir = d
	}
	if d := os.Getenv("CODEX_HOME"); d != "" {
		p.CodexDir = d
	}
	return p
}

func (p Paths) config(app App) *Config {
	host, _ := os.Hostname()
	return &Config{
		Machine:    host,
		ClaudeDir:  p.ClaudeDir,
		CodexDir:   p.CodexDir,
		SyncClaude: app == AppClaude,
		SyncCodex:  app == AppCodex,
	}
}

func (p Paths) root(app App) string {
	if app == AppClaude {
		return p.ClaudeDir
	}
	return p.CodexDir
}

// ---- stats -----------------------------------------------------------------

type Stats struct {
	Found    bool   `json:"found"`
	Dir      string `json:"dir"`
	Sessions int    `json:"sessions"`
	Files    int    `json:"files"`
	Bytes    int64  `json:"bytes"`
}

// sessionID returns the conversation a history file belongs to, or "" for
// side files (Claude memory notes, sub-agent folders, titles ...).
func sessionID(app App, rel string) string {
	base := path.Base(rel)
	if !strings.HasPrefix(rel, "claude/projects/") && !strings.HasPrefix(rel, "codex/sessions/") && !strings.HasPrefix(rel, "codex/archived_sessions/") {
		return "" // project files, attachments, metadata
	}
	if app == AppClaude {
		// claude/projects/<project>/<uuid>.jsonl
		if strings.Count(rel, "/") == 3 && strings.HasSuffix(base, ".jsonl") {
			return strings.TrimSuffix(base, ".jsonl")
		}
		return ""
	}
	// rollout-<timestamp>-<thread id>[_<segment id>].jsonl[.zst]; a thread may
	// be split across several segment files.
	name := strings.TrimSuffix(strings.TrimSuffix(base, ".zst"), ".jsonl")
	if !strings.HasPrefix(name, "rollout-") || len(name) < len("rollout-2006-01-02T15-04-05-")+36 {
		return ""
	}
	return name[len("rollout-2006-01-02T15-04-05-"):][:36]
}

func GetStats(app App, p Paths) Stats {
	st := Stats{Dir: p.root(app)}
	if info, err := os.Stat(st.Dir); err != nil || !info.IsDir() {
		return st
	}
	st.Found = true
	files, err := newMapper(p.config(app)).scan()
	if err != nil {
		return st
	}
	// Count what the app's own list shows, not files of removed conversations.
	listed := -1
	if app == AppClaude {
		dropRemoved(files, loadClaudeApp())
	} else if vis, ok := codexSidebar(p.CodexDir); ok {
		for rel := range files {
			if id := sessionID(app, rel); id != "" && !vis.keep[id] {
				delete(files, rel)
			}
		}
		listed = len(vis.places) // top-level threads, without sub-agents
	}
	ids := map[string]bool{}
	for rel, f := range files {
		st.Files++
		st.Bytes += f.size
		if id := sessionID(app, rel); id != "" {
			ids[id] = true
		}
	}
	st.Sessions = len(ids)
	if listed >= 0 {
		st.Sessions = listed
	}
	return st
}

// ---- progress --------------------------------------------------------------

type Progress func(done, total int64)

type progressWriter struct {
	done, total int64
	fn          Progress
	last        time.Time
}

func (w *progressWriter) add(n int64) {
	w.done += n
	if w.fn != nil && (time.Since(w.last) > 80*time.Millisecond || w.done >= w.total) {
		w.last = time.Now()
		w.fn(w.done, w.total)
	}
}

func (w *progressWriter) Write(b []byte) (int, error) {
	w.add(int64(len(b)))
	return len(b), nil
}

// ---- export ----------------------------------------------------------------

type packInfo struct {
	Version  int       `json:"version"`
	App      App       `json:"app"`
	Machine  string    `json:"machine"`
	Created  time.Time `json:"created"`
	Sessions int       `json:"sessions"`
	Files    int       `json:"files"`
	Bytes    int64     `json:"bytes"`
	// Since version 2: the exporting user's home folder, and for Claude the
	// working folder behind each project directory, so imports can map paths.
	Home    string            `json:"home,omitempty"`
	Cwds    map[string]string `json:"cwds,omitempty"`
	Removed int               `json:"removed,omitempty"`
	// Codex: where each exported thread sat in the sidebar.
	CodexPlaces map[string]codexPlace `json:"codexPlaces,omitempty"`
	// Working folders packed with the conversations, and ones left out.
	Workspaces     []string `json:"workspaces,omitempty"`
	SkippedFolders []string `json:"skippedFolders,omitempty"`
}

type Result struct {
	Path           string   `json:"path"`
	Sessions       int      `json:"sessions"`
	Files          int      `json:"files"`
	Bytes          int64    `json:"bytes"`
	Added          int      `json:"added"`
	Updated        int      `json:"updated"`
	Skipped        int      `json:"skipped"`
	Removed        int      `json:"removed"` // conversations left out because they were removed from the app's list
	CreatedDirs    []string `json:"createdDirs"`
	Folders        int      `json:"folders"`        // project folders packed or restored
	FolderFiles    int      `json:"folderFiles"`    // files restored into them
	Kept           int      `json:"kept"`           // files that already differed here, left alone
	SkippedFolders []string `json:"skippedFolders"` // working folders too broad to pack
	Outputs        int      `json:"outputs"`        // files written outside the working folders
	OutputBytes    int64    `json:"outputBytes"`
	Target         string   `json:"target"` // folder the files were put in
	BackupDir      string   `json:"backupDir"`
	Note           string   `json:"note"`
}

func DefaultExportName(app App) string {
	host, _ := os.Hostname()
	label := map[App]string{AppClaude: "Claude", AppCodex: "ChatGPT-Codex"}[app]
	return fmt.Sprintf("%s-聊天记录-%s-%s.zip", label, host, time.Now().Format("20060102-1504"))
}

// dropRemoved takes conversations the user removed from the Claude app out
// of a file listing and returns how many there were.
func dropRemoved(files map[string]localFile, st claudeAppState) int {
	gone := map[string]bool{}
	for rel := range files {
		if sid := claudeSessionOf(rel); sid != "" && st.removed(sid) {
			gone[sid] = true
			delete(files, rel)
		}
	}
	return len(gone)
}

// claudeProjectCwds works out the working folder behind each project
// directory, preferring a path that encodes back to the directory's name.
func claudeProjectCwds(files map[string]localFile, st claudeAppState) map[string]string {
	cands := map[string][]string{}
	for rel, f := range files {
		proj, rest, ok := claudeSplit(rel)
		if !ok || strings.Contains(rest, "/") || !strings.HasSuffix(rest, ".jsonl") {
			continue
		}
		if m, ok := st.live[strings.TrimSuffix(rest, ".jsonl")]; ok && m.Cwd != "" {
			cands[proj] = append(cands[proj], m.Cwd)
		}
		if src, err := os.Open(f.abs); err == nil {
			if c := firstCwd(src); c != "" {
				cands[proj] = append(cands[proj], c)
			}
			src.Close()
		}
	}
	return pickCwds(cands)
}

func pickCwds(cands map[string][]string) map[string]string {
	out := map[string]string{}
	for proj, list := range cands {
		for _, c := range list {
			if claudeProjectName(strings.TrimPrefix(c, `\\?\`)) == proj {
				out[proj] = c
				break
			}
		}
		if out[proj] == "" && len(list) > 0 {
			out[proj] = list[0]
		}
	}
	return out
}

func Export(app App, p Paths, out string, prog Progress) (*Result, error) {
	cfg := p.config(app)
	files, err := newMapper(cfg).scan()
	if err != nil {
		return nil, err
	}
	home, _ := os.UserHomeDir()
	info := packInfo{Version: 2, App: app, Machine: cfg.Machine, Created: time.Now(), Home: home}
	inline := map[string][]byte{}  // generated entries
	keepIDs := map[string]bool{}   // conversations that go into the pack
	sessCwd := map[string]string{} // and the folder each one worked in
	var cwds []string              // and the folders they worked in
	if app == AppClaude {
		st := loadClaudeApp()
		info.Removed = dropRemoved(files, st)
		info.Cwds = claudeProjectCwds(files, st)
		// The desktop app lists a conversation only through its metadata file.
		for rel := range files {
			proj, rest, _ := claudeSplit(rel)
			if proj == "" || strings.Contains(rest, "/") || !strings.HasSuffix(rest, ".jsonl") {
				continue
			}
			if m, ok := st.live[strings.TrimSuffix(rest, ".jsonl")]; ok && m.File != "" {
				if b, err := os.ReadFile(m.File); err == nil {
					inline[claudeMetaPrefix+m.Rel+"/"+filepath.Base(m.File)] = b
				}
			}
		}
		for rel := range files {
			if sid := sessionID(app, rel); sid != "" {
				keepIDs[sid] = true
				if m, ok := st.live[sid]; ok && m.Cwd != "" {
					cwds = append(cwds, m.Cwd)
					sessCwd[sid] = m.Cwd
				}
			}
		}
		for _, c := range info.Cwds {
			cwds = append(cwds, c)
		}
	}
	if app == AppCodex {
		vis, ok := codexSidebar(p.CodexDir)
		if ok {
			for rel := range files {
				if id := sessionID(app, rel); id != "" && !vis.keep[id] {
					delete(files, rel)
				}
			}
			info.Removed = vis.hidden
			info.CodexPlaces = vis.places
			for id := range vis.keep {
				keepIDs[id] = true
				cwds = append(cwds, vis.cwd[id])
				sessCwd[id] = vis.cwd[id]
			}
		}
		idx := filepath.Join(p.CodexDir, "session_index.jsonl")
		if b, err := os.ReadFile(idx); err == nil {
			if ok {
				b = filterIndex(b, vis.keep)
			}
			inline["codex/session_index.jsonl"] = b
		}
	}
	for rel, b := range inline {
		files[rel] = localFile{size: int64(len(b)), mtime: time.Now().UnixNano()}
	}
	// What the conversations worked on, generated and attached travels too.
	ws := collectWorkspaces(cwds, p.ClaudeDir, p.CodexDir)
	for _, f := range ws.folders {
		if key, ok := workspaceKey(f); ok {
			addTree(files, f, key+"/")
			info.Workspaces = append(info.Workspaces, f)
		}
	}
	info.SkippedFolders = ws.skipped
	addConversationFiles(files, app, p, keepIDs)
	// Files the conversations wrote elsewhere: transcript -> its working folder.
	transcripts := map[string]string{}
	for rel, f := range files {
		if id := sessionID(app, rel); id != "" {
			cwd := sessCwd[id]
			if proj, _, ok := claudeSplit(rel); ok && cwd == "" {
				cwd = info.Cwds[proj]
			}
			transcripts[f.abs] = cwd
		}
	}
	outCount, outBytes := addOutputs(files, app, transcripts, ws.folders, p.ClaudeDir, p.CodexDir)
	if len(files) == 0 {
		return nil, fmt.Errorf("没有找到聊天记录（%s）", p.root(app))
	}
	keys := make([]string, 0, len(files))
	ids := map[string]bool{}
	var total int64
	for k, f := range files {
		keys = append(keys, k)
		total += f.size
		if id := sessionID(app, k); id != "" {
			ids[id] = true
		}
	}
	sort.Strings(keys)
	info.Sessions, info.Files, info.Bytes = len(ids), len(keys), total

	tmp := out + tmpSuffix
	f, err := os.Create(tmp)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Result, error) {
		f.Close()
		os.Remove(tmp)
		return nil, err
	}
	zw := zip.NewWriter(f)
	if w, err := zw.Create(packInfoName); err != nil {
		return fail(err)
	} else {
		json.NewEncoder(w).Encode(info)
	}
	pw := &progressWriter{total: total, fn: prog}
	for _, rel := range keys {
		lf := files[rel]
		method := zip.Deflate
		if strings.HasSuffix(rel, ".zst") {
			method = zip.Store
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: rel, Method: method, Modified: time.Unix(0, lf.mtime)})
		if err != nil {
			return fail(err)
		}
		if b, ok := inline[rel]; ok {
			_, err = io.MultiWriter(w, pw).Write(b)
		} else {
			src, oerr := os.Open(lf.abs)
			if oerr != nil {
				continue // vanished since the scan
			}
			_, err = io.Copy(io.MultiWriter(w, pw), io.LimitReader(src, lf.size))
			src.Close()
		}
		if err != nil {
			return fail(fmt.Errorf("%s: %w", rel, err))
		}
	}
	if err := zw.Close(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmp, out); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	st, _ := os.Stat(out)
	sessions := len(ids)
	if len(info.CodexPlaces) > 0 {
		sessions = len(info.CodexPlaces) // top-level threads, without sub-agents
	}
	return &Result{Path: out, Sessions: sessions, Files: len(keys), Bytes: st.Size(), Removed: info.Removed,
		Folders: len(info.Workspaces), SkippedFolders: info.SkippedFolders, Outputs: outCount, OutputBytes: outBytes}, nil
}

// ---- import ----------------------------------------------------------------

var ErrWrongApp = errors.New("wrong app")

func readPackInfo(files []*zip.File) packInfo {
	var info packInfo
	for _, f := range files {
		if f.Name == packInfoName {
			if rc, err := f.Open(); err == nil {
				json.NewDecoder(rc).Decode(&info)
				rc.Close()
			}
		}
	}
	return info
}

// entryCwd reads the first working folder recorded in a transcript entry.
func entryCwd(f *zip.File) string {
	rc, err := f.Open()
	if err != nil {
		return ""
	}
	defer rc.Close()
	r, done, err := openRollout(f.Name, rc)
	if err != nil {
		return ""
	}
	defer done()
	return firstCwd(r)
}

// importPlan says where each pack entry lands and how its paths change.
type importPlan struct {
	target func(name string) (rel string, rw *lineRewriter)
	skip   func(name string) bool // conversations removed on this machine
	metas  []claudeMetaImport     // desktop-app list entries to add
}

func planClaude(entries []*zip.File, info packInfo, target string) (importPlan, *relocator) {
	cwds := info.Cwds
	if len(cwds) == 0 { // version 1 pack: read the transcripts
		cands := map[string][]string{}
		for _, f := range entries {
			proj, rest, ok := claudeSplit(f.Name)
			if ok && !strings.Contains(rest, "/") && strings.HasSuffix(rest, ".jsonl") {
				if c := entryCwd(f); c != "" {
					cands[proj] = append(cands[proj], c)
				}
			}
		}
		cwds = pickCwds(cands)
	}
	// Every home folder the history mentions, including ones left by an
	// earlier move, is mapped to this machine's.
	homes := []string{info.Home}
	txCwd := map[string]string{} // first cwd recorded in each transcript
	projOf := map[string]string{}
	for _, f := range entries {
		if proj, rest, ok := claudeSplit(f.Name); ok && strings.HasSuffix(rest, ".jsonl") && !strings.Contains(rest, "/") {
			sid := strings.TrimSuffix(rest, ".jsonl")
			c := entryCwd(f)
			txCwd[sid], projOf[sid] = c, proj
			homes = append(homes, guessHome(c))
		}
	}
	for _, c := range cwds {
		homes = append(homes, guessHome(c))
	}
	type meta struct {
		rel, sid, cwd string
		obj           map[string]any
	}
	var raw []meta
	for _, f := range entries {
		rest, ok := strings.CutPrefix(f.Name, claudeMetaPrefix)
		if !ok {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		obj := map[string]any{}
		err = json.NewDecoder(rc).Decode(&obj)
		rc.Close()
		if sid, c := str(obj["cliSessionId"]), str(obj["cwd"]); err == nil && sid != "" && c != "" {
			raw = append(raw, meta{path.Dir(rest), sid, c, obj})
			homes = append(homes, guessHome(c))
		}
	}

	r := newRelocator(target, newPathMover(homes...), info.Home)
	for _, w := range info.Workspaces { // packed folders first: they own their names
		r.place(w)
	}
	// A conversation follows the folder in its desktop-app metadata, which can
	// differ from the project directory its transcript was saved in.
	sessTo, sessNew := map[string]string{}, map[string]string{}
	var metas []claudeMetaImport
	for _, m := range raw {
		nc := r.place(m.cwd)
		sessTo[m.sid], sessNew[m.sid] = claudeProjectName(nc), nc
		metas = append(metas, claudeMetaImport{rel: m.rel, obj: m.obj, sid: m.sid, cwd: nc})
	}
	projTo := map[string]string{}
	for proj, cwd := range cwds {
		projTo[proj] = claudeProjectName(r.place(cwd))
	}
	for sid, c := range txCwd {
		if sessNew[sid] == "" { // listed conversations already have their folder
			r.place(c)
		}
	}
	rw := r.rewriter() // every moved folder, wherever a transcript mentions it
	// A listed conversation keeps one folder: whatever its transcript recorded
	// as the working folder now means the folder from its metadata.
	sessRw := map[string]*lineRewriter{}
	for sid, nc := range sessNew {
		sessRw[sid] = rw.with([2]string{txCwd[sid], nc}, [2]string{cwds[projOf[sid]], nc})
	}

	local := loadClaudeApp()
	return importPlan{
		metas: metas,
		target: func(name string) (string, *lineRewriter) {
			proj, rest, ok := claudeSplit(name)
			to, mapped := projTo[proj]
			lr := rw
			if sid := claudeSessionOf(name); sessTo[sid] != "" {
				to, mapped, lr = sessTo[sid], true, sessRw[sid]
			}
			if !ok || !mapped {
				return name, nil
			}
			if !strings.HasSuffix(rest, ".jsonl") || lr.empty() {
				return "claude/projects/" + to + "/" + rest, nil
			}
			return "claude/projects/" + to + "/" + rest, lr
		},
		skip: func(name string) bool {
			sid := claudeSessionOf(name)
			return sid != "" && local.deleted[sid]
		},
	}, r
}

func planCodex(entries []*zip.File, info packInfo, target string) (importPlan, *relocator) {
	homes := []string{info.Home}
	var cwds []string
	for _, f := range entries {
		if strings.Contains(f.Name, "/rollout-") {
			c := entryCwd(f)
			cwds = append(cwds, c)
			homes = append(homes, guessHome(c))
		}
	}
	for _, pl := range info.CodexPlaces {
		homes = append(homes, guessHome(pl.Root), guessHome(pl.Hint))
	}
	r := newRelocator(target, newPathMover(homes...), info.Home)
	for _, w := range info.Workspaces {
		r.place(w)
	}
	for _, c := range cwds {
		r.place(c)
	}
	for _, pl := range info.CodexPlaces {
		r.place(pl.Root)
	}
	rw := r.rewriter()
	return importPlan{
		target: func(name string) (string, *lineRewriter) {
			if !rw.empty() && strings.Contains(name, "/rollout-") {
				return name, rw
			}
			return name, nil
		},
		skip: func(string) bool { return false },
	}, r
}

// metaCwd reads the working folder from a packed desktop-app metadata file.
func metaCwd(f *zip.File) string {
	rc, err := f.Open()
	if err != nil {
		return ""
	}
	defer rc.Close()
	var m struct {
		Cwd string `json:"cwd"`
	}
	json.NewDecoder(rc).Decode(&m)
	return m.Cwd
}

// Import merges a pack into this machine. Conversations go back into the
// apps' own folders; everything they worked with goes into target, the
// folder the user picked, and the apps are pointed there. Nothing on this
// machine is overwritten: transcripts are only replaced by a longer copy
// (they are append-only) after a backup, and any other file that already
// exists with different content is saved next to it under a new name.
func Import(app App, p Paths, zipPath, target string, prog Progress) (*Result, error) {
	if target == "" {
		return nil, fmt.Errorf("没有选择存放项目文件的文件夹")
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("无法打开压缩包：%w", err)
	}
	defer zr.Close()

	var entries []*zip.File
	var index *zip.File
	var total int64
	prefix := string(app) + "/"
	for _, f := range zr.File {
		switch {
		case f.FileInfo().IsDir() || f.Name == packInfoName:
		case f.Name == "codex/session_index.jsonl":
			index = f
		case (strings.HasPrefix(f.Name, prefix) || strings.HasPrefix(f.Name, workspacePrefix) || strings.HasPrefix(f.Name, outputPrefix)) && !strings.Contains(f.Name, ".."):
			entries = append(entries, f)
			total += int64(f.UncompressedSize64)
		}
	}
	if len(entries) == 0 || !hasPrefixAny(entries, prefix) {
		return nil, fmt.Errorf("%w: 这个压缩包里没有%s的聊天记录", ErrWrongApp, map[App]string{AppClaude: " Claude ", AppCodex: " ChatGPT Codex "}[app])
	}
	if app == AppCodex {
		if procs, err := runningCodexCheck(); err == nil && len(procs) > 0 {
			return nil, ErrCodexRunning
		}
	}
	if err := os.MkdirAll(target, 0755); err != nil {
		return nil, fmt.Errorf("无法创建文件夹 %s：%w", target, err)
	}

	info := readPackInfo(zr.File)
	var plan importPlan
	var r *relocator
	if app == AppClaude {
		plan, r = planClaude(entries, info, target)
	} else {
		plan, r = planCodex(entries, info, target)
	}

	cfg := p.config(app)
	m := newMapper(cfg)
	local, err := m.scan()
	if err != nil {
		return nil, err
	}
	codexLocal := map[string]bool{}
	for k := range local {
		if b := codexBase(k); b != "" {
			codexLocal[b] = true
		}
	}

	res := &Result{Path: zipPath, Target: target}
	ids, removed := map[string]bool{}, map[string]bool{}
	backupRoot := filepath.Join(p.root(app), "chatmove-backup", time.Now().Format("20060102-150405"))
	pw := &progressWriter{total: total, fn: prog}
	for _, f := range entries {
		size := int64(f.UncompressedSize64)
		if strings.HasPrefix(f.Name, claudeMetaPrefix) {
			pw.add(size)
			continue
		}
		if dst, ok := extraTarget(app, p, r, info.Home, f.Name); ok {
			res.Files++
			if err := restoreExtra(f, dst, pw, res); err != nil {
				return nil, fmt.Errorf("%s: %w", f.Name, err)
			}
			continue
		}
		if plan.skip(f.Name) {
			removed[claudeSessionOf(f.Name)] = true
			pw.add(size)
			continue
		}
		res.Files++
		if id := sessionID(app, f.Name); id != "" {
			ids[id] = true
		}
		rel, rw := plan.target(f.Name)
		dst, ok := m.localPath(rel)
		if !ok {
			pw.add(size)
			continue
		}
		lf, exists := local[rel]
		if !exists {
			if b := codexBase(rel); b != "" && codexLocal[b] {
				// Same rollout already here, archived or compressed differently.
				res.Skipped++
				pw.add(size)
				continue
			}
		}
		if exists && (time.Since(time.Unix(0, lf.mtime)) < 10*time.Minute || (rw != nil && strings.HasSuffix(rel, ".zst"))) {
			// In use, or a recompressed rollout whose size says nothing.
			res.Skipped++
			pw.add(size)
			continue
		}
		tmp, n, err := extractTo(f, dst, rw, pw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Name, err)
		}
		if exists {
			if lf.size >= n || (lf.size == n && fileCRC(lf.abs) == fileCRC(tmp)) {
				os.Remove(tmp)
				res.Skipped++
				continue
			}
			backup := filepath.Join(backupRoot, filepath.FromSlash(rel))
			if err := copyFile(lf.abs, backup); err != nil {
				os.Remove(tmp)
				return nil, err
			}
			res.BackupDir = backupRoot
		}
		if err := os.Rename(tmp, dst); err != nil {
			os.Remove(tmp)
			return nil, err
		}
		os.Chtimes(dst, f.Modified, f.Modified)
		if exists {
			res.Updated++
		} else {
			res.Added++
		}
		res.Bytes += n
	}
	res.Sessions = len(ids)
	res.Removed = len(removed)

	if app == AppCodex {
		if index != nil {
			if err := mergeIndexFile(index, filepath.Join(p.CodexDir, "session_index.jsonl")); err != nil {
				return nil, err
			}
		}
		if res.Added+res.Updated > 0 {
			if err := CodexReindex(cfg, io.Discard, false); err != nil {
				res.Note = "记录已导入，但没能让 Codex 刷新列表（" + err.Error() + "）。"
			} else if len(info.CodexPlaces) > 0 {
				if _, err := registerCodexSidebar(p.CodexDir, info.CodexPlaces, r.place, backupRoot); err != nil {
					res.Note = "记录已导入，但没能登记到 Codex 侧边栏（" + err.Error() + "）。"
				} else {
					res.BackupDir = backupRoot
				}
			}
		}
	}
	if app == AppClaude {
		n, err := writeClaudeMeta(plan.metas, loadClaudeApp())
		switch {
		case err != nil:
			res.Note = "记录已导入，但没能写入 Claude 的会话列表（" + err.Error() + "）。"
		case n > 0 || res.Added+res.Updated > 0:
			res.Note = "完全退出 Claude（包括右下角托盘图标）再重新打开，导入的对话就会出现在列表里。"
		}
	}
	if err := r.save(); err != nil {
		return nil, err
	}
	res.Folders = len(info.Workspaces)
	res.CreatedDirs = r.created
	sort.Strings(res.CreatedDirs)
	return res, nil
}

func hasPrefixAny(files []*zip.File, prefix string) bool {
	for _, f := range files {
		if strings.HasPrefix(f.Name, prefix) {
			return true
		}
	}
	return false
}

func fileCRC(p string) uint32 {
	f, err := os.Open(p)
	if err != nil {
		return 0
	}
	defer f.Close()
	h := crc32.NewIEEE()
	io.Copy(h, f)
	return h.Sum32()
}

// extractTo writes an entry next to dst as a temporary file, rewriting paths
// when rw is set, and returns the temp path and its size.
func extractTo(f *zip.File, dst string, rw *lineRewriter, pw *progressWriter) (string, int64, error) {
	rc, err := f.Open()
	if err != nil {
		return "", 0, err
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return "", 0, err
	}
	tmp := dst + tmpSuffix
	out, err := os.Create(tmp)
	if err != nil {
		return "", 0, err
	}
	src := io.TeeReader(rc, pw) // zip verifies CRC at EOF
	switch {
	case rw == nil:
		_, err = io.Copy(out, src)
	case strings.HasSuffix(dst, ".zst"):
		err = rw.copyZst(out, src)
	default:
		err = rw.copy(out, src)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return "", 0, err
	}
	info, err := os.Stat(tmp)
	if err != nil {
		return "", 0, err
	}
	return tmp, info.Size(), nil
}

func mergeIndexFile(f *zip.File, localPath string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	theirs, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return err
	}
	mine, err := os.ReadFile(localPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	merged := mergeIndex(mine, theirs)
	if string(merged) == string(mine) {
		return nil
	}
	return writeAtomic(localPath, merged)
}

// filterIndex keeps the title index lines of the given threads.
func filterIndex(b []byte, keep map[string]bool) []byte {
	var out []byte
	for _, l := range strings.SplitAfter(string(b), "\n") {
		var v struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(l)), &v) == nil && keep[v.ID] {
			out = append(out, l...)
		}
	}
	return out
}

// extraTarget places project files, other output files and per-conversation
// files.
func extraTarget(app App, p Paths, r *relocator, oldHome, name string) (string, bool) {
	switch {
	case strings.HasPrefix(name, workspacePrefix):
		return r.place(workspaceOrigin(name, oldHome)), true
	case strings.HasPrefix(name, outputPrefix):
		return r.outputTarget(name), true
	}
	return conversationFileTarget(app, p, name)
}

// restoreExtra writes a project, output or conversation file. When a file
// with different content is already there, the incoming copy is saved next
// to it as "name (导入).ext" so both versions are kept.
func restoreExtra(f *zip.File, dst string, pw *progressWriter, res *Result) error {
	size := int64(f.UncompressedSize64)
	if info, err := os.Stat(dst); err == nil {
		if info.Size() == size && fileCRC(dst) == f.CRC32 {
			res.Skipped++
			pw.add(size)
			return nil
		}
		ext := filepath.Ext(dst)
		base := strings.TrimSuffix(dst, ext)
		for i := 1; ; i++ {
			alt := base + " (导入)" + ext
			if i > 1 {
				alt = fmt.Sprintf("%s (导入 %d)%s", base, i, ext)
			}
			if st, err := os.Stat(alt); err != nil {
				dst = alt
				break
			} else if st.Size() == size && fileCRC(alt) == f.CRC32 {
				res.Skipped++
				pw.add(size)
				return nil
			}
		}
		res.Kept++
	}
	tmp, n, err := extractTo(f, dst, nil, pw)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	os.Chtimes(dst, f.Modified, f.Modified)
	res.Added++
	res.FolderFiles++
	res.Bytes += n
	return nil
}

// PackSummary describes a pack before importing it.
type PackSummary struct {
	App      App       `json:"app"`
	Machine  string    `json:"machine"`
	Created  time.Time `json:"created"`
	Sessions int       `json:"sessions"`
	Folders  int       `json:"folders"`
	Outputs  int       `json:"outputs"`
	Bytes    int64     `json:"bytes"`
}

// ReadPack reads a pack's summary without importing anything.
func ReadPack(zipPath string) (*PackSummary, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("无法打开压缩包：%w", err)
	}
	defer zr.Close()
	info := readPackInfo(zr.File)
	s := &PackSummary{App: info.App, Machine: info.Machine, Created: info.Created, Sessions: info.Sessions, Folders: len(info.Workspaces)}
	if len(info.CodexPlaces) > 0 {
		s.Sessions = len(info.CodexPlaces)
	}
	for _, f := range zr.File {
		s.Bytes += int64(f.UncompressedSize64)
		if strings.HasPrefix(f.Name, outputPrefix) {
			s.Outputs++
		}
		if s.App == "" && strings.HasPrefix(f.Name, "claude/") {
			s.App = AppClaude
		} else if s.App == "" && strings.HasPrefix(f.Name, "codex/") {
			s.App = AppCodex
		}
	}
	return s, nil
}

// SuggestTarget proposes a new folder for imported files: on D: when this
// machine has one, otherwise in the home folder, never one already in use.
func SuggestTarget(app App) string {
	name := map[App]string{AppClaude: "Claude 对话迁移", AppCodex: "ChatGPT 对话迁移"}[app]
	root, _ := os.UserHomeDir()
	if dirExists(`D:\`) {
		root = `D:\`
	}
	p := filepath.Join(root, name)
	for i := 2; nonEmptyDir(p) && !dirExists(filepath.Join(p, relocateMapName)) && !fileExists(filepath.Join(p, relocateMapName)); i++ {
		p = filepath.Join(root, fmt.Sprintf("%s (%d)", name, i))
	}
	return p
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
