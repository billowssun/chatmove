package core

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RunExport writes every synced file into one zip, using the same layout as
// the remote (claude/projects/..., codex/sessions/...).
func RunExport(cfg *Config, out string, log io.Writer) error {
	if out == "" {
		out = filepath.Join(cfg.Dir(), fmt.Sprintf("chat-backup-%s-%s.zip", cfg.Machine, time.Now().Format("20060102-150405")))
	}
	files, err := newMapper(cfg).scan()
	if err != nil {
		return err
	}
	if cfg.SyncCodex {
		idx := filepath.Join(cfg.CodexDir, "session_index.jsonl")
		if info, err := os.Stat(idx); err == nil {
			files["codex/session_index.jsonl"] = localFile{idx, info.Size(), info.ModTime().UnixNano()}
		}
	}
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	f, err := os.Create(out + tmpSuffix)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	var total int64
	for _, rel := range keys {
		lf := files[rel]
		method := zip.Deflate
		if strings.HasSuffix(rel, ".zst") {
			method = zip.Store // already compressed
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: rel, Method: method, Modified: time.Unix(0, lf.mtime)})
		if err != nil {
			break
		}
		src, err := os.Open(lf.abs)
		if err != nil {
			fmt.Fprintf(log, "  skip %s: %v\n", rel, err)
			continue
		}
		n, err := io.Copy(w, io.LimitReader(src, lf.size))
		src.Close()
		if err != nil {
			zw.Close()
			f.Close()
			os.Remove(out + tmpSuffix)
			return fmt.Errorf("%s: %w", rel, err)
		}
		total += n
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(out+tmpSuffix, out); err != nil {
		return err
	}
	info, _ := os.Stat(out)
	fmt.Fprintf(log, "exported %d files (%s raw) -> %s (%s)\n", len(keys), Human(total), out, Human(info.Size()))
	return nil
}
