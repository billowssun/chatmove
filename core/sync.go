package core

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const tmpSuffix = ".chatsync-tmp"

// ---- local layout ----------------------------------------------------------

type source struct {
	prefix string // remote prefix, e.g. "claude/projects"
	dir    string // local directory
	claude bool   // first path segment is a Claude project folder name
}

type localFile struct {
	abs   string
	size  int64
	mtime int64
}

type mapper struct {
	srcs              []source
	toLocal, toRemote map[string]string
}

func newMapper(cfg *Config) *mapper {
	m := &mapper{}
	m.toLocal, m.toRemote = cfg.projectMaps()
	if cfg.SyncClaude {
		m.srcs = append(m.srcs, source{"claude/projects", filepath.Join(cfg.ClaudeDir, "projects"), true})
	}
	if cfg.SyncCodex {
		m.srcs = append(m.srcs,
			source{"codex/sessions", filepath.Join(cfg.CodexDir, "sessions"), false},
			source{"codex/archived_sessions", filepath.Join(cfg.CodexDir, "archived_sessions"), false})
	}
	return m
}

func mapFirst(rel string, m map[string]string) string {
	first, rest, ok := strings.Cut(rel, "/")
	if to, hit := m[first]; hit {
		first = to
	}
	if !ok {
		return first
	}
	return first + "/" + rest
}

func (m *mapper) localPath(rel string) (string, bool) {
	for _, s := range m.srcs {
		rest, ok := strings.CutPrefix(rel, s.prefix+"/")
		if !ok {
			continue
		}
		if s.claude {
			rest = mapFirst(rest, m.toLocal)
		}
		return filepath.Join(s.dir, filepath.FromSlash(rest)), true
	}
	return "", false
}

// scan lists local files keyed by their remote relative path.
func (m *mapper) scan() (map[string]localFile, error) {
	out := map[string]localFile{}
	for _, s := range m.srcs {
		err := filepath.WalkDir(s.dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.IsDir() || strings.HasSuffix(p, tmpSuffix) {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			r, _ := filepath.Rel(s.dir, p)
			r = filepath.ToSlash(r)
			if s.claude {
				r = mapFirst(r, m.toRemote)
			}
			out[s.prefix+"/"+r] = localFile{p, info.Size(), info.ModTime().UnixNano()}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// codexBase identifies a Codex rollout regardless of whether it sits in
// sessions/ or archived_sessions/ and whether Codex has zstd-compressed it.
func codexBase(rel string) string {
	if !strings.HasPrefix(rel, "codex/") {
		return ""
	}
	return strings.TrimSuffix(path.Base(rel), ".zst")
}

// ---- state & manifest ------------------------------------------------------

type fileState struct {
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
	Hash  string `json:"hash"` // hash of the local file at Size/MTime
	Base  string `json:"base"` // hash both sides agreed on at last sync
}

type State struct {
	Files map[string]*fileState `json:"files"`
	// Set when Codex rollouts or the title index were pulled and Codex's
	// own DB has not been refreshed yet.
	CodexReindexPending bool `json:"codex_reindex_pending"`
}

type Entry struct {
	Size    int64  `json:"size"`
	MTime   int64  `json:"mtime"`
	Hash    string `json:"hash"`
	Machine string `json:"machine"`
}

type Manifest struct {
	Version int              `json:"version"`
	Updated time.Time        `json:"updated"`
	Files   map[string]Entry `json:"files"`
}

func LoadState(p string) *State {
	st := &State{Files: map[string]*fileState{}}
	if b, err := os.ReadFile(p); err == nil {
		json.Unmarshal(b, st)
	}
	if st.Files == nil {
		st.Files = map[string]*fileState{}
	}
	return st
}

func (st *State) Save(p string) error {
	b, _ := json.Marshal(st)
	return writeAtomic(p, b)
}

func (st *State) get(rel string) *fileState {
	f := st.Files[rel]
	if f == nil {
		f = &fileState{}
		st.Files[rel] = f
	}
	return f
}

func writeAtomic(p string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	tmp := p + tmpSuffix
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---- syncer ----------------------------------------------------------------

type Mode int

const (
	ModePush Mode = 1 << iota
	ModePull
	ModeSync = ModePush | ModePull
)

type syncer struct {
	cfg    *Config
	dav    *DAV
	m      *mapper
	st     *State
	man    *Manifest
	pushed map[string]Entry
	dry    bool
	stamp  string
	log    io.Writer

	nPush, nPull, nSkip, nConflict, nErr int
	bytesUp, bytesDown                   int64
}

func (s *syncer) remote(rel string) string  { return s.cfg.RemoteDir + "/files/" + rel }
func (s *syncer) manifestPath() string      { return s.cfg.RemoteDir + "/manifest.json" }
func (s *syncer) printf(f string, a ...any) { fmt.Fprintf(s.log, f+"\n", a...) }

func (s *syncer) fetchManifest() (*Manifest, error) {
	man := &Manifest{Version: 1, Files: map[string]Entry{}}
	rc, err := s.dav.Get(s.manifestPath())
	if errors.Is(err, errNotFound) {
		return man, nil
	}
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	if err := json.NewDecoder(rc).Decode(man); err != nil {
		return nil, fmt.Errorf("remote manifest is corrupt: %w", err)
	}
	if man.Files == nil {
		man.Files = map[string]Entry{}
	}
	return man, nil
}

// saveManifest re-reads the remote manifest and applies only what this run
// pushed, so a concurrent sync from another machine is not clobbered.
func (s *syncer) saveManifest() error {
	if len(s.pushed) == 0 {
		return nil
	}
	man, err := s.fetchManifest()
	if err != nil {
		return err
	}
	for k, v := range s.pushed {
		man.Files[k] = v
	}
	man.Updated = time.Now().UTC()
	b, _ := json.MarshalIndent(man, "", " ")
	if err := s.dav.Put(s.manifestPath(), bytes.NewReader(b), int64(len(b))); err != nil {
		return err
	}
	s.man = man
	s.pushed = map[string]Entry{}
	return nil
}

func (s *syncer) localHash(rel string, lf localFile) (string, error) {
	fst := s.st.get(rel)
	if fst.Hash != "" && fst.Size == lf.size && fst.MTime == lf.mtime {
		return fst.Hash, nil
	}
	h, err := hashFile(lf.abs)
	if err != nil {
		return "", err
	}
	fst.Size, fst.MTime, fst.Hash = lf.size, lf.mtime, h
	return h, nil
}

func (s *syncer) push(rel string, lf localFile) error {
	s.printf("  up    %s (%s)", rel, Human(lf.size))
	if s.dry {
		s.nPush++
		return nil
	}
	f, err := os.Open(lf.abs)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	// Cap at the scanned size: an active session may keep appending while we
	// upload, and the tail will go up on the next run.
	r := io.TeeReader(io.LimitReader(f, lf.size), h)
	if err := s.dav.Put(s.remote(rel), r, lf.size); err != nil {
		return err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	e := Entry{Size: lf.size, MTime: lf.mtime, Hash: sum, Machine: s.cfg.Machine}
	s.pushed[rel] = e
	s.man.Files[rel] = e
	fst := s.st.get(rel)
	fst.Base = sum
	if info, err := os.Stat(lf.abs); err == nil && info.Size() == lf.size && info.ModTime().UnixNano() == lf.mtime {
		fst.Size, fst.MTime, fst.Hash = lf.size, lf.mtime, sum
	} else {
		fst.Hash = ""
	}
	s.nPush++
	s.bytesUp += lf.size
	if len(s.pushed) >= 25 {
		return s.saveManifest()
	}
	return nil
}

func (s *syncer) download(rel, dst string, want string) error {
	rc, err := s.dav.Get(s.remote(rel))
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	tmp := dst + tmpSuffix
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), rc)
	f.Close()
	if err == nil && want != "" && hex.EncodeToString(h.Sum(nil)) != want {
		err = fmt.Errorf("hash mismatch after download (remote changed mid-sync?)")
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	s.bytesDown += n
	return os.Rename(tmp, dst)
}

func (s *syncer) pull(rel string, e Entry) error {
	dst, _ := s.m.localPath(rel)
	s.printf("  down  %s (%s, from %s)", rel, Human(e.Size), e.Machine)
	if s.dry {
		s.nPull++
		return nil
	}
	if err := s.download(rel, dst, e.Hash); err != nil {
		return err
	}
	mt := time.Unix(0, e.MTime)
	os.Chtimes(dst, mt, mt)
	fst := s.st.get(rel)
	if info, err := os.Stat(dst); err == nil {
		fst.Size, fst.MTime, fst.Hash = info.Size(), info.ModTime().UnixNano(), e.Hash
	}
	fst.Base = e.Hash
	if strings.HasPrefix(rel, "codex/") {
		s.st.CodexReindexPending = true
	}
	s.nPull++
	return nil
}

func (s *syncer) conflictDir() string {
	return filepath.Join(s.cfg.Dir(), "conflicts", s.stamp)
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// resolveConflict keeps the larger copy (sessions are append-only, so the
// longer one usually contains the other) and saves the loser under conflicts/.
func (s *syncer) resolveConflict(rel string, lf localFile, e Entry, mode Mode) error {
	s.nConflict++
	localWins := lf.size >= e.Size
	s.printf("  CONFLICT %s: local %s vs remote %s (%s) -> keep %s",
		rel, Human(lf.size), Human(e.Size), e.Machine, map[bool]string{true: "local", false: "remote"}[localWins])
	if s.dry {
		return nil
	}
	backup := filepath.Join(s.conflictDir(), filepath.FromSlash(rel))
	if localWins {
		if mode&ModePush == 0 {
			return nil
		}
		if err := s.download(rel, backup+".remote-"+e.Machine, ""); err != nil {
			return err
		}
		return s.push(rel, lf)
	}
	if mode&ModePull == 0 {
		return nil
	}
	if err := copyFile(lf.abs, backup+".local"); err != nil {
		return err
	}
	return s.pull(rel, e)
}

func (s *syncer) skip(rel, why string) {
	s.nSkip++
	s.printf("  skip  %s (%s)", rel, why)
}

func (s *syncer) handle(rel string, lf localFile, lok bool, mode Mode, codexLocal map[string]bool) error {
	e, rok := s.man.Files[rel]
	if rok {
		if _, mapped := s.m.localPath(rel); !mapped {
			return nil // a source this machine doesn't sync
		}
	}
	switch {
	case lok && !rok:
		if mode&ModePush != 0 {
			return s.push(rel, lf)
		}
	case !lok && rok:
		if mode&ModePull == 0 {
			return nil
		}
		if b := codexBase(rel); b != "" && codexLocal[b] {
			// Same rollout exists locally under another folder or compression.
			return nil
		}
		return s.pull(rel, e)
	case lok && rok:
		lh, err := s.localHash(rel, lf)
		if err != nil {
			return err
		}
		fst := s.st.get(rel)
		if lh == e.Hash {
			fst.Base = lh
			return nil
		}
		localChanged, remoteChanged := lh != fst.Base, e.Hash != fst.Base
		active := time.Since(time.Unix(0, lf.mtime)) < time.Duration(s.cfg.ActiveMinutes)*time.Minute
		switch {
		case !remoteChanged:
			if mode&ModePush != 0 {
				return s.push(rel, lf)
			}
		case !localChanged:
			if mode&ModePull == 0 {
				return nil
			}
			if active {
				s.skip(rel, "remote is newer but local file is in use")
				return nil
			}
			return s.pull(rel, e)
		default:
			if active {
				s.skip(rel, "conflict, local file is in use")
				return nil
			}
			return s.resolveConflict(rel, lf, e, mode)
		}
	}
	return nil
}

func RunSync(cfg *Config, mode Mode, dry bool, log io.Writer) error {
	dav, err := newDAV(cfg)
	if err != nil {
		return err
	}
	s := &syncer{cfg: cfg, dav: dav, m: newMapper(cfg), st: LoadState(cfg.StatePath()),
		pushed: map[string]Entry{}, dry: dry, log: log, stamp: time.Now().Format("20060102-150405")}
	start := time.Now()
	s.printf("chatsync %s  machine=%s  remote=%s/%s%s", modeName(mode), cfg.Machine, cfg.WebDAVURL, cfg.RemoteDir,
		map[bool]string{true: "  (dry run)", false: ""}[dry])

	if s.man, err = s.fetchManifest(); err != nil {
		return err
	}
	local, err := s.m.scan()
	if err != nil {
		return err
	}
	codexLocal := map[string]bool{}
	keys := map[string]bool{}
	for k := range local {
		keys[k] = true
		if b := codexBase(k); b != "" {
			codexLocal[b] = true
		}
	}
	for k := range s.man.Files {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	s.printf("local files: %d, remote files: %d", len(local), len(s.man.Files))

	for _, rel := range sorted {
		lf, lok := local[rel]
		if err := s.handle(rel, lf, lok, mode, codexLocal); err != nil {
			s.nErr++
			s.printf("  ERROR %s: %v", rel, err)
		}
	}
	if cfg.SyncCodex {
		if err := s.syncIndex(mode); err != nil {
			s.nErr++
			s.printf("  ERROR session_index.jsonl: %v", err)
		}
	}
	if cfg.SyncCodex && cfg.CodexReindex && mode&ModePull != 0 && !dry && s.st.CodexReindexPending {
		// Not counted as a sync error: the flag stays set and the next sync retries.
		if err := CodexReindex(cfg, log, false); errors.Is(err, ErrCodexRunning) {
			s.printf("  codex reindex postponed until Codex is closed: %v", err)
		} else if err != nil {
			s.printf("  WARN codex reindex failed, will retry next sync: %v", err)
		} else {
			s.st.CodexReindexPending = false
		}
	}
	if !dry {
		if err := s.saveManifest(); err != nil {
			s.nErr++
			s.printf("  ERROR manifest: %v", err)
		}
		if err := s.st.Save(cfg.StatePath()); err != nil {
			s.printf("  ERROR state: %v", err)
		}
	}
	s.printf("done in %s: %d up (%s), %d down (%s), %d conflicts, %d skipped, %d errors",
		time.Since(start).Round(time.Second), s.nPush, Human(s.bytesUp), s.nPull, Human(s.bytesDown),
		s.nConflict, s.nSkip, s.nErr)
	if s.nErr > 0 {
		return fmt.Errorf("%d errors", s.nErr)
	}
	return nil
}

// ---- Codex session_index.jsonl ---------------------------------------------

// syncIndex merges Codex's thread-title index line by line (keyed by id,
// newest updated_at wins) instead of treating it as one opaque file.
func (s *syncer) syncIndex(mode Mode) error {
	localPath := filepath.Join(s.cfg.CodexDir, "session_index.jsonl")
	rel := s.cfg.RemoteDir + "/files/codex/session_index.jsonl"

	localB, err := os.ReadFile(localPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var remoteB []byte
	if rc, err := s.dav.Get(rel); err == nil {
		remoteB, err = io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}
	} else if !errors.Is(err, errNotFound) {
		return err
	}

	merged := mergeIndex(localB, remoteB)
	if mode&ModePull != 0 && !bytes.Equal(merged, localB) {
		s.printf("  merge session_index.jsonl -> local")
		if !s.dry {
			if err := writeAtomic(localPath, merged); err != nil {
				return err
			}
			s.st.CodexReindexPending = true
		}
	}
	if mode&ModePush != 0 && !bytes.Equal(merged, remoteB) {
		s.printf("  merge session_index.jsonl -> remote")
		if !s.dry {
			return s.dav.Put(rel, bytes.NewReader(merged), int64(len(merged)))
		}
	}
	return nil
}

func mergeIndex(local, remote []byte) []byte {
	type line struct {
		raw     string
		updated string
	}
	var order []string
	byID := map[string]*line{}
	var loose []string
	add := func(data []byte) {
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			raw := strings.TrimSpace(sc.Text())
			if raw == "" {
				continue
			}
			var v struct {
				ID        string `json:"id"`
				UpdatedAt string `json:"updated_at"`
			}
			if json.Unmarshal([]byte(raw), &v) != nil || v.ID == "" {
				loose = append(loose, raw)
				continue
			}
			if cur, ok := byID[v.ID]; ok {
				if v.UpdatedAt > cur.updated {
					cur.raw, cur.updated = raw, v.UpdatedAt
				}
				continue
			}
			byID[v.ID] = &line{raw, v.UpdatedAt}
			order = append(order, v.ID)
		}
	}
	add(local)
	add(remote)
	var buf bytes.Buffer
	seen := map[string]bool{}
	for _, l := range loose {
		if !seen[l] {
			seen[l] = true
			buf.WriteString(l + "\n")
		}
	}
	for _, id := range order {
		buf.WriteString(byID[id].raw + "\n")
	}
	return buf.Bytes()
}

// ---- helpers ---------------------------------------------------------------

func modeName(m Mode) string {
	switch m {
	case ModePush:
		return "push"
	case ModePull:
		return "pull"
	}
	return "sync"
}

func Human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
