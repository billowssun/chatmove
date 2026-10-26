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

// A pack made on another computer carries that computer's paths, typically
// C:Users<old name>.... The relocator moves working folders into the
// folder the user picks; the mover handles what stays put, such as a chat
// opened in the old Documents folder, by swapping in this user's home.

type pathMover struct {
	oldHomes []string // a history can carry several, e.g. after an earlier move
	newHome  string
}

func newPathMover(oldHomes ...string) *pathMover {
	home, _ := os.UserHomeDir()
	m := &pathMover{newHome: home}
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

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
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

// addPath replaces a folder path and anything inside it, but not a sibling
// whose name merely starts the same (D:\a must not touch D:\ab).
func (lr *lineRewriter) addPath(from, to string) {
	if from == "" || strings.EqualFold(from, to) {
		return
	}
	f, t := jsonEsc(from), jsonEsc(to)
	sep := []byte(`\\`)
	lr.pairs = append(lr.pairs,
		[2][]byte{append(append([]byte{}, f...), sep...), append(append([]byte{}, t...), sep...)},
		[2][]byte{append(append([]byte{}, f...), '"'), append(append([]byte{}, t...), '"')})
}

func (lr *lineRewriter) empty() bool { return lr == nil || len(lr.pairs) == 0 }

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
	// One pass over the line: each spot is rewritten at most once, by the
	// earliest pair that matches there, so a path already moved into the
	// new folder is never rewritten again by a later, broader pair.
	var out []byte
	pos := 0
	for {
		at, pick := -1, -1
		for i, p := range lr.pairs {
			j := bytes.Index(l[pos:], p[0])
			if j >= 0 && (at < 0 || j < at) {
				at, pick = j, i
			}
		}
		if at < 0 {
			break
		}
		out = append(out, l[pos:pos+at]...)
		out = append(out, lr.pairs[pick][1]...)
		pos += at + len(lr.pairs[pick][0])
	}
	if out == nil {
		return l
	}
	return append(out, l[pos:]...)
}

// with returns a copy that tries pairs for from -> to before its own.
func (lr *lineRewriter) with(pairs ...[2]string) *lineRewriter {
	first := &lineRewriter{only: lr.only}
	for _, p := range pairs {
		first.addPath(p[0], p[1])
	}
	first.pairs = append(first.pairs, lr.pairs...)
	return first
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
