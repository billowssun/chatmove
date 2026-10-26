package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"

	"chatsync/core"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

type App struct {
	ctx   context.Context
	paths core.Paths
	busy  atomic.Bool
}

// Outcome is what the UI gets back from Export/Import. Errors are returned
// as data so the page can show a friendly message instead of a raw rejection.
type Outcome struct {
	Canceled bool         `json:"canceled"`
	Result   *core.Result `json:"result"`
	Error    string       `json:"error"`
	Code     string       `json:"code"` // "codex-running", "wrong-app", "busy"
	Path     string       `json:"path"` // the zip that was picked, for retries
}

func NewApp() *App { return &App{paths: core.DefaultPaths()} }

func (a *App) startup(ctx context.Context) { a.ctx = ctx }

func parseApp(s string) core.App {
	if s == "codex" {
		return core.AppCodex
	}
	return core.AppClaude
}

func (a *App) Stats(app string) core.Stats {
	return core.GetStats(parseApp(app), a.paths)
}

func (a *App) progress(done, total int64) {
	runtime.EventsEmit(a.ctx, "progress", done, total)
}

func zipFilter() []runtime.FileFilter {
	return []runtime.FileFilter{{DisplayName: "聊天记录压缩包 (*.zip)", Pattern: "*.zip"}}
}

func desktop() string {
	home, _ := os.UserHomeDir()
	if d := filepath.Join(home, "Desktop"); dirExists(d) {
		return d
	}
	return home
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func (a *App) Export(app string) Outcome {
	if !a.busy.CompareAndSwap(false, true) {
		return Outcome{Code: "busy", Error: "上一个任务还没完成"}
	}
	defer a.busy.Store(false)
	ap := parseApp(app)
	out, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title:            "导出到哪里？",
		DefaultDirectory: desktop(),
		DefaultFilename:  core.DefaultExportName(ap),
		Filters:          zipFilter(),
	})
	if err != nil {
		return Outcome{Error: err.Error()}
	}
	if out == "" {
		return Outcome{Canceled: true}
	}
	if !strings.EqualFold(filepath.Ext(out), ".zip") {
		out += ".zip"
	}
	runtime.EventsEmit(a.ctx, "started", "export")
	res, err := core.Export(ap, a.paths, out, a.progress)
	if err != nil {
		return Outcome{Error: err.Error()}
	}
	return Outcome{Result: res}
}

// ImportPlan is what the guide page shows after a pack is picked.
type ImportPlan struct {
	Canceled bool              `json:"canceled"`
	Error    string            `json:"error"`
	Zip      string            `json:"zip"`
	Pack     *core.PackSummary `json:"pack"`
	Target   string            `json:"target"` // suggested folder for the files
}

// PickPack asks for the pack to import and proposes where its files go.
// Nothing is written yet.
func (a *App) PickPack(app string) ImportPlan {
	in, err := runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title:            "选择要导入的聊天记录压缩包",
		DefaultDirectory: desktop(),
		Filters:          zipFilter(),
	})
	if err != nil {
		return ImportPlan{Error: err.Error()}
	}
	if in == "" {
		return ImportPlan{Canceled: true}
	}
	pack, err := core.ReadPack(in)
	if err != nil {
		return ImportPlan{Error: err.Error()}
	}
	if pack.App != "" && pack.App != parseApp(app) {
		return ImportPlan{Error: "这个压缩包是从" + map[core.App]string{core.AppClaude: " Claude ", core.AppCodex: " ChatGPT "}[pack.App] + "导出的，请回到首页选对应的卡片。"}
	}
	return ImportPlan{Zip: in, Pack: pack, Target: core.SuggestTarget(parseApp(app))}
}

// PickFolder lets the user choose (or create) the folder for imported files.
func (a *App) PickFolder(current string) string {
	start := filepath.Dir(current)
	if !dirExists(start) {
		start = desktop()
	}
	dir, err := runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title:                "选一个新文件夹，对话用到的所有文件都会放在这里",
		DefaultDirectory:     start,
		CanCreateDirectories: true,
	})
	if err != nil || dir == "" {
		return current
	}
	return dir
}

// ImportTo imports a pack, putting every file the conversations use under
// target. It is also how the page retries after the user closes Codex.
func (a *App) ImportTo(app, zip, target string) Outcome {
	if !a.busy.CompareAndSwap(false, true) {
		return Outcome{Code: "busy", Error: "上一个任务还没完成"}
	}
	defer a.busy.Store(false)
	runtime.EventsEmit(a.ctx, "started", "import")
	res, err := core.Import(parseApp(app), a.paths, zip, target, a.progress)
	switch {
	case errors.Is(err, core.ErrCodexRunning):
		return Outcome{Code: "codex-running", Path: zip}
	case errors.Is(err, core.ErrWrongApp):
		return Outcome{Code: "wrong-app", Error: strings.TrimPrefix(err.Error(), core.ErrWrongApp.Error()+": ")}
	case err != nil:
		return Outcome{Error: err.Error()}
	}
	return Outcome{Result: res}
}

func (a *App) Reveal(path string) {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		exec.Command("explorer", path).Start()
		return
	}
	exec.Command("explorer", "/select,", path).Start()
}
