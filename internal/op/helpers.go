package op

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/check/box"
	"github.com/valolink/hestiascripts/internal/conf"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/plan"
)

const bin = "/usr/local/hestia/bin/"

type Step = plan.Step

func self() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "hs"
}

func confArgs(o conf.Opts) []string {
	var a []string
	if o.Section != "" {
		a = append(a, "--section", o.Section)
	}
	if o.Style != "" {
		a = append(a, "--style", string(o.Style))
	}
	if o.Comment != "" {
		a = append(a, "--comment", o.Comment)
	}
	if o.After != "" {
		a = append(a, "--after", o.After)
	}
	return a
}

// confSet is `hs conf set`: prints before → after, keeps a backup.
func confSet(why, file string, o conf.Opts, kv ...string) Step {
	argv := append(append([]string{self(), "conf", "set"}, confArgs(o)...), file)
	return Step{Why: why, Argv: append(argv, kv...)}
}

func confUnset(why, file string, o conf.Opts, keys ...string) Step {
	argv := append(append([]string{self(), "conf", "unset"}, confArgs(o)...), file)
	return Step{Why: why, Argv: append(argv, keys...)}
}

func confInstall(why, src, dst string) Step {
	return Step{Why: why, Argv: []string{self(), "conf", "install", src, dst}}
}

// aptUpdate refreshes package lists. One broken third-party repository
// makes apt-get update exit non-zero while every other list refreshed —
// run.sh's apt_update_safe tolerated that, and so does this: the install
// after it fails on its own if its packages are really unreachable.
func aptUpdate() Step {
	return Step{Why: "refresh package lists (a failing third-party repository is reported, not fatal)",
		Argv: []string{"sh", "-c", "apt-get update -q 2>&1 | grep -v '^\\(Hit\\|Get\\|Ign\\|Reading\\)' ; exit 0"}}
}

func aptInstall(why string, pkgs ...string) Step {
	return Step{Why: why, Argv: append([]string{"env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y", "-q"}, pkgs...)}
}

func wp(d hestia.Domain, args ...string) []string {
	return append([]string{"runuser", "-u", d.User, "--", "env", "HOME=/home/" + d.User, "wp", "--path=" + d.DocRoot()}, args...)
}

func have(env *check.Env, cmd string) bool { return env.Sys.Have(cmd) }

func exists(env *check.Env, path string) bool {
	_, err := env.Sys.Stat(path)
	return err == nil
}

func readFile(env *check.Env, path string) string {
	b, _ := env.Sys.ReadFile(path)
	return string(b)
}

// fpmVersions: PHP versions with an FPM php.ini, oldest first.
func fpmVersions(env *check.Env) []string {
	var out []string
	for _, v := range box.PHPVersions(env.Sys) {
		if exists(env, "/etc/php/"+v+"/fpm/php.ini") {
			out = append(out, v)
		}
	}
	return out
}

func servesPools(env *check.Env, v string) bool {
	pools, _ := env.Sys.Glob("/etc/php/" + v + "/fpm/pool.d/*.conf")
	for _, p := range pools {
		if !strings.HasSuffix(p, "/dummy.conf") {
			return true
		}
	}
	return false
}

func choicesOf(vals []string, label func(string) string) [][2]string {
	var out [][2]string
	for _, v := range vals {
		out = append(out, [2]string{v, label(v)})
	}
	return out
}

func mb(n int) string {
	if n >= 1024 && n%1024 == 0 {
		return fmt.Sprintf("%d GB", n/1024)
	}
	return fmt.Sprintf("%d MB", n)
}

func ctxOK(ctx context.Context) bool { return ctx.Err() == nil }
