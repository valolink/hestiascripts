package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"time"

	"github.com/valolink/hestiascripts/internal/aptinfo"
	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/check/box"
	"github.com/valolink/hestiascripts/internal/sys"
)

const aptUsage = `hs apt — read-only views of pending package updates

  hs apt preview [--refresh]   what apt-get upgrade would do, classified (refresh = apt-get update first)
  hs apt repos                 refresh package lists and explain every failing repository
  hs apt status [--json]       every repository verified without apt-get update (signed InRelease,
                               current key), pending security updates, unattended-upgrades —
                               one JSON line with --json (v-server-health embeds it for EngineLink)
`

func aptUpdateOut() (string, error) {
	out, err := exec.Command("apt-get", "update").CombinedOutput()
	return string(out), err
}

func cmdApt(args []string) int {
	if len(args) == 0 {
		fmt.Print(aptUsage)
		return 2
	}
	switch args[0] {
	case "status":
		return cmdAptStatus(args[1:])
	case "preview":
		if len(args) > 1 && args[1] == "--refresh" {
			fmt.Println("refreshing package lists …")
			out, _ := aptUpdateOut()
			ok, total, errs := aptinfo.ParseUpdate(out)
			fmt.Printf("%d / %d repositories reachable\n", ok, total)
			for _, e := range errs {
				fmt.Printf("  ! %s %s — %s   (hs apt repos explains)\n", e.URL, e.Suite, e.Reason)
			}
			fmt.Println()
		}
		out, err := exec.Command("apt-get", "-s", "upgrade").Output()
		if err != nil {
			fmt.Fprintln(os.Stderr, "hs apt: apt-get -s upgrade:", err)
			return 1
		}
		for _, l := range aptinfo.Parse(string(out)).Render(0) {
			fmt.Println(l)
		}
		return 0
	case "repos":
		fmt.Println("refreshing package lists — nothing is installed, removed or upgraded")
		out, err := aptUpdateOut()
		ok, total, errs := aptinfo.ParseUpdate(out)
		fmt.Printf("\n%d / %d repositories reachable\n", ok, total)
		if err == nil && len(errs) == 0 {
			fmt.Println("every configured repository responded")
			return 0
		}
		for _, e := range errs {
			host := e.URL
			if _, after, ok := strings.Cut(host, "://"); ok {
				host, _, _ = strings.Cut(after, "/")
			}
			file := "(not found under /etc/apt)"
			if g, _ := exec.Command("grep", "-rls", "--", e.URL, "/etc/apt/sources.list", "/etc/apt/sources.list.d/").Output(); len(g) > 0 {
				file = strings.SplitN(strings.TrimSpace(string(g)), "\n", 2)[0]
			}
			meaning, fix := aptinfo.Advice(e.Reason)
			fmt.Printf("\n✗ %s\n  URL         %s\n  Suite       %s\n  Reason      %s\n  Defined in  %s\n", host, e.URL, e.Suite, e.Reason, file)
			if meaning != "" {
				fmt.Printf("  Meaning     %s\n", meaning)
			}
			fmt.Printf("  Fix         %s\n", fix)
		}
		if len(errs) == 0 {
			fmt.Println(strings.TrimSpace(out))
		}
		fmt.Println()
		for _, l := range aptinfo.Primer {
			fmt.Println("  " + l)
		}
		return 1
	}
	fmt.Print(aptUsage)
	return 2
}

func cmdAptStatus(args []string) int {
	asJSON := len(args) > 0 && args[0] == "--json"
	env := &check.Env{Sys: sys.Real{}, RepoDir: repoDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a := box.GetAptStatus(ctx, env)
	if asJSON {
		b, _ := json.Marshal(a)
		fmt.Println(string(b))
		return 0
	}
	fmt.Printf("%d / %d repositories verified\n", a.Repos.OK, a.Repos.Total)
	for _, f := range a.Repos.Failing {
		fmt.Printf("\n✗ %s %s\n  %s\n  Fix  %s\n", f.Host, f.Suite, f.Reason, f.Fix)
	}
	fmt.Printf("\nsecurity updates pending: %d\nunattended-upgrades: %s", a.SecurityPending, a.Unattended.Scope)
	if a.Unattended.LastRun != "" {
		fmt.Printf(", last ran %s", a.Unattended.LastRun)
	}
	fmt.Println()
	if len(a.Repos.Failing) > 0 {
		return 1
	}
	return 0
}
