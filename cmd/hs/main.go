// hs — Valolink's HestiaCP tool. Phase 1: the check engine and the
// EngineLink compatibility outputs. See docs/hs-design.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/check/box"
	"github.com/valolink/hestiascripts/internal/check/site"
	"github.com/valolink/hestiascripts/internal/compat"
	"github.com/valolink/hestiascripts/internal/serve"
	"github.com/valolink/hestiascripts/internal/state"
	"github.com/valolink/hestiascripts/internal/sys"
	"github.com/valolink/hestiascripts/internal/tui"
)

// Set at build time: -ldflags "-X main.version=$(git describe --always --dirty)".
var version = "dev"

const usage = `hs — HestiaCP server tool (phase 1: checks)

Usage:
  hs                       interactive dashboard (read-only)
  hs check [--json] [--brief] [--all] [--sites] [--section NAME] [--save]
      Run every check and report what is not working. Read-only.
      Exit: 2 any fail · 1 any warn/unknown · 0 clean.
  hs compat health         JSON for EngineLink (v-server-health contract)
  hs compat setup-status   JSON for EngineLink (v-server-setup-status contract)
  hs serve [--addr :8091]  the streamer EngineLink talks to (replaces hestia-streamer);
                           token from HESTIA_STREAMER_TOKEN
  hs version

Sections: ` + "security, backups, sites, system, performance, web, mail, monitoring" + `
`

func main() {
	env := &check.Env{Sys: sys.Real{}, RepoDir: repoDir()}
	if len(os.Args) < 2 {
		if !isTTY() {
			fmt.Print(usage)
			os.Exit(2)
		}
		if os.Geteuid() != 0 {
			fmt.Fprintln(os.Stderr, "hs: run as root — most probes need it")
			os.Exit(2)
		}
		if err := tui.Run(env, version); err != nil {
			fmt.Fprintln(os.Stderr, "hs:", err)
			os.Exit(1)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	switch os.Args[1] {
	case "check":
		os.Exit(cmdCheck(ctx, env, os.Args[2:]))
	case "compat":
		os.Exit(cmdCompat(ctx, env, os.Args[2:]))
	case "serve":
		os.Exit(cmdServe(os.Args[2:]))
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
	save := fs.Bool("save", false, "write results to "+state.Dir()+"/results.json (merged with what is there)")
	sites := fs.Bool("sites", false, "also run the per-site checks (WP-CLI per WordPress site; slower)")
	fs.Parse(args)

	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "hs: warning: not root — many probes will come back unknown")
	}
	if _, err := os.Stat("/usr/local/hestia"); err != nil {
		fmt.Fprintln(os.Stderr, "hs: warning: /usr/local/hestia not found — this is not a HestiaCP box")
	}

	checks := box.All()
	if *sites || *section == "sites" {
		checks = append(checks, site.All(env)...)
	}
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
		prev, _ := state.Load()
		if err := state.Save(host, check.Merge(prev, shown), now); err != nil {
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

func cmdServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8091", "listen address")
	fs.Parse(args)

	token := os.Getenv("HESTIA_STREAMER_TOKEN")
	srv := &http.Server{
		Addr:              *addr,
		Handler:           serve.Default(token).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: /execute streams for as long as the script runs.
	}
	auth := "token required"
	if token == "" {
		auth = "NO TOKEN — auth disabled"
	}
	fmt.Printf("hs %s serving on %s (%s)\n", version, *addr, auth)
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "hs serve:", err)
		return 1
	}
	return 0
}
