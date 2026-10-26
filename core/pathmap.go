package core

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Both apps only list a conversation whose working folder exists on this
// machine. A pack made on another computer carries that computer's paths,
// typically C:\Users\<old name>\..., so imports move them under this user's
// home and create any project folder that is still missing.

// createDirs is turned off by tests that import into a sandbox but keep the
// original drive paths.
var createDirs = true

type pathMover struct {
	oldHomes []string // a history can carry several, e.g. after an earlier move
	newHome  string
	created  map[string]bool
}

func newPathMover(oldHomes ...string) *pathMover {
	home, _ := os.UserHomeDir()
	m := &pathMover{newHome: home, created: map[string]bool{}}
	seen := map[string]bool{}
	for _, h := range oldHomes {
		k := strings.ToLower(filepath.Clean(h))
		if h != "" && !seen[k] && k != strings.ToLower(filepath.Clean(home)) {
			seen[k] = true
			m.oldHomes = append(m.oldHomes, h)
		}
	}
	return m
}

// rewriter replaces every old home folder in a transcript's paths; the
// trailing separator keeps C:\Users\Al from matching C:\Users\Alice.
func (m *pathMover) rewriter(only ...string) *lineRewriter {
	lr := &lineRewriter{}
	for _, o := range only {
		lr.only = append(lr.only, []byte(o))
	}
	for _, h := range m.oldHomes {
		lr.add(h+`\`, m.newHome+`\`)
	}
	return lr
}

var homeRe = regexp.MustCompile(`(?i)^(?:\\\\\?\\)?[a-z]:\\users\\[^\\]+`)

// guessHome extracts C:\Users\<name> from a path, for packs that predate
// recording the exporting machine's home folder.
func guessHome(p string) string {
	m := homeRe.FindString(p)
	return strings.TrimPrefix(m, `\\?\`)
}

// move rewrites a path that lived under the old home folder.
func (m *pathMover) move(p string) string {
	long := strings.HasPrefix(p, `\\?\`)
	bare := strings.TrimPrefix(p, `\\?\`)
	for _, old := range m.oldHomes {
		if len(bare) < len(old) || !strings.EqualFold(bare[:len(old)], old) {
			continue
		}
		rest := bare[len(old):]
		if rest != "" && rest[0] != '\\' && rest[0] != '/' {
			continue // C:\Users\Admin must not match C:\Users\Administrator
		}
		out := m.newHome + rest
		if long {
			out = `\\?\` + out
		}
		return out
	}
	return p
}

// ensure makes sure a working folder exists, creating it at the same path, or
// under this user's home when its drive is missing. It returns the folder
// the conversation should now point at.
func (m *pathMover) ensure(p string) string {
	if p == "" {
		return p
	}
	bare := strings.TrimPrefix(p, `\\?\`)
	if info, err := os.Stat(bare); err == nil && info.IsDir() {
		return p
	}
	if vol := filepath.VolumeName(bare); vol == "" || !dirExists(vol+`\`) {
		bare = filepath.Join(m.newHome, strings.TrimSuffix(vol, ":"), bare[len(vol):])
		p = bare
	}
	if !createDirs || os.MkdirAll(bare, 0755) == nil {
		m.created[bare] = true
	}
	return p
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func (m *pathMover) createdDirs() []string {
	var out []string
	for d := range m.created {
		out = append(out, d)
	}
	return out
}

// jsonEsc returns s as it appears inside a JSON string literal.
func jsonEsc(s string) []byte {
	b, _ := json.Marshal(s)
	return b[1 : len(b)-1]
}

// lineRewriter replaces old paths with new ones on matching JSONL lines.
type lineRewriter struct {
	only  [][]byte // rewrite only lines containing one of these; nil = all lines
	pairs [][2][]byte
}

func (lr *lineRewriter) add(from, to string) {
	if from != "" && from != to {
		lr.pairs = append(lr.pairs, [2][]byte{jsonEsc(from), jsonEsc(to)})
	}
}

func (lr *lineRewriter) empty() bool { return len(lr.pairs) == 0 }

func (lr *lineRewriter) line(l []byte) []byte {
	if lr.only != nil {
		hit := false
		for _, o := range lr.only {
			if bytes.Contains(l, o) {
				hit = true
				break
			}
		}
		if !hit {
			return l
		}
	}
	for _, p := range lr.pairs {
		l = bytes.ReplaceAll(l, p[0], p[1])
	}
	return l
}

// copy streams JSONL from r to w, rewriting line by line.
func (lr *lineRewriter) copy(w io.Writer, r io.Reader) error {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		l, err := br.ReadBytes('\n')
		if len(l) > 0 {
			if _, werr := w.Write(lr.line(l)); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// copyZst does the same for a zstd-compressed rollout.
func (lr *lineRewriter) copyZst(w io.Writer, r io.Reader) error {
	dec, err := zstd.NewReader(r, zstd.WithDecoderMaxWindow(1<<31))
	if err != nil {
		return err
	}
	defer dec.Close()
	enc, err := zstd.NewWriter(w)
	if err != nil {
		return err
	}
	if err := lr.copy(enc, dec); err != nil {
		enc.Close()
		return err
	}
	return enc.Close()
}

// openRollout returns a reader over a rollout's JSONL, decompressing .zst.
func openRollout(name string, r io.Reader) (io.Reader, func(), error) {
	if !strings.HasSuffix(name, ".zst") {
		return r, func() {}, nil
	}
	dec, err := zstd.NewReader(r, zstd.WithDecoderMaxWindow(1<<31))
	if err != nil {
		return nil, nil, err
	}
	return dec, dec.Close, nil
}
