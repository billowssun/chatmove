package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Imports put everything a conversation worked with into one folder the user
// picks, so nothing lands on top of what this machine already has:
//
//	<target>\<project name>\           a conversation's working folder
//	<target>\Codex 对话\<date>\<name>\  Codex chats that had no project
//	<target>\其他产出文件\<D|用户目录>\...  files written outside those folders
//
// Every path the apps keep (working folders, project roots) is rewritten to
// the new place. A map of old to new folders is kept in the target so
// importing again reuses the same places.

const (
	chatFolderName   = "Codex 对话"
	outputFolderName = "其他产出文件"
	relocateMapName  = ".chatmove-map.json"
)

type relocator struct {
	target    string
	mover     *pathMover        // fallback for folders that are not moved
	chatRoots []string          // <home>\Documents\Codex on the old machine
	m         map[string]string // lower(old folder) -> new folder
	olds      []string          // old folders, for prefix lookups
	used      map[string]bool   // lower(new folder)
	created   []string
}

func newRelocator(target string, mover *pathMover, oldHome string) *relocator {
	r := &relocator{target: filepath.Clean(target), mover: mover, m: map[string]string{}, used: map[string]bool{}}
	for _, h := range append(append([]string{oldHome}, mover.oldHomes...), mover.newHome) {
		if h != "" {
			r.chatRoots = append(r.chatRoots, filepath.Join(h, "Documents", "Codex"))
		}
	}
	// Folders placed by an earlier import of the same history keep their place.
	if b, err := os.ReadFile(filepath.Join(r.target, relocateMapName)); err == nil {
		saved := map[string]string{}
		if json.Unmarshal(b, &saved) == nil {
			for old, nw := range saved {
				r.remember(old, nw)
			}
		}
	}
	return r
}

func (r *relocator) remember(old, nw string) {
	k := strings.ToLower(old)
	if _, ok := r.m[k]; !ok {
		r.olds = append(r.olds, old)
	}
	r.m[k] = nw
	r.used[strings.ToLower(nw)] = true
}

func (r *relocator) save() error {
	out := map[string]string{}
	for _, o := range r.olds {
		out[o] = r.m[strings.ToLower(o)]
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return writeAtomic(filepath.Join(r.target, relocateMapName), b)
}

func nonEmptyDir(p string) bool {
	ents, err := os.ReadDir(p)
	return err == nil && len(ents) > 0
}

// assign gives an old folder its own new folder under the target.
func (r *relocator) assign(old string) string {
	if nw, ok := r.m[strings.ToLower(old)]; ok {
		return nw
	}
	var nw string
	for _, root := range r.chatRoots {
		if rest, ok := cutPrefixFold(old, root+`\`); ok {
			nw = filepath.Join(r.target, chatFolderName, rest)
			break
		}
	}
	if nw == "" {
		base := filepath.Base(old)
		if base == "" || base == "." || base == `\` {
			base = "项目"
		}
		nw = filepath.Join(r.target, base)
		for i := 2; r.used[strings.ToLower(nw)] || nonEmptyDir(nw); i++ {
			nw = filepath.Join(r.target, fmt.Sprintf("%s (%d)", base, i))
		}
	}
	if !dirExists(nw) {
		if os.MkdirAll(nw, 0755) == nil {
			r.created = append(r.created, nw)
		}
	}
	r.remember(old, nw)
	return nw
}

// place maps any old path (a working folder, or a file inside one) to where
// it lives now. Folders that are too broad to move keep their place, with
// the old home folder swapped for this machine's.
func (r *relocator) place(p string) string {
	if p == "" {
		return p
	}
	clean := filepath.Clean(strings.TrimPrefix(p, `\\?\`))
	// Longest known folder first, so nested folders win.
	sort.Slice(r.olds, func(i, j int) bool { return len(r.olds[i]) > len(r.olds[j]) })
	for _, old := range r.olds {
		if strings.EqualFold(clean, old) {
			return r.m[strings.ToLower(old)]
		}
		if rest, ok := cutPrefixFold(clean, old+`\`); ok {
			return filepath.Join(r.m[strings.ToLower(old)], rest)
		}
	}
	for _, root := range r.chatRoots {
		if strings.EqualFold(clean, root) {
			nw := filepath.Join(r.target, chatFolderName)
			os.MkdirAll(nw, 0755)
			return nw
		}
	}
	if broadFolderIn(r.mover.move(clean), r.mover.newHome) {
		return r.mover.move(clean)
	}
	return r.assign(clean)
}

// rewriter rewrites every moved folder, then any remaining old home folder,
// on the given lines (all lines when none are given).
func (r *relocator) rewriter(only ...string) *lineRewriter {
	lr := &lineRewriter{}
	for _, o := range only {
		lr.only = append(lr.only, []byte(o))
	}
	olds := append([]string(nil), r.olds...)
	sort.Slice(olds, func(i, j int) bool { return len(olds[i]) > len(olds[j]) })
	for _, old := range olds {
		lr.addPath(old, r.m[strings.ToLower(old)])
	}
	for _, h := range r.mover.oldHomes {
		lr.addPath(h, r.mover.newHome)
	}
	return lr
}

// outputTarget places a file written outside the working folders.
func (r *relocator) outputTarget(rel string) string {
	rest := strings.TrimPrefix(rel, outputPrefix)
	where, path, _ := strings.Cut(rest, "/")
	if where == "~" {
		where = "用户目录"
	}
	return filepath.Join(r.target, outputFolderName, where, filepath.FromSlash(path))
}
