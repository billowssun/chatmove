package core

import (
	"crypto/md5"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// The Codex desktop sidebar shows a thread only when it is in state_5.sqlite,
// not archived, has a preview, and is placed in .codex-global-state.json:
// listed as a projectless chat, assigned to a project, or working inside a
// project's root folder. Sub-agent and guardian threads follow their parent.
// Anything else is a conversation the user removed.

const codexGlobalState = ".codex-global-state.json"

// codexPlace records where a thread sat in the exporting machine's sidebar.
type codexPlace struct {
	Kind    string `json:"kind"` // "chat" (no project) or "project"
	Project string `json:"project,omitempty"`
	Root    string `json:"root,omitempty"`
	Hint    string `json:"hint,omitempty"` // workspace root hint for chats
}

type codexVisible struct {
	keep   map[string]bool       // every thread worth exporting, children included
	cwd    map[string]string     // their working folders
	places map[string]codexPlace // top-level threads only
	hidden int                   // top-level threads the user removed or archived
}

func normPath(p string) string {
	p = strings.TrimPrefix(p, `\\?\`)
	return strings.ToLower(strings.TrimRight(p, `\/`))
}

func readGlobalState(codexDir string) (map[string]any, error) {
	b, err := os.ReadFile(filepath.Join(codexDir, codexGlobalState))
	if err != nil {
		return nil, err
	}
	gs := map[string]any{}
	return gs, json.Unmarshal(b, &gs)
}

func gsMap(gs map[string]any, key string) map[string]any {
	m, ok := gs[key].(map[string]any)
	if !ok {
		m = map[string]any{}
		gs[key] = m
	}
	return m
}

func gsList(gs map[string]any, key string) []any {
	l, _ := gs[key].([]any)
	return l
}

func str(v any) string { s, _ := v.(string); return s }

type gsProject struct{ id, name, root string }

// gsProjects lists local projects keyed by their normalized root folders.
func gsProjects(gs map[string]any) map[string]gsProject {
	out := map[string]gsProject{}
	for id, v := range gsMap(gs, "local-projects") {
		p, _ := v.(map[string]any)
		roots, _ := p["rootPaths"].([]any)
		for _, r := range roots {
			out[normPath(str(r))] = gsProject{id, str(p["name"]), str(r)}
		}
	}
	return out
}

// openCodexDBCopy opens a private copy of state_5.sqlite, so reading never
// disturbs a Codex that is running.
func openCodexDBCopy(codexDir string) (*sql.DB, func(), error) {
	tmp, err := os.MkdirTemp("", "chatmove-db")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.RemoveAll(tmp) }
	for _, suf := range []string{"", "-wal", "-shm"} {
		src := filepath.Join(codexDir, "state_5.sqlite"+suf)
		if _, err := os.Stat(src); err == nil {
			if err := copyFile(src, filepath.Join(tmp, "state_5.sqlite"+suf)); err != nil {
				cleanup()
				return nil, nil, err
			}
		} else if suf == "" {
			cleanup()
			return nil, nil, err
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(tmp, "state_5.sqlite"))
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return db, func() { db.Close(); cleanup() }, nil
}

// codexSidebar works out which threads the exporting machine's sidebar shows.
// It returns ok=false when Codex's state is missing, in which case nothing
// is filtered.
func codexSidebar(codexDir string) (v codexVisible, ok bool) {
	gs, err := readGlobalState(codexDir)
	if err != nil {
		return v, false
	}
	db, done, err := openCodexDBCopy(codexDir)
	if err != nil {
		return v, false
	}
	defer done()

	type thread struct {
		cwd, kind string
		archived  bool
		preview   bool
	}
	threads := map[string]thread{}
	rows, err := db.Query(`select id, coalesce(cwd, ''), coalesce(archived, 0), coalesce(preview, '') <> '', coalesce(thread_source, '') from threads`)
	if err != nil {
		return v, false
	}
	for rows.Next() {
		var id, cwd, kind string
		var archived, preview bool
		if rows.Scan(&id, &cwd, &archived, &preview, &kind) == nil {
			threads[id] = thread{cwd, kind, archived, preview}
		}
	}
	rows.Close()
	parent := map[string]string{}
	if rows, err := db.Query(`select child_thread_id, parent_thread_id from thread_spawn_edges`); err == nil {
		for rows.Next() {
			var c, p string
			if rows.Scan(&c, &p) == nil {
				parent[c] = p
			}
		}
		rows.Close()
	}

	projectless := map[string]bool{}
	for _, id := range gsList(gs, "projectless-thread-ids") {
		projectless[str(id)] = true
	}
	assigned := gsMap(gs, "thread-project-assignments")
	hints := gsMap(gs, "thread-workspace-root-hints")
	projects := gsProjects(gs)
	byID := map[string]gsProject{}
	for _, p := range projects {
		byID[p.id] = p
	}
	saved := map[string]string{}
	for _, r := range gsList(gs, "electron-saved-workspace-roots") {
		saved[normPath(str(r))] = str(r)
	}

	v = codexVisible{keep: map[string]bool{}, places: map[string]codexPlace{}, cwd: map[string]string{}}
	defer func() {
		for id := range v.keep {
			v.cwd[id] = threads[id].cwd
		}
	}()
	child := func(t thread) bool { return t.kind == "subagent" || t.kind == "guardian_review" }
	for id, t := range threads {
		topLevel := !child(t) && parent[id] == ""
		if t.archived || !t.preview {
			if topLevel {
				v.hidden++
			}
			continue
		}
		var place codexPlace
		switch a, _ := assigned[id].(map[string]any); {
		case projectless[id]:
			place = codexPlace{Kind: "chat", Hint: str(hints[id])}
		case a != nil && byID[str(a["projectId"])].root != "":
			p := byID[str(a["projectId"])]
			place = codexPlace{Kind: "project", Project: p.name, Root: p.root}
		case projects[normPath(t.cwd)].root != "":
			p := projects[normPath(t.cwd)]
			place = codexPlace{Kind: "project", Project: p.name, Root: p.root}
		case saved[normPath(t.cwd)] != "":
			place = codexPlace{Kind: "project", Project: filepath.Base(t.cwd), Root: saved[normPath(t.cwd)]}
		default:
			if topLevel {
				v.hidden++
			}
			continue
		}
		v.keep[id] = true
		v.places[id] = place
	}
	// Children ride along with a visible ancestor.
	for id := range threads {
		if v.keep[id] {
			continue
		}
		for p, hops := parent[id], 0; p != "" && hops < 20; p, hops = parent[p], hops+1 {
			if v.keep[p] && v.places[p].Kind != "" {
				v.keep[id] = true
				break
			}
		}
	}
	return v, true
}

func uuidV7() string {
	var b [16]byte
	rand.Read(b[:])
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(time.Now().UnixMilli()))
	copy(b[:6], ts[2:])
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// registerCodexSidebar places imported threads in this machine's sidebar the
// way they were placed on the exporting machine, creating local projects as
// needed. Codex must not be running. Both files are backed up first.
func registerCodexSidebar(codexDir string, places map[string]codexPlace, mover *pathMover, backupDir string) (int, error) {
	gsPath := filepath.Join(codexDir, codexGlobalState)
	dbPath := filepath.Join(codexDir, "state_5.sqlite")
	gs, err := readGlobalState(codexDir)
	if errors.Is(err, os.ErrNotExist) {
		gs, err = map[string]any{}, nil
	}
	if err != nil {
		return 0, fmt.Errorf("读取 Codex 侧边栏状态失败：%w", err)
	}
	for _, f := range []string{dbPath, dbPath + "-wal", gsPath} {
		if _, err := os.Stat(f); err == nil {
			if err := copyFile(f, filepath.Join(backupDir, filepath.Base(f))); err != nil {
				return 0, err
			}
		}
	}

	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	home, _ := os.UserHomeDir()
	host := "local:" + codexDir
	projects := gsProjects(gs)
	localProjects := gsMap(gs, "local-projects")
	assigned := gsMap(gs, "thread-project-assignments")
	hints := gsMap(gs, "thread-workspace-root-hints")
	idMap := map[string]any{}
	if byHost, ok := gsMap(gs, "app-server-project-id-by-legacy-project-id-by-host")[host].(map[string]any); ok {
		idMap = byHost
	}
	gsMap(gs, "app-server-project-id-by-legacy-project-id-by-host")[host] = idMap
	projectless := map[string]bool{}
	for _, id := range gsList(gs, "projectless-thread-ids") {
		projectless[str(id)] = true
	}

	var pos int64
	tx.QueryRow(`select coalesce(max(position), -1) + 1 from projects`).Scan(&pos)
	ids := make([]string, 0, len(places))
	for id := range places {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	placed := 0
	for _, id := range ids {
		var n int
		if tx.QueryRow(`select count(*) from threads where id = ?`, id).Scan(&n); n == 0 {
			continue // Codex did not take the rollout in (e.g. no messages)
		}
		pl := places[id]
		if pl.Kind == "chat" {
			if !projectless[id] {
				gs["projectless-thread-ids"] = append(gsList(gs, "projectless-thread-ids"), id)
				projectless[id] = true
			}
			hint := mover.move(pl.Hint)
			if hint == "" {
				hint = filepath.Join(home, "Documents", "Codex")
			}
			hints[id] = mover.ensure(hint)
			placed++
			continue
		}
		root := mover.ensure(mover.move(pl.Root))
		p, ok := projects[normPath(root)]
		if !ok {
			legacy := fmt.Sprintf("local-%x", md5.Sum([]byte(root)))
			name := pl.Project
			if name == "" {
				name = filepath.Base(root)
			}
			now := time.Now().UnixMilli()
			dbID := uuidV7()
			for _, q := range []struct {
				sql  string
				args []any
			}{
				{`insert into projects (id, name, metadata, position, created_at_ms, updated_at_ms) values (?, ?, '{}', ?, ?, ?)`, []any{dbID, name, pos, now, now}},
				{`insert into project_roots (project_id, position, path) values (?, 0, ?)`, []any{dbID, root}},
				{`insert into project_idempotency_keys (key, project_id, created_at_ms) values (?, ?, ?)`, []any{legacy, dbID, now}},
			} {
				if _, err := tx.Exec(q.sql, q.args...); err != nil {
					return 0, fmt.Errorf("登记 Codex 项目失败：%w", err)
				}
			}
			pos++
			localProjects[legacy] = map[string]any{"id": legacy, "name": name, "rootPaths": []any{root}, "createdAt": now, "updatedAt": now}
			gs["project-order"] = append(gsList(gs, "project-order"), legacy)
			gs["electron-saved-workspace-roots"] = append(gsList(gs, "electron-saved-workspace-roots"), root)
			idMap[legacy] = dbID
			p = gsProject{legacy, name, root}
			projects[normPath(root)] = p
		}
		assigned[id] = map[string]any{"projectKind": "local", "projectId": p.id}
		dbID := str(idMap[p.id])
		if dbID == "" {
			tx.QueryRow(`select project_id from project_roots where lower(path) = lower(?)`, p.root).Scan(&dbID)
		}
		if dbID != "" {
			if _, err := tx.Exec(`update threads set project_id = ? where id = ?`, dbID, id); err != nil {
				return 0, err
			}
		}
		placed++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	out, err := json.Marshal(gs)
	if err != nil {
		return placed, err
	}
	if err := writeAtomic(gsPath, out); err != nil {
		return placed, err
	}
	// Codex restores from this copy when the main file looks damaged.
	if _, err := os.Stat(gsPath + ".bak"); err == nil {
		os.WriteFile(gsPath+".bak", out, 0600)
	}
	return placed, nil
}
