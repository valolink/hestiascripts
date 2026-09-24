// hs — Valolink's HestiaCP tool. Phase 1: the check engine and the
// EngineLink compatibility outputs. See docs/hs-design.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/check/box"
	"github.com/valolink/hestiascripts/internal/compat"
	"github.com/valolink/hestiascripts/internal/sys"
)

// Set at build time: -ldflags "-X main.version=$(git describe --always --dirty)".
var version = "dev"

const usage = `hs — HestiaCP server tool (phase 1: checks)

Usage:
  hs check [--json] [--brief] [--all] [--section NAME] [--save]
      Run every check and report what is not working. Read-only.
      Exit: 2 any fail · 1 any warn/unknown · 0 clean.
  hs compat health         JSON for EngineLink (v-server-health contract)
  hs compat setup-status   JSON for EngineLink (v-server-setup-status contract)
  hs version

Sections: ` + "security, backups, system, performance, web, mail, monitoring" + `
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	env := &check.Env{Sys: sys.Real{}, RepoDir: repoDir()}

	switch os.Args[1] {
	case "check":
		os.Exit(cmdCheck(ctx, env, os.Args[2:]))
	case "compat":
		os.Exit(cmdCompat(ctx, env, os.Args[2:]))
	case "version", "--version":
		fmt.Println("hs", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "hs: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func cmdCheck(ctx context.Context, env *check.Env, args []string) int {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	brief := fs.Bool("brief", false, "one line per box plus failures")
	all := fs.Bool("all", false, "also list working and configured results")
	section := fs.String("section", "", "only this section")
	save := fs.Bool("save", false, "write results to "+statePath)
	fs.Parse(args)

	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "hs: warning: not root — many probes will come back unknown")
	}
	if _, err := os.Stat("/usr/local/hestia"); err != nil {
		fmt.Fprintln(os.Stderr, "hs: warning: /usr/local/hestia not found — this is not a HestiaCP box")
	}

	checks := box.All()
	if *section != "" {
		var sel []check.Check
		for _, c := range checks {
			if c.Section == *section {
				sel = append(sel, c)
			}
		}
		if len(sel) == 0 {
			fmt.Fprintf(os.Stderr, "hs: no checks in section %q\n", *section)
			return 2
		}
		checks = sel
	}

	rs := check.RunAll(ctx, env, checks)
	var shown []check.Result
	for _, r := range rs {
		if r.State != check.NA {
			shown = append(shown, r)
		}
	}
	now := env.Sys.Now()
	host, _ := os.Hostname()

	if *save {
		if err := saveResults(host, shown, now); err != nil {
			fmt.Fprintln(os.Stderr, "hs: could not save results:", err)
		}
	}

	switch {
	case *asJSON:
		check.JSON(os.Stdout, host, shown, now)
	case *brief:
		check.Brief(os.Stdout, host, shown, now)
	default:
		check.Text(os.Stdout, host, shown, now, check.Style{Color: isTTY(), All: *all})
	}
	return check.ExitCode(shown, now)
}

// Compat outputs keep the bash contract: progress lines first, the JSON as
// the final stdout line, exit 0 on a completed run.
func cmdCompat(ctx context.Context, env *check.Env, args []string) int {
	if len(args) != 1 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	var v any
	switch args[0] {
	case "health":
		v = compat.HealthReport(ctx, env)
	case "setup-status":
		fmt.Println("Collecting server setup status...")
		v = compat.SetupStatusReport(ctx, env)
		fmt.Println("Checks complete.")
	default:
		fmt.Fprintf(os.Stderr, "hs: unknown compat output %q\n", args[0])
		return 2
	}
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hs:", err)
		return 1
	}
	fmt.Println(string(b))
	return 0
}

const statePath = "/var/lib/hs/results.json"

func saveResults(host string, rs []check.Result, now time.Time) error {
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		return err
	}
	tmp := statePath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := check.JSON(f, host, rs, now); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, statePath)
}

// repoDir is the hestiascripts checkout hs runs from (the binary lives in the
// repo root; /usr/local/bin/hs is a symlink to it). Used to compare deployed
// templates with the repo. HS_REPO_DIR overrides (testing a binary that
// lives outside the checkout). "" when it cannot be found.
func repoDir() string {
	if d := os.Getenv("HS_REPO_DIR"); d != "" {
		return d
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	for dir := filepath.Dir(exe); dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "templates", "nginx", "wp-rocket.tpl")); err == nil {
			return dir
		}
		if strings.Count(dir, "/") < 2 {
			break
		}
	}
	return ""
}

func isTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
