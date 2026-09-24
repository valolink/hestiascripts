// Package action is hs's registry of things that change or inspect a box.
//
// Phase 4 wraps before it rewrites: every action runs an existing v-script,
// root-shell script or setup/ menu function. What the registry adds is the
// flow run.sh's run_action got right, made uniform — show the exact command,
// confirm (typed for anything destructive), run it, then re-run the checks it
// affects so the dashboard shows the result instead of a stale green.
package action

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/valolink/hestiascripts/internal/hestia"
)

type Mode int

const (
	// Stream: non-interactive; output is shown live inside the TUI.
	Stream Mode = iota
	// Interactive: the terminal is handed to the command (prompts, pickers,
	// the old run.sh menus) and returned when it exits.
	Interactive
)

type Confirm int

const (
	// ConfirmNone: read-only — runs on enter.
	ConfirmNone Confirm = iota
	// ConfirmYes: changes something recoverable — y to run.
	ConfirmYes
	// ConfirmTyped: destructive or hard to undo — type the target's name.
	ConfirmTyped
)

// Target is what an action runs against: the box, or one site.
type Target struct {
	Domain *hestia.Domain // nil = the box
	Host   string
}

func (t Target) Name() string {
	if t.Domain != nil {
		return t.Domain.Name
	}
	return t.Host
}

type Action struct {
	ID      string
	Title   string
	Section string // box actions: which tab lists it; site actions: "sites"
	Site    bool   // runs per site (needs Target.Domain)
	WPOnly  bool   // site action that needs WordPress
	Mode    Mode
	Confirm Confirm
	Note    string // one line shown with the plan: what it changes, what it costs
	// Recheck: check IDs to re-run afterwards (site checks are scoped to the
	// target domain automatically). Empty = nothing it touches is checked.
	Recheck []string
	// Command builds argv. repo is the hestiascripts checkout.
	Command func(t Target, repo string) []string
	// DescribeFrom: repo-relative script to describe when the command is a
	// wrapper around it (e.g. a pipeline).
	DescribeFrom string
	// NeedsRepo: the command lives in the checkout (setup/ functions,
	// non-v- scripts), so it is unavailable when the repo is not found.
	NeedsRepo bool
}

// Plan is the command as shown on screen. For run.sh menu functions the
// sixteen `source` lines are summarised; everything else is the exact,
// shell-quoted command. The log always records Exact.
func (a Action) Plan(t Target, repo string) string {
	argv := a.Command(t, repo)
	if fn, ok := setupFnName(argv); ok {
		return "setup/" + setupFile(repo, fn) + " → " + fn + "   (bash, SCRIPT_DIR=" + repo + ", every setup/ module sourced as run.sh does)"
	}
	return a.Exact(t, repo)
}

// Exact is the full command line, shell-quoted — what the log records.
func (a Action) Exact(t Target, repo string) string {
	var parts []string
	for _, p := range a.Command(t, repo) {
		parts = append(parts, quote(p))
	}
	return strings.Join(parts, " ")
}

func (a Action) Cmd(t Target, repo string) *exec.Cmd {
	argv := a.Command(t, repo)
	return exec.Command(argv[0], argv[1:]...)
}

func quote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"$`\\;&|<>*?()[]{}!#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// --- command builders -------------------------------------------------------

const bin = "/usr/local/hestia/bin/"

func vscript(name string, args ...string) func(Target, string) []string {
	return func(Target, string) []string { return append([]string{bin + name}, args...) }
}

// siteScript passes --user/--domain the way every v-wp-* script takes them.
func siteScript(name string, extra ...string) func(Target, string) []string {
	return func(t Target, _ string) []string {
		return append([]string{bin + name, "--user=" + t.Domain.User, "--domain=" + t.Domain.Name}, extra...)
	}
}

// repoScript runs a non-v- script from the checkout (root-shell-only tools
// that are deliberately never symlinked into Hestia's bin).
func repoScript(name string, args ...string) func(Target, string) []string {
	return func(_ Target, repo string) []string {
		return append([]string{"bash", filepath.Join(repo, name)}, args...)
	}
}

// setupModules is run.sh's source list: the menu functions depend on each other.
var setupModules = []string{"common", "status", "wpcli", "redis", "fail2ban", "maldet", "netdata", "security", "smtp",
	"php-fpm", "opcache", "mariadb", "nginx-templates", "maintenance", "disk", "audit"}

// setupFn runs one run.sh menu function with every module sourced, exactly
// as run.sh does, then pauses so its output can be read before the TUI returns.
func setupFn(fn string) func(Target, string) []string {
	return func(_ Target, repo string) []string {
		var src []string
		for _, m := range setupModules {
			src = append(src, `source "$SCRIPT_DIR/setup/`+m+`.sh"`)
		}
		script := `export SCRIPT_DIR=` + quote(repo) + `; ` + strings.Join(src, "; ") + `; ` + fn
		return []string{"bash", "-c", script}
	}
}

// Describe returns what the command itself says it does: the leading comment
// block of the script, or the comments above a setup/ function. Read from the
// source, so it cannot drift from what runs. Empty when there is none.
func Describe(a Action, t Target, repo string) []string {
	if a.DescribeFrom != "" {
		return describeScript(filepath.Join(repo, a.DescribeFrom))
	}
	argv := a.Command(t, repo)
	switch {
	case isSetupFn(argv):
		fn, _ := setupFnName(argv)
		return describeFunc(repo, fn)
	case len(argv) >= 2 && argv[0] == "bash" && strings.HasSuffix(argv[1], ".sh"):
		return describeScript(argv[1])
	case strings.HasPrefix(argv[0], bin):
		name := strings.TrimPrefix(argv[0], bin)
		if p := filepath.Join(repo, name+".sh"); repo != "" && fileExists(p) {
			return describeScript(p)
		}
		return describeScript(argv[0])
	}
	return nil
}

const maxDescribe = 40

func describeScript(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for i, l := range strings.Split(string(b), "\n") {
		if i == 0 && strings.HasPrefix(l, "#!") {
			continue
		}
		t := strings.TrimSpace(l)
		if !strings.HasPrefix(t, "#") {
			if len(out) == 0 && t == "" {
				continue
			}
			break
		}
		out = append(out, strings.TrimPrefix(strings.TrimPrefix(t, "#"), " "))
		if len(out) >= maxDescribe {
			out = append(out, "…")
			break
		}
	}
	if out = trimBlank(out); len(out) > 0 {
		return out
	}
	return usageText(string(b))
}

// usageText is the fallback for scripts without a header comment: the first
// heredoc that reads like help text (`cat <<'EOF'` inside usage/show_help).
func usageText(src string) []string {
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		t := strings.TrimSpace(l)
		j := strings.Index(t, "<<")
		if !strings.HasPrefix(t, "cat") || j < 0 {
			continue
		}
		term := strings.Trim(strings.TrimSpace(strings.TrimPrefix(t[j+2:], "-")), `'"`)
		var out []string
		for k := i + 1; k < len(lines) && strings.TrimSpace(lines[k]) != term; k++ {
			out = append(out, strings.TrimRight(lines[k], " "))
		}
		body := strings.Join(out, "\n")
		if !strings.Contains(body, "--") && !strings.Contains(strings.ToLower(body), "usage") {
			continue
		}
		if len(out) > maxDescribe {
			out = append(out[:maxDescribe], "…")
		}
		return trimBlank(out)
	}
	return nil
}

// describeFunc finds `fn() {` in setup/*.sh and returns the comment lines
// directly above it, or failing that the echo'd menu lines inside it.
func describeFunc(repo, fn string) []string {
	files, _ := filepath.Glob(filepath.Join(repo, "setup", "*.sh"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		lines := strings.Split(string(b), "\n")
		for i, l := range lines {
			if !strings.HasPrefix(strings.TrimSpace(l), fn+"()") {
				continue
			}
			var com []string
			for j := i - 1; j >= 0 && strings.HasPrefix(strings.TrimSpace(lines[j]), "#"); j-- {
				com = append([]string{strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(lines[j]), "#"), " ")}, com...)
			}
			if len(com) == 0 {
				// a menu: list its options
				for j := i + 1; j < len(lines) && j < i+80 && !strings.HasPrefix(lines[j], "}"); j++ {
					t := strings.TrimSpace(lines[j])
					if strings.HasPrefix(t, `echo "  `) && strings.Contains(t, ")") {
						com = append(com, strings.Trim(strings.TrimPrefix(t, "echo "), `"`))
					}
				}
			}
			com = append([]string{"setup/" + filepath.Base(f) + " → " + fn}, com...)
			if len(com) > maxDescribe {
				com = append(com[:maxDescribe], "…")
			}
			return trimBlank(com)
		}
	}
	return nil
}

func trimBlank(ls []string) []string {
	for len(ls) > 0 && strings.TrimSpace(ls[len(ls)-1]) == "" {
		ls = ls[:len(ls)-1]
	}
	return ls
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func setupFnName(argv []string) (string, bool) {
	if len(argv) == 3 && argv[0] == "bash" && argv[1] == "-c" && strings.Contains(argv[2], `source "$SCRIPT_DIR/setup/`) {
		return argv[2][strings.LastIndex(argv[2], "; ")+2:], true
	}
	return "", false
}

// setupFile names the setup/ module that defines fn ("?.sh" if not found).
func setupFile(repo, fn string) string {
	files, _ := filepath.Glob(filepath.Join(repo, "setup", "*.sh"))
	for _, f := range files {
		if b, err := os.ReadFile(f); err == nil && strings.Contains(string(b), "\n"+fn+"()") {
			return filepath.Base(f)
		}
	}
	return "?.sh"
}

func isSetupFn(argv []string) bool { _, ok := setupFnName(argv); return ok }
