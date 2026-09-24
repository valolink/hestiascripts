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
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/check/box"
	"github.com/valolink/hestiascripts/internal/check/site"
	"github.com/valolink/hestiascripts/internal/compat"
	"github.com/valolink/hestiascripts/internal/fix"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/logcap"
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
  hs fix ID [SUBJECT] [--dry-run]
                           plan a fix from the live box, print each command, run it
  hs fix --list            every fix and the check it resolves
  hs logcap add KEY PATH OWNER SIZE | remove KEY | list
                           hourly size cap for chosen logs (/etc/hs/logcap.conf)
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
	case "fix":
		os.Exit(cmdFix(ctx, env, os.Args[2:]))
	case "logcap":
		os.Exit(cmdLogcap(os.Args[2:]))
	case "site-php":
		os.Exit(cmdSitePHP(ctx, env, os.Args[2:]))
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

// cmdFix plans a fix from the live box and runs it step by step, printing
// every command before it runs. --dry-run prints the plan only (the TUI's
// preview uses the same planner). Stops at the first failing step, except
// for read-only diagnoses, which run every step.
func cmdFix(ctx context.Context, env *check.Env, args []string) int {
	var pos []string
	dry := false
	for _, a := range args {
		switch a {
		case "--dry-run", "-n":
			dry = true
		case "--list":
			for _, f := range fix.All() {
				fmt.Printf("%-18s %-20s %s\n", f.ID, f.Check, f.Title)
			}
			return 0
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) == 0 {
		fmt.Fprintln(os.Stderr, "hs fix: which fix? (hs fix --list)")
		return 2
	}
	f, ok := fix.ByID(pos[0])
	if !ok {
		fmt.Fprintf(os.Stderr, "hs fix: no fix %q (hs fix --list)\n", pos[0])
		return 2
	}
	subject := ""
	if len(pos) > 1 {
		subject = pos[1]
	}
	if f.Scope == "site" && subject == "" {
		fmt.Fprintln(os.Stderr, "hs fix: "+f.ID+" needs a domain")
		return 2
	}
	pctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	steps, err := f.Plan(pctx, env, subject)
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "hs fix:", err)
		return 1
	}
	fmt.Printf("# %s%s\n", f.Title, map[bool]string{true: " — " + subject, false: ""}[subject != ""])
	if len(steps) == 0 {
		fmt.Println("# nothing to do: the box no longer has what this fix resolves")
		return 0
	}
	failed := 0
	for i, st := range steps {
		if st.Why != "" {
			for j, l := range strings.Split(st.Why, "\n") {
				if j == 0 {
					fmt.Printf("\n# %d/%d %s\n", i+1, len(steps), l)
				} else {
					fmt.Printf("#     %s\n", l)
				}
			}
		}
		fmt.Println("$ " + fix.Quote(st.Argv))
		if dry {
			continue
		}
		cmd := exec.CommandContext(ctx, st.Argv[0], st.Argv[1:]...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stdout
		if err := cmd.Run(); err != nil {
			fmt.Printf("! %v\n", err)
			failed++
			if f.Risk != fix.ReadOnly {
				fmt.Printf("\n# stopped after step %d of %d; nothing after it ran\n", i+1, len(steps))
				return 1
			}
		}
	}
	if dry {
		fmt.Printf("\n# dry run: %d step(s), nothing ran\n", len(steps))
		return 0
	}
	if failed > 0 {
		return 1
	}
	fmt.Printf("\n# done: %d step(s)\n", len(steps))
	return 0
}

// cmdSitePHP lists backend (PHP-FPM pool) templates, marks the current one,
// shows the exact command and asks before switching.
func cmdSitePHP(ctx context.Context, env *check.Env, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: hs site-php DOMAIN")
		return 2
	}
	var d hestia.Domain
	for _, x := range hestia.WebDomains(env.Sys) {
		if x.Name == args[0] {
			d = x
		}
	}
	if d.Name == "" {
		fmt.Fprintln(os.Stderr, "hs: no web domain", args[0])
		return 1
	}
	out, err := env.Sys.Run(ctx, "/usr/local/hestia/bin/v-list-web-templates-backend", "plain")
	if err != nil {
		fmt.Fprintln(os.Stderr, "hs: v-list-web-templates-backend:", err)
		return 1
	}
	var tpls []string
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			tpls = append(tpls, f[0])
		}
	}
	fmt.Printf("\n  %s (user %s) — PHP-FPM pool template\n\n", d.Name, d.User)
	for i, t := range tpls {
		mark := "  "
		if t == d.Backend {
			mark = "▸ "
		}
		fmt.Printf("  %s%2d) %s\n", mark, i+1, t)
	}
	fmt.Print("\n  Number (enter cancels): ")
	var in string
	fmt.Scanln(&in)
	n := 0
	fmt.Sscanf(in, "%d", &n)
	if n < 1 || n > len(tpls) {
		fmt.Println("  Cancelled — nothing changed.")
		return 0
	}
	argv := []string{"/usr/local/hestia/bin/v-change-web-domain-backend-tpl", d.User, d.Name, tpls[n-1]}
	fmt.Printf("\n  $ %s\n  Run it? [y/N] ", fix.Quote(argv))
	in = ""
	fmt.Scanln(&in)
	if in != "y" && in != "Y" {
		fmt.Println("  Cancelled — nothing changed.")
		return 0
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stdout
	if err := cmd.Run(); err != nil {
		fmt.Println("  !", err)
		return 1
	}
	fmt.Printf("  %s now uses %s.\n", d.Name, tpls[n-1])
	return 0
}

func cmdLogcap(args []string) int {
	switch {
	case len(args) == 5 && args[0] == "add":
		if err := logcap.Add(args[1], args[2], args[3], args[4]); err != nil {
			fmt.Fprintln(os.Stderr, "hs logcap:", err)
			return 1
		}
		fmt.Printf("capped %s at %s (hourly) — %s, %s\n", args[2], args[4], logcap.ConfPath(), logcap.CronPath())
	case len(args) == 2 && args[0] == "remove":
		if err := logcap.Remove(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "hs logcap:", err)
			return 1
		}
		fmt.Println("removed", args[1])
	case len(args) == 1 && args[0] == "list":
		for _, k := range logcap.Keys() {
			p := logcap.Entries()[k]
			size, _ := logcap.Managed(p)
			fmt.Printf("%-30s %-6s %s\n", k, size, p)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: hs logcap add KEY PATH OWNER SIZE | remove KEY | list")
		return 2
	}
	return 0
}
