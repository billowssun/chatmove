package core

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Round-trips this machine's real history through Export/Import into a temp
// directory. Source folders are only read. Opt in with CHATMOVE_REAL_TEST=1.
func TestRealRoundTrip(t *testing.T) {
	if os.Getenv("CHATMOVE_REAL_TEST") == "" {
		t.Skip("set CHATMOVE_REAL_TEST=1")
	}
	src := DefaultPaths()
	tmp, err := os.MkdirTemp("", "cm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	dst := Paths{filepath.Join(tmp, "claude"), filepath.Join(tmp, "codex")}

	for _, app := range []App{AppClaude, AppCodex} {
		t.Run(string(app), func(t *testing.T) {
			before := GetStats(app, src)
			zipPath := filepath.Join(tmp, string(app)+".zip")
			var calls int
			res, err := Export(app, src, zipPath, func(d, total int64) { calls++ })
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("export: %d sessions, %d files, zip %s, %d progress events", res.Sessions, res.Files, Human(res.Bytes), calls)

			other := AppCodex
			if app == AppCodex {
				other = AppClaude
			}
			if _, err := Import(other, dst, zipPath, filepath.Join(tmp, "迁移"), nil); !errors.Is(err, ErrWrongApp) {
				t.Fatalf("wrong-app import: got %v", err)
			}

			runningCodexCheck = func() ([]string, error) { return nil, nil }
			if app == AppCodex {
				runningCodexCheck = func() ([]string, error) { return []string{"codex.exe (pid 1)"}, nil }
				_, err = Import(app, dst, zipPath, filepath.Join(tmp, "迁移"), nil)
				runningCodexCheck = func() ([]string, error) { return nil, nil }
				if !errors.Is(err, ErrCodexRunning) {
					t.Fatalf("codex running: got %v", err)
				}
			}

			res, err = Import(app, dst, zipPath, filepath.Join(tmp, "迁移"), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("import #1: +%d ~%d =%d, note=%q", res.Added, res.Updated, res.Skipped, res.Note)
			after := GetStats(app, dst)
			if after.Sessions != before.Sessions {
				t.Errorf("sessions: source %d, imported %d", before.Sessions, after.Sessions)
			}
			if app == AppCodex {
				if _, err := os.Stat(filepath.Join(dst.CodexDir, "state_5.sqlite")); err != nil {
					t.Errorf("codex reindex did not create its DB: %v", err)
				}
			}

			res, err = Import(app, dst, zipPath, filepath.Join(tmp, "迁移"), nil)
			if err != nil || res.Added+res.Updated != 0 {
				t.Fatalf("import #2 should change nothing: %+v %v", res, err)
			}

			// Truncate one imported file: the pack's longer copy must win,
			// with the short one backed up.
			files, _ := newMapper(dst.config(app)).scan()
			var victim localFile
			var victimRel string
			for rel, f := range files {
				if f.size > 1000 {
					victim, victimRel = f, rel
					break
				}
			}
			os.Truncate(victim.abs, victim.size/2)
			old := victim.size / 2
			hourAgo := time.Now().Add(-time.Hour)
			os.Chtimes(victim.abs, hourAgo, hourAgo)
			res, err = Import(app, dst, zipPath, filepath.Join(tmp, "迁移"), nil)
			if err != nil || res.Updated != 1 || res.BackupDir == "" {
				t.Fatalf("import #3 should update 1 file: %+v %v", res, err)
			}
			if info, _ := os.Stat(victim.abs); info.Size() != victim.size {
				t.Errorf("victim size %d, want %d", info.Size(), victim.size)
			}
			backup := filepath.Join(res.BackupDir, filepath.FromSlash(victimRel))
			if info, err := os.Stat(backup); err != nil || info.Size() != old {
				t.Errorf("backup missing or wrong size: %v", err)
			}
		})
	}
}
