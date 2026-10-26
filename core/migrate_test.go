package core

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathMover(t *testing.T) {
	m := &pathMover{oldHomes: []string{`C:\Users\Administrator`}, newHome: `C:\Users\billo`}
	for in, want := range map[string]string{
		`C:\Users\Administrator\Documents\Codex\2026-10-01\x`: `C:\Users\billo\Documents\Codex\2026-10-01\x`,
		`c:\users\administrator`:                              `C:\Users\billo`,
		`\\?\C:\Users\Administrator\proj`:                     `\\?\C:\Users\billo\proj`,
		`C:\Users\Administrator2\proj`:                        `C:\Users\Administrator2\proj`,
		`D:\vibe motion`:                                      `D:\vibe motion`,
	} {
		if got := m.move(in); got != want {
			t.Errorf("move(%q) = %q, want %q", in, got, want)
		}
	}
	if g := guessHome(`\\?\C:\Users\Administrator\AppData\x`); g != `C:\Users\Administrator` {
		t.Errorf("guessHome = %q", g)
	}
	rw := &lineRewriter{}
	rw.addPath(`D:\vibe motion`, `E:\迁移\vibe motion`)
	for in, want := range map[string]string{
		`{"cwd":"D:\\vibe motion","x":1}`:        `{"cwd":"E:\\迁移\\vibe motion","x":1}`,
		`{"file_path":"D:\\vibe motion\\a.txt"}`: `{"file_path":"E:\\迁移\\vibe motion\\a.txt"}`,
		`{"cwd":"D:\\vibe motion 2"}`:            `{"cwd":"D:\\vibe motion 2"}`,
		`{"cwd":"\\\\?\\D:\\vibe motion\\src"}`:  `{"cwd":"\\\\?\\E:\\迁移\\vibe motion\\src"}`,
	} {
		if got := string(rw.line([]byte(in))); got != want {
			t.Errorf("rewrite %s = %s, want %s", in, got, want)
		}
	}
	// A path moved into the picked folder is not rewritten again, even when
	// that folder sits under the old home.
	once := &lineRewriter{}
	once.addPath(`D:\x`, `C:\Users\old\迁移\x`)
	once.addPath(`C:\Users\old`, `C:\Users\new`)
	if got := string(once.line([]byte(`{"cwd":"D:\\x\\a","b":"C:\\Users\\old\\Desktop"}`))); got != `{"cwd":"C:\\Users\\old\\迁移\\x\\a","b":"C:\\Users\\new\\Desktop"}` {
		t.Errorf("single pass: %s", got)
	}
	if claudeSessionOf("claude/projects/D--x/0f902e67-b4d4-4563-b0c8-5fa83751bca1/custom-title.json") != "0f902e67-b4d4-4563-b0c8-5fa83751bca1" ||
		claudeSessionOf("claude/projects/D--x/memory/MEMORY.md") != "" {
		t.Error("claudeSessionOf")
	}
}

func TestRelocator(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "迁移")
	os.MkdirAll(filepath.Join(target, "xxd"), 0755)
	os.WriteFile(filepath.Join(target, "xxd", "mine.txt"), []byte("user's own"), 0644)
	r := newRelocator(target, &pathMover{oldHomes: []string{`C:\Users\Administrator`}, newHome: tmp}, `C:\Users\Administrator`)
	for _, c := range [][2]string{ // in order: names are handed out first come, first served
		{`D:\vibe motion`, filepath.Join(target, "vibe motion")},
		{`D:\vibe motion\out\a.mp4`, filepath.Join(target, "vibe motion", "out", "a.mp4")},
		{`D:\代码\xxd`, filepath.Join(target, "xxd (2)")}, // "xxd" holds the user's own files
		{`E:\other\xxd`, filepath.Join(target, "xxd (3)")},
		{`C:\Users\Administrator\Documents\Codex\2026-07-14\jin`, filepath.Join(target, chatFolderName, "2026-07-14", "jin")},
		{`C:\Users\Administrator\Documents\Codex`, filepath.Join(target, chatFolderName)},
		{`C:\Users\Administrator\Documents`, filepath.Join(tmp, "Documents")}, // too broad: stays, home swapped
	} {
		if got := r.place(c[0]); got != c[1] {
			t.Errorf("place(%q) = %q, want %q", c[0], got, c[1])
		}
	}
	if err := r.save(); err != nil {
		t.Fatal(err)
	}
	again := newRelocator(target, &pathMover{oldHomes: []string{`C:\Users\Administrator`}, newHome: tmp}, `C:\Users\Administrator`)
	if got := again.place(`E:\other\xxd`); got != filepath.Join(target, "xxd (3)") {
		t.Errorf("a second import moved the folder to %q", got)
	}
}

// Exports this machine's real history, then imports it into a sandbox that
// looks like another computer: a different home folder, empty app data, and
// a folder picked for the project files. Source folders are only read. Opt
// in with CHATMOVE_MIGRATE_TEST=1.
func TestMigrateToNewMachine(t *testing.T) {
	if os.Getenv("CHATMOVE_MIGRATE_TEST") == "" {
		t.Skip("set CHATMOVE_MIGRATE_TEST=1")
	}
	src := DefaultPaths()
	// Codex may leave a git child running in the sandbox for a while, so
	// clean up best-effort instead of failing on a locked file.
	tmp, _ := os.MkdirTemp("", "chatmove-migrate")
	if k := os.Getenv("CHATMOVE_KEEP"); k != "" {
		tmp = k
		os.MkdirAll(tmp, 0755)
	} else {
		defer os.RemoveAll(tmp)
	}
	runningCodexCheck = func() ([]string, error) { return nil, nil }

	zips := map[App]string{}
	want := map[App]Stats{}
	for _, app := range []App{AppClaude, AppCodex} {
		want[app] = GetStats(app, src)
		zips[app] = filepath.Join(tmp, string(app)+".zip")
		res, err := Export(app, src, zips[app], nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s export: %d conversations, %d removed left out, %d project folders, %d other output files (%s), %d too broad %v, zip %s",
			app, res.Sessions, res.Removed, res.Folders, res.Outputs, Human(res.OutputBytes), len(res.SkippedFolders), res.SkippedFolders, Human(res.Bytes))
		if res.Sessions != want[app].Sessions {
			t.Errorf("%s: exported %d, app lists %d", app, res.Sessions, want[app].Sessions)
		}
	}

	// The "new computer".
	home := filepath.Join(tmp, "newhome")
	target := filepath.Join(tmp, "迁移")
	appData, localAppData := filepath.Join(home, "AppData", "Roaming"), filepath.Join(home, "AppData", "Local")
	acct := firstAccountDir(t)
	metaDir := filepath.Join(localAppData, "Claude-Data", "claude_test", "claude-code-sessions", filepath.FromSlash(acct))
	os.MkdirAll(metaDir, 0755)
	if c := findCodex(); c != "" { // still findable once LOCALAPPDATA moves
		t.Setenv("PATH", filepath.Dir(c)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", appData)
	t.Setenv("LOCALAPPDATA", localAppData)
	dst := Paths{filepath.Join(home, ".claude"), filepath.Join(home, ".codex")}
	// Every path the apps keep must now be in the picked folder, or (for a
	// conversation opened in a broad folder) under the new home.
	moved := func(p string) bool {
		p = strings.ToLower(strings.TrimPrefix(p, `\\?\`))
		return strings.HasPrefix(p, strings.ToLower(target)) || strings.HasPrefix(p, strings.ToLower(home))
	}

	// Claude
	res, err := Import(AppClaude, dst, zips[AppClaude], target, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("claude import: +%d ~%d =%d kept=%d folderFiles=%d note=%q", res.Added, res.Updated, res.Skipped, res.Kept, res.FolderFiles, res.Note)
	metas, _ := filepath.Glob(filepath.Join(metaDir, "local_*.json"))
	if len(metas) != want[AppClaude].Sessions {
		t.Errorf("claude: %d list entries written, want %d", len(metas), want[AppClaude].Sessions)
	}
	for _, f := range metas {
		var m struct{ CliSessionID, Cwd string }
		b, _ := os.ReadFile(f)
		json.Unmarshal(b, &m)
		if !moved(m.Cwd) {
			t.Errorf("claude %s: cwd not moved: %s", m.CliSessionID, m.Cwd)
		}
		fh, err := os.Open(filepath.Join(dst.ClaudeDir, "projects", claudeProjectName(m.Cwd), m.CliSessionID+".jsonl"))
		if err != nil {
			t.Errorf("claude %s: transcript not in the folder for %s", m.CliSessionID, m.Cwd)
			continue
		}
		if c := firstCwd(fh); c != "" && c != m.Cwd {
			t.Errorf("claude %s: transcript cwd %q, list says %q", m.CliSessionID, c, m.Cwd)
		}
		fh.Close()
	}

	// Codex
	res, err = Import(AppCodex, dst, zips[AppCodex], target, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("codex import: +%d ~%d =%d kept=%d folderFiles=%d note=%q", res.Added, res.Updated, res.Skipped, res.Kept, res.FolderFiles, res.Note)
	if res.Note != "" {
		t.Fatalf("codex: %s", res.Note)
	}
	db, err := sql.Open("sqlite", filepath.Join(dst.CodexDir, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	notMoved := 0
	if rows, err := db.Query(`select coalesce(cwd, '') from threads`); err == nil {
		for rows.Next() {
			var c string
			if rows.Scan(&c) == nil && c != "" && !moved(c) {
				if notMoved < 3 {
					t.Logf("  not moved: %s", c)
				}
				notMoved++
			}
		}
		rows.Close()
	}
	if notMoved > 0 {
		t.Errorf("codex: %d threads still point outside the picked folder", notMoved)
	}
	// Codex gives a thread a preview only from a user message, so threads
	// without one stay hidden on the new machine whatever we register.
	gs, _ := readGlobalState(dst.CodexDir)
	var placed []string
	for id := range gsMap(gs, "thread-project-assignments") {
		placed = append(placed, id)
	}
	for _, id := range gsList(gs, "projectless-thread-ids") {
		placed = append(placed, str(id))
	}
	empty := 0
	for _, id := range placed {
		var p string
		db.QueryRow(`select coalesce(preview, '') from threads where id = ?`, id).Scan(&p)
		if p == "" {
			empty++
		}
	}
	if st := GetStats(AppCodex, dst); st.Sessions != want[AppCodex].Sessions-empty {
		t.Errorf("codex: sidebar shows %d, want %d (%d without messages)", st.Sessions, want[AppCodex].Sessions-empty, empty)
	} else {
		t.Logf("codex: sidebar shows %d of %d; %d have no user message, which Codex hides itself", st.Sessions, want[AppCodex].Sessions, empty)
	}
	for _, root := range gsMap(gs, "local-projects") {
		p, _ := root.(map[string]any)
		for _, r := range p["rootPaths"].([]any) {
			if !moved(str(r)) {
				t.Errorf("codex project %s still at %s", str(p["name"]), str(r))
			}
		}
	}

	top, _ := os.ReadDir(target)
	t.Logf("picked folder holds %d entries", len(top))
	// Importing again changes nothing and reuses the same folders.
	for _, app := range []App{AppClaude, AppCodex} {
		res, err := Import(app, dst, zips[app], target, nil)
		if err != nil || res.Added+res.Updated != 0 || res.Kept != 0 {
			t.Errorf("%s re-import should change nothing: +%d ~%d kept=%d %v", app, res.Added, res.Updated, res.Kept, err)
		}
	}
	if again, _ := os.ReadDir(target); len(again) != len(top) {
		t.Errorf("re-import changed the picked folder: %d -> %d entries", len(top), len(again))
	}
}

func firstAccountDir(t *testing.T) string {
	for _, r := range claudeMetaRoots() {
		dirs, _ := filepath.Glob(filepath.Join(r, "*", "*"))
		for _, d := range dirs {
			if rel, err := filepath.Rel(r, d); err == nil && dirExists(d) {
				return filepath.ToSlash(rel)
			}
		}
	}
	t.Skip("no Claude desktop data on this machine")
	return ""
}
