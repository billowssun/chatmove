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
	ids := map[string]bool{}
	for rel, f := range files {
		st.Files++
		st.Bytes += f.size
		if id := sessionID(app, rel); id != "" {
			ids[id] = true
		}
	}
	st.Sessions = len(ids)
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
}

type Result struct {
	Path      string `json:"path"`
	Sessions  int    `json:"sessions"`
	Files     int    `json:"files"`
	Bytes     int64  `json:"bytes"`
	Added     int    `json:"added"`
	Updated   int    `json:"updated"`
	Skipped   int    `json:"skipped"`
	BackupDir string `json:"backupDir"`
	Note      string `json:"note"`
}

func DefaultExportName(app App) string {
	host, _ := os.Hostname()
	label := map[App]string{AppClaude: "Claude", AppCodex: "ChatGPT-Codex"}[app]
	return fmt.Sprintf("%s-聊天记录-%s-%s.zip", label, host, time.Now().Format("20060102-1504"))
}

func Export(app App, p Paths, out string, prog Progress) (*Result, error) {
	cfg := p.config(app)
	files, err := newMapper(cfg).scan()
	if err != nil {
		return nil, err
	}
	if app == AppCodex {
		idx := filepath.Join(p.CodexDir, "session_index.jsonl")
		if info, err := os.Stat(idx); err == nil {
			files["codex/session_index.jsonl"] = localFile{idx, info.Size(), info.ModTime().UnixNano()}
		}
	}
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
	info := packInfo{1, app, cfg.Machine, time.Now(), len(ids), len(keys), total}
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
		src, err := os.Open(lf.abs)
		if err != nil {
			continue // vanished since the scan
		}
		_, err = io.Copy(io.MultiWriter(w, pw), io.LimitReader(src, lf.size))
		src.Close()
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
	return &Result{Path: out, Sessions: len(ids), Files: len(keys), Bytes: st.Size()}, nil
}

// ---- import ----------------------------------------------------------------

var ErrWrongApp = errors.New("wrong app")

// Import merges a pack into this machine's history. Nothing is deleted: new
// files are added, a file is replaced only when the pack's copy is longer
// (sessions are append-only) and the old one is backed up first.
func Import(app App, p Paths, zipPath string, prog Progress) (*Result, error) {
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
		case strings.HasPrefix(f.Name, prefix) && !strings.Contains(f.Name, ".."):
			entries = append(entries, f)
			total += int64(f.UncompressedSize64)
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("%w: 这个压缩包里没有%s的聊天记录", ErrWrongApp, map[App]string{AppClaude: " Claude ", AppCodex: " ChatGPT Codex "}[app])
	}
	if app == AppCodex {
		if procs, err := runningCodexCheck(); err == nil && len(procs) > 0 {
			return nil, ErrCodexRunning
		}
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

	res := &Result{Path: zipPath}
	ids := map[string]bool{}
	backupRoot := filepath.Join(p.root(app), "chatmove-backup", time.Now().Format("20060102-150405"))
	pw := &progressWriter{total: total, fn: prog}
	for _, f := range entries {
		res.Files++
		if id := sessionID(app, f.Name); id != "" {
			ids[id] = true
		}
		dst, ok := m.localPath(f.Name)
		size := int64(f.UncompressedSize64)
		if !ok {
			pw.add(size)
			continue
		}
		lf, exists := local[f.Name]
		if !exists {
			if b := codexBase(f.Name); b != "" && codexLocal[b] {
				// Same rollout already here, archived or compressed differently.
				res.Skipped++
				pw.add(size)
				continue
			}
		}
		if exists {
			same := lf.size == size && fileCRC(lf.abs) == f.CRC32
			active := time.Since(time.Unix(0, lf.mtime)) < 10*time.Minute
			if same || lf.size >= size || active {
				res.Skipped++
				pw.add(size)
				continue
			}
			backup := filepath.Join(backupRoot, filepath.FromSlash(f.Name))
			if err := copyFile(lf.abs, backup); err != nil {
				return nil, err
			}
			res.BackupDir = backupRoot
		}
		if err := extract(f, dst, pw); err != nil {
			return nil, fmt.Errorf("%s: %w", f.Name, err)
		}
		if exists {
			res.Updated++
		} else {
			res.Added++
		}
		res.Bytes += size
	}
	res.Sessions = len(ids)

	if app == AppCodex && index != nil {
		if err := mergeIndexFile(index, filepath.Join(p.CodexDir, "session_index.jsonl")); err != nil {
			return nil, err
		}
	}
	if app == AppCodex && res.Added+res.Updated > 0 {
		if err := CodexReindex(cfg, io.Discard, false); err != nil {
			res.Note = "记录已导入，但没能让 Codex 刷新列表（" + err.Error() + "）。可以安装 Codex 命令行后再导入一次。"
		}
	}
	return res, nil
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

func extract(f *zip.File, dst string, pw *progressWriter) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	tmp := dst + tmpSuffix
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = io.Copy(io.MultiWriter(out, pw), rc) // zip verifies CRC at EOF
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	os.Chtimes(dst, f.Modified, f.Modified)
	return nil
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
