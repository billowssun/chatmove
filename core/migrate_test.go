package core

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathMover(t *testing.T) {
	m := &pathMover{oldHomes: []string{`C:\Users\Administrator`}, newHome: `C:\Users\billo`, created: map[string]bool{}}
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
	rw.add(`D:\vibe motion`, `D:\vibe-motion`)
	got := string(rw.line([]byte(`{"cwd":"D:\\vibe motion","x":1}`)))
	if got != `{"cwd":"D:\\vibe-motion","x":1}` {
		t.Errorf("rewrite = %s", got)
	}
	if claudeSessionOf("claude/projects/D--x/0f902e67-b4d4-4563-b0c8-5fa83751bca1/custom-title.json") != "0f902e67-b4d4-4563-b0c8-5fa83751bca1" ||
		claudeSessionOf("claude/projects/D--x/memory/MEMORY.md") != "" {
		t.Error("claudeSessionOf")
	}
}

// Exports this machine's real history, then imports it into a sandbox that
// looks like another computer: a different home folder and empty app data.
// Source folders are only read. Opt in with CHATMOVE_MIGRATE_TEST=1.
func TestMigrateToNewMachine(t *testing.T) {
	if os.Getenv("CHATMOVE_MIGRATE_TEST") == "" {
		t.Skip("set CHATMOVE_MIGRATE_TEST=1")
	}
	src := DefaultPaths()
	// Codex may leave a git child running in the sandbox for a while, so
	// clean up best-effort instead of failing on a locked file.
	tmp, _ := os.MkdirTemp("", "chatmove-migrate")
	defer os.RemoveAll(tmp)
	if k := os.Getenv("CHATMOVE_KEEP"); k != "" {
		tmp = k
		os.MkdirAll(tmp, 0755)
	}
	createDirs = false
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
		t.Logf("%s export: %d listed conversations, %d files, %d removed left out, %d project folders (%d too broad: %v), zip %s", app, res.Sessions, res.Files, res.Removed, res.Folders, len(res.SkippedFolders), res.SkippedFolders, Human(res.Bytes))
		if res.Sessions != want[app].Sessions {
			t.Errorf("%s: exported %d, app lists %d", app, res.Sessions, want[app].Sessions)
		}
	}

	// The "new computer".
	home := filepath.Join(tmp, "newhome")
	appData, localAppData := filepath.Join(home, "AppData", "Roaming"), filepath.Join(home, "AppData", "Local")
	acct := firstAccountDir(t)
	metaDir := filepath.Join(localAppData, "Claude-Data", "claude_test", "claude-code-sessions", filepath.FromSlash(acct))
	os.MkdirAll(metaDir, 0755)
	if c := findCodex(); c != "" { // still findable once LOCALAPPDATA moves
		t.Setenv("PATH", filepath.Dir(c)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	t.Setenv("USERPROFILE", home)
	driveRoot = func(letter string) string { // never write to the real drives
		d := filepath.Join(tmp, "drives", letter) + `\`
		os.MkdirAll(d, 0755)
		return d
	}
	t.Setenv("APPDATA", appData)
	t.Setenv("LOCALAPPDATA", localAppData)
	dst := Paths{filepath.Join(home, ".claude"), filepath.Join(home, ".codex")}

	// Claude
	res, err := Import(AppClaude, dst, zips[AppClaude], nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("claude import: +%d ~%d =%d kept=%d removed=%d folders=%d folderFiles=%d note=%q", res.Added, res.Updated, res.Skipped, res.Kept, res.Removed, res.Folders, res.FolderFiles, res.Note)
	metas, _ := filepath.Glob(filepath.Join(metaDir, "local_*.json"))
	if len(metas) != want[AppClaude].Sessions {
		t.Errorf("claude: %d list entries written, want %d", len(metas), want[AppClaude].Sessions)
	}
	for _, f := range metas {
		var m struct{ CliSessionID, Cwd string }
		b, _ := os.ReadFile(f)
		json.Unmarshal(b, &m)
		if strings.Contains(strings.ToLower(m.Cwd), `:\users\`) && !strings.HasPrefix(strings.ToLower(m.Cwd), strings.ToLower(home)) {
			t.Errorf("claude %s: cwd still on the old home: %s", m.CliSessionID, m.Cwd)
		}
		jsonl := filepath.Join(dst.ClaudeDir, "projects", claudeProjectName(m.Cwd), m.CliSessionID+".jsonl")
		fh, err := os.Open(jsonl)
		if err != nil {
			t.Errorf("claude %s: transcript not in the folder for %s", m.CliSessionID, m.Cwd)
			continue
		}
		if c := firstCwd(fh); c != "" && c != m.Cwd {
			t.Errorf("claude %s: transcript cwd %q, list says %q", m.CliSessionID, c, m.Cwd)
		}
		fh.Close()
	}
	if st := GetStats(AppClaude, dst); st.Sessions != want[AppClaude].Sessions {
		t.Errorf("claude: new machine lists %d, want %d", st.Sessions, want[AppClaude].Sessions)
	}

	// Codex
	res, err = Import(AppCodex, dst, zips[AppCodex], nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("codex import: +%d ~%d =%d kept=%d folders=%d folderFiles=%d note=%q", res.Added, res.Updated, res.Skipped, res.Kept, res.Folders, res.FolderFiles, res.Note)
	if res.Note != "" {
		t.Fatalf("codex: %s", res.Note)
	}
	db, err := sql.Open("sqlite", filepath.Join(dst.CodexDir, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Codex gives a thread a preview only from a user message, so threads
	// without one stay hidden on the new machine whatever we register.
	gsNew, _ := readGlobalState(dst.CodexDir)
	var placed []string
	for id := range gsMap(gsNew, "thread-project-assignments") {
		placed = append(placed, id)
	}
	for _, id := range gsList(gsNew, "projectless-thread-ids") {
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
		t.Errorf("codex: new machine sidebar shows %d, want %d (%d without messages)", st.Sessions, want[AppCodex].Sessions-empty, empty)
	} else {
		t.Logf("codex: sidebar shows %d of %d; %d have no user message, which Codex hides itself", st.Sessions, want[AppCodex].Sessions, empty)
	}
	var oldPaths, projects int
	// The sandbox home sits inside the real one, so look for any user folder
	// that is not the sandbox's.
	db.QueryRow(`select count(*) from threads where lower(cwd) like '%:\users\%' and lower(cwd) not like ?`, "%"+strings.ToLower(home)+"%").Scan(&oldPaths)
	db.QueryRow(`select count(*) from projects`).Scan(&projects)
	t.Logf("codex: %d projects, %d threads still pointing at the old home", projects, oldPaths)
	if oldPaths != 0 {
		t.Errorf("codex: %d threads keep old paths", oldPaths)
	}
	gs, _ := readGlobalState(dst.CodexDir)
	t.Logf("codex sidebar: %d chats, %d project assignments, %d projects", len(gsList(gs, "projectless-thread-ids")), len(gsMap(gs, "thread-project-assignments")), len(gsMap(gs, "local-projects")))

	// Importing again changes nothing.
	for _, app := range []App{AppClaude, AppCodex} {
		res, err := Import(app, dst, zips[app], nil)
		if err != nil || res.Added+res.Updated != 0 {
			t.Errorf("%s re-import should change nothing: %+v %v", app, res, err)
		}
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

var _ = bufio.NewReader
