package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"chatsync/core"
)

const usage = `chatsync - sync Claude Code and Codex local chat history over WebDAV

usage:
  chatsync init                    write a config template next to the exe
  chatsync sync   [--dry-run]      two-way incremental sync
  chatsync push   [--dry-run]      upload local changes only
  chatsync pull   [--dry-run]      download remote changes only
  chatsync export [file.zip]       pack all history into one zip
  chatsync codex-reindex           make Codex pick up pulled sessions and restore titles
                                   (runs automatically after pull; skipped while Codex is open,
                                   --force only if that Codex uses a different CODEX_HOME)
  chatsync schedule --every 30     run "sync" every N minutes (Windows Task Scheduler)
  chatsync schedule --remove       remove the scheduled task

common flags:
  --config path                    config file (default: chatsync.json next to the exe)
  --log path                       append output to a file instead of the console
`

const taskName = "ChatSync"

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("config", core.DefaultConfigPath(), "config file")
	logPath := fs.String("log", "", "log file")
	dry := fs.Bool("dry-run", false, "show what would happen")
	every := fs.Int("every", 30, "minutes between scheduled syncs")
	remove := fs.Bool("remove", false, "remove the scheduled task")
	force := fs.Bool("force", false, "codex-reindex: run even if codex.exe is running")
	fs.Parse(os.Args[2:])

	var log io.Writer = os.Stdout
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			fail(err)
		}
		defer f.Close()
		log = f
	}

	if cmd == "init" {
		if err := core.WriteTemplate(*cfgPath); err != nil {
			fail(err)
		}
		fmt.Printf("wrote %s - fill in webdav_url, username and password, then run `chatsync sync --dry-run`\n", *cfgPath)
		return
	}
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		fmt.Print(usage)
		return
	}

	cfg, err := core.LoadConfig(*cfgPath)
	if err != nil {
		fail(err)
	}
	switch cmd {
	case "sync":
		err = core.RunSync(cfg, core.ModeSync, *dry, log)
	case "push":
		err = core.RunSync(cfg, core.ModePush, *dry, log)
	case "pull":
		err = core.RunSync(cfg, core.ModePull, *dry, log)
	case "export":
		err = core.RunExport(cfg, fs.Arg(0), log)
	case "codex-reindex":
		if err = core.CodexReindex(cfg, log, *force); err == nil {
			st := core.LoadState(cfg.StatePath())
			st.CodexReindexPending = false
			err = st.Save(cfg.StatePath())
		}
	case "schedule":
		err = schedule(cfg, *every, *remove)
	default:
		fmt.Print(usage)
		os.Exit(2)
	}
	if err != nil {
		fail(err)
	}
}

func schedule(cfg *core.Config, every int, remove bool) error {
	if remove {
		return run("schtasks", "/Delete", "/F", "/TN", taskName)
	}
	if every < 5 {
		return fmt.Errorf("--every must be at least 5 minutes")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logFile := filepath.Join(cfg.Dir(), "chatsync.log")
	tr := fmt.Sprintf(`"%s" sync --config "%s" --log "%s"`, exe, cfg.Path(), logFile)
	if err := run("schtasks", "/Create", "/F", "/SC", "MINUTE", "/MO", strconv.Itoa(every), "/TN", taskName, "/TR", tr); err != nil {
		return err
	}
	fmt.Printf("scheduled every %d minutes, log: %s\n", every, logFile)
	return nil
}

func run(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c.Run()
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
