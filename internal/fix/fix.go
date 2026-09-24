// Package fix turns a finding into the commands that resolve it.
//
// A fix is planned from the live box when it is about to run — it rescans,
// it does not trust the evidence stored with the result — and the plan is a
// list of plain commands, each with the reason for it. The TUI shows that
// list before asking; `hs fix` prints each command before running it. The
// same code produces the preview and the execution, so what you approve is
// what runs.
//
// Reversible where it can be: files are moved, never deleted — dumps and
// logs to the site's own private/ (outside the web root, inside the
// account), suspicious PHP to a root-only quarantine.
package fix

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/sys"
)

type Risk int

const (
	ReadOnly    Risk = iota // diagnosis: runs on enter
	Change                  // reversible change: y
	Destructive             // hard to undo: type the name
)

// Step is one command and why it is in the plan.
type Step struct {
	Why  string
	Argv []string
}

type Fix struct {
	ID    string
	Title string
	Check string // the check it resolves
	// Scope of the subject: "site" (a domain), "all" (no subject — every
	// site), or "" (the result's own subject, e.g. a hestia.conf key).
	Scope   string
	Risk    Risk
	Note    string
	Applies func(r check.Result) bool
	Plan    func(ctx context.Context, env *check.Env, subject string) ([]Step, error)
}

var all []Fix

func register(f Fix) { all = append(all, f) }

func All() []Fix { return all }

func ByID(id string) (Fix, bool) {
	for _, f := range all {
		if f.ID == id {
			return f, true
		}
	}
	return Fix{}, false
}

// For returns the fixes that apply to a result.
func For(r check.Result) []Fix {
	var out []Fix
	for _, f := range all {
		if f.Check == r.Check && (f.Applies == nil || f.Applies(r)) {
			out = append(out, f)
		}
	}
	return out
}

// --- helpers ---------------------------------------------------------------

const bin = "/usr/local/hestia/bin/"

func domainByName(s sys.Sys, name string) (hestia.Domain, error) {
	for _, d := range hestia.WebDomains(s) {
		if d.Name == name {
			return d, nil
		}
	}
	return hestia.Domain{}, fmt.Errorf("no web domain %q on this box", name)
}

func today() string { return time.Now().Format("20060102") }

// flat turns a path relative to the docroot into one file name, so moved
// files from different directories cannot collide ("a/b/x.sql" → "a__b__x.sql").
func flat(rel string) string { return strings.ReplaceAll(rel, "/", "__") }

func wp(d hestia.Domain, args ...string) []string {
	return append([]string{"runuser", "-u", d.User, "--", "env", "HOME=/home/" + d.User, "wp", "--path=" + d.DocRoot()}, args...)
}

func lines(out string) []string {
	var r []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			r = append(r, l)
		}
	}
	return r
}

// moveSteps moves files (absolute paths under root) into dest, creating dest
// owned by owner. mv -n never overwrites; -v prints what moved.
func moveSteps(files []string, root, dest, owner string) []Step {
	if len(files) == 0 {
		return nil
	}
	mk := []string{"install", "-d", "-m", "700", dest}
	if owner != "" {
		mk = []string{"install", "-d", "-o", owner, "-g", owner, "-m", "750", dest}
	}
	steps := []Step{{Why: "destination outside the web root", Argv: mk}}
	for _, f := range files {
		rel := strings.TrimPrefix(f, root+"/")
		steps = append(steps, Step{Why: "move " + rel, Argv: []string{"mv", "-n", "-v", "--", f, filepath.Join(dest, flat(rel))}})
	}
	return steps
}

// quarantineSteps keeps the original path under /root/hs-quarantine.
func quarantineSteps(files []string, d hestia.Domain) []Step {
	if len(files) == 0 {
		return nil
	}
	base := filepath.Join("/root/hs-quarantine", today(), d.Name)
	steps := []Step{{Why: "root-only quarantine", Argv: []string{"install", "-d", "-m", "700", base}}}
	dirs := map[string]bool{}
	for _, f := range files {
		rel := strings.TrimPrefix(f, d.DocRoot()+"/")
		dst := filepath.Join(base, rel)
		if dir := filepath.Dir(dst); !dirs[dir] && dir != base {
			dirs[dir] = true
			steps = append(steps, Step{Argv: []string{"install", "-d", "-m", "700", dir}})
		}
		steps = append(steps, Step{Why: "quarantine " + rel, Argv: []string{"mv", "-n", "-v", "--", f, dst}})
	}
	return steps
}

func find(ctx context.Context, s sys.Sys, args ...string) []string {
	out, _ := s.Run(ctx, "find", args...)
	r := lines(out)
	sort.Strings(r)
	return r
}

var dumpPatterns = []string{"(", "-name", "wp-config*.bak*", "-o", "-name", "wp-config*.save", "-o", "-name", "wp-config*.old",
	"-o", "-name", "*.sql", "-o", "-name", "*.sql.gz", ")", "-type", "f"}

func dumpFiles(ctx context.Context, s sys.Sys, d hestia.Domain) []string {
	return find(ctx, s, append([]string{d.DocRoot(), "-maxdepth", "2"}, dumpPatterns...)...)
}

func private(d hestia.Domain) string {
	return "/home/" + d.User + "/web/" + d.Name + "/private/hs-moved-" + today()
}

// --- the fixes -------------------------------------------------------------

func init() {
	register(Fix{
		ID: "dumps", Title: "Move database dumps out of the web root", Check: "web.exposure", Scope: "site", Risk: Change,
		Note:    "Moves *.sql, *.sql.gz and wp-config backups (docroot, two levels deep) to the site's private/ — not web-served, still in the account. Nothing is deleted.",
		Applies: func(r check.Result) bool { return strings.Contains(r.Summary, "dump") },
		Plan: func(ctx context.Context, env *check.Env, subject string) ([]Step, error) {
			d, err := domainByName(env.Sys, subject)
			if err != nil {
				return nil, err
			}
			return moveSteps(dumpFiles(ctx, env.Sys, d), d.DocRoot(), private(d), d.User), nil
		},
	})
	register(Fix{
		ID: "dumps-all", Title: "Move every dump out of every web root", Check: "web.exposure", Scope: "all", Risk: Change,
		Note:    "The same move for every site on the box, each into its own private/.",
		Applies: func(r check.Result) bool { return strings.Contains(r.Summary, "dump") },
		Plan: func(ctx context.Context, env *check.Env, _ string) ([]Step, error) {
			var steps []Step
			for _, d := range hestia.WebDomains(env.Sys) {
				steps = append(steps, moveSteps(dumpFiles(ctx, env.Sys, d), d.DocRoot(), private(d), d.User)...)
			}
			return steps, nil
		},
	})
	register(Fix{
		ID: "debug-log", Title: "Stop debug logging and move the log out", Check: "web.exposure", Scope: "site", Risk: Change,
		Note:    "Sets WP_DEBUG false (so the log stops growing), then moves wp-content/debug*.log to private/. Nothing is deleted — remove it there once read.",
		Applies: func(r check.Result) bool { return strings.Contains(r.Summary, "debug") },
		Plan: func(ctx context.Context, env *check.Env, subject string) ([]Step, error) {
			d, err := domainByName(env.Sys, subject)
			if err != nil {
				return nil, err
			}
			var steps []Step
			if b, err := env.Sys.ReadFile(d.DocRoot() + "/wp-config.php"); err == nil && wpDebugOn.Match(b) {
				steps = append(steps, Step{Why: "stop writing the log", Argv: wp(d, "config", "set", "WP_DEBUG", "false", "--raw")})
			}
			logs, _ := env.Sys.Glob(d.DocRoot() + "/wp-content/debug*.log")
			return append(steps, moveSteps(logs, d.DocRoot(), private(d), d.User)...), nil
		},
	})
	register(Fix{
		ID: "wp-debug-off", Title: "Turn WP_DEBUG off", Check: "site.debug", Scope: "site", Risk: Change,
		Note: "wp config set WP_DEBUG false, as the site's user.",
		Plan: func(ctx context.Context, env *check.Env, subject string) ([]Step, error) {
			d, err := domainByName(env.Sys, subject)
			if err != nil {
				return nil, err
			}
			return []Step{{Why: "production sites should not log every notice", Argv: wp(d, "config", "set", "WP_DEBUG", "false", "--raw")}}, nil
		},
	})
	register(Fix{
		ID: "uploads-php", Title: "Quarantine PHP files from uploads", Check: "web.uploads-php", Scope: "site", Risk: Change,
		Note: "Moves every .php/.phtml/.phar in wp-content/uploads (except index.php) to /root/hs-quarantine/<date>/<domain>/, keeping its path. Some are legitimate plugin files (Sucuri, WP All Import) — read the list; move one back if the plugin needs it.",
		Plan: func(ctx context.Context, env *check.Env, subject string) ([]Step, error) {
			d, err := domainByName(env.Sys, subject)
			if err != nil {
				return nil, err
			}
			files := find(ctx, env.Sys, d.DocRoot()+"/wp-content/uploads", "-type", "f", "(", "-iname", "*.php", "-o", "-iname", "*.phtml",
				"-o", "-iname", "*.phar", ")", "!", "-name", "index.php")
			return quarantineSteps(files, d), nil
		},
	})
	register(Fix{
		ID: "wp-core", Title: "Restore WordPress core files", Check: "site.core", Scope: "site", Risk: Destructive,
		Note:    "Quarantines files that should not exist in core, then re-downloads the same WordPress version over wp-admin/ and wp-includes/ (content untouched). Take a backup first if unsure.",
		Applies: func(r check.Result) bool { return r.State == check.Fail || r.State == check.Warn },
		Plan: func(ctx context.Context, env *check.Env, subject string) ([]Step, error) {
			d, err := domainByName(env.Sys, subject)
			if err != nil {
				return nil, err
			}
			ver, _ := env.Sys.Run(ctx, "runuser", "-u", d.User, "--", "env", "HOME=/home/"+d.User, "wp", "--path="+d.DocRoot(), "core", "version")
			ver = strings.TrimSpace(ver)
			if ver == "" {
				return nil, fmt.Errorf("wp core version failed for %s", d.Name)
			}
			out, err := env.Sys.Run(ctx, "runuser", "-u", d.User, "--", "env", "HOME=/home/"+d.User, "wp", "--path="+d.DocRoot(), "core", "verify-checksums")
			detail := out
			if ee, ok := err.(*sys.ExitError); ok {
				detail += "\n" + ee.Stderr
			}
			var extra []string
			for _, l := range lines(detail) {
				if f, ok := strings.CutPrefix(l, "Warning: File should not exist: "); ok {
					extra = append(extra, d.DocRoot()+"/"+f)
				}
			}
			steps := quarantineSteps(extra, d)
			return append(steps, Step{Why: "replace modified or missing core files with the " + ver + " release",
				Argv: wp(d, "core", "download", "--force", "--skip-content", "--version="+ver)}), nil
		},
	})
	register(Fix{
		ID: "redis-guard", Title: "Add the Redis flush guard", Check: "redis.sites", Scope: "site", Risk: Change,
		Note:    "WP_REDIS_DISABLE_GROUP_FLUSH true and WP_REDIS_MAXTTL 86400 in wp-config (the kuumalahde 504 fix).",
		Applies: func(r check.Result) bool { return strings.Contains(r.Summary, "WP_REDIS_DISABLE_GROUP_FLUSH") },
		Plan: func(ctx context.Context, env *check.Env, subject string) ([]Step, error) {
			d, err := domainByName(env.Sys, subject)
			if err != nil {
				return nil, err
			}
			steps := []Step{{Why: "group flush falls back to FLUSHDB instead of scanning the keyspace",
				Argv: wp(d, "config", "set", "WP_REDIS_DISABLE_GROUP_FLUSH", "true", "--raw", "--type=constant")}}
			if b, _ := env.Sys.ReadFile(d.DocRoot() + "/wp-config.php"); !strings.Contains(string(b), "WP_REDIS_MAXTTL") {
				steps = append(steps, Step{Why: "keys expire, so the keyspace stays bounded",
					Argv: wp(d, "config", "set", "WP_REDIS_MAXTTL", "86400", "--raw", "--type=constant")})
			}
			return steps, nil
		},
	})
	register(Fix{
		ID: "letsencrypt", Title: "Issue a Let's Encrypt certificate", Check: "web.ssl", Scope: "site", Risk: Change,
		Note: "Fails if the domain's DNS does not point here (see the site's DNS column) — then it was a stale copy, not an outage.",
		Plan: func(ctx context.Context, env *check.Env, subject string) ([]Step, error) {
			d, err := domainByName(env.Sys, subject)
			if err != nil {
				return nil, err
			}
			return []Step{{Why: "renew the expired certificate", Argv: []string{bin + "v-add-letsencrypt-domain", d.User, d.Name}}}, nil
		},
	})
	register(Fix{
		ID: "hestia-conf-key", Title: "Clear the stale hestia.conf service key", Check: "hestia.services", Scope: "", Risk: Change,
		Note: "Hestia stops trying to restart a service whose package is gone (and stops emailing root about it).",
		Plan: func(ctx context.Context, env *check.Env, subject string) ([]Step, error) {
			if !regexp.MustCompile(`^[A-Z_]+_SYSTEM$`).MatchString(subject) {
				return nil, fmt.Errorf("not a *_SYSTEM key: %q", subject)
			}
			return []Step{{Why: subject + " names a removed package", Argv: []string{bin + "v-change-sys-config-value", subject, ""}}}, nil
		},
	})
	register(Fix{
		ID: "postfix-start", Title: "Start postfix and send the queue", Check: "mail.delivery", Scope: "", Risk: Change,
		Applies: func(r check.Result) bool { return strings.Contains(r.Summary, "postfix is not running") },
		Note:    "If it stops again, the status output below says why.",
		Plan: func(ctx context.Context, env *check.Env, _ string) ([]Step, error) {
			return []Step{
				{Why: "start the MTA", Argv: []string{"systemctl", "start", "postfix"}},
				{Why: "deliver what is waiting", Argv: []string{"postqueue", "-f"}},
				{Why: "confirm it stayed up", Argv: []string{"systemctl", "--no-pager", "status", "postfix"}},
			}, nil
		},
	})
	register(Fix{
		ID: "netdata-port", Title: "Close port 19999 in the firewall", Check: "netdata", Scope: "", Risk: Change,
		Applies: func(r check.Result) bool { return strings.Contains(r.Summary, "19999") },
		Note:    "EngineLink reads Netdata through the streamer; nothing needs 19999 from outside.",
		Plan: func(ctx context.Context, env *check.Env, _ string) ([]Step, error) {
			b, _ := env.Sys.ReadFile(hestia.Root + "/data/firewall/rules.conf")
			var steps []Step
			for _, l := range strings.Split(string(b), "\n") {
				kv := hestia.ParseKV(l)
				if kv["PORT"] == "19999" && kv["RULE"] != "" {
					steps = append(steps, Step{Why: fmt.Sprintf("rule %s opens 19999 to %s", kv["RULE"], kv["IP"]),
						Argv: []string{bin + "v-delete-firewall-rule", kv["RULE"]}})
				}
			}
			return steps, nil
		},
	})
	register(Fix{
		ID: "failed-units", Title: "Show why the failed units failed", Check: "systemd.failed", Scope: "", Risk: ReadOnly,
		Note: "Diagnosis only: restarting blindly hides the cause. Each unit's status and last journal lines.",
		Plan: func(ctx context.Context, env *check.Env, _ string) ([]Step, error) {
			out, _ := env.Sys.Run(ctx, "systemctl", "--failed", "--no-legend", "--plain")
			var steps []Step
			for _, l := range lines(out) {
				u := strings.Fields(l)[0]
				steps = append(steps, Step{Why: u, Argv: []string{"systemctl", "--no-pager", "-n", "15", "status", u}})
			}
			return steps, nil
		},
	})
	register(Fix{
		ID: "storagebox-test", Title: "Test the Storage Box connection", Check: "backups.nightly", Scope: "", Risk: ReadOnly,
		Applies: func(r check.Result) bool { return strings.Contains(r.Summary, "restic unreachable") },
		Note:    "TCP to the Storage Box's port 23 from this box. Refused here but open from another box = this IP is banned for failed logins (Hetzner lifts it after a while).",
		Plan: func(ctx context.Context, env *check.Env, _ string) ([]Step, error) {
			host, port := rcloneHost(env.Sys)
			if host == "" {
				return nil, fmt.Errorf("no sftp remote with a host in /root/.config/rclone/rclone.conf")
			}
			return []Step{{Why: "is " + host + ":" + port + " reachable from here", Argv: []string{"nc", "-vz", "-w", "5", host, port}}}, nil
		},
	})
	register(Fix{
		ID: "proxy-wp-secure", Title: "Put the site on the wp-secure template", Check: "web.template-usage", Scope: "site", Risk: Change,
		Note: "Hardened proxy template (deny dumps, logs, PHP in uploads, xmlrpc…). Rebuilds the domain's nginx config. Use wp-rocket instead for WP Rocket sites.",
		Plan: func(ctx context.Context, env *check.Env, subject string) ([]Step, error) {
			return proxySteps(env, subject, "wp-secure")
		},
	})
	register(Fix{
		ID: "proxy-wp-rocket", Title: "Put the site on the wp-rocket template", Check: "web.template-usage", Scope: "site", Risk: Change,
		Note: "Hardened + serves WP Rocket's cached pages straight from nginx. Only for sites running WP Rocket.",
		Plan: func(ctx context.Context, env *check.Env, subject string) ([]Step, error) {
			return proxySteps(env, subject, "wp-rocket")
		},
	})
}

var wpDebugOn = regexp.MustCompile(`(?m)^\s*define\s*\(\s*['"]WP_DEBUG['"]\s*,\s*(true|1)\s*\)`)

func proxySteps(env *check.Env, subject, tpl string) ([]Step, error) {
	d, err := domainByName(env.Sys, subject)
	if err != nil {
		return nil, err
	}
	if _, err := env.Sys.Stat(hestia.NginxTpl + "/" + tpl + ".tpl"); err != nil {
		return nil, fmt.Errorf("template %s is not installed — Web server → Install / update nginx templates first", tpl)
	}
	return []Step{{Why: fmt.Sprintf("%s → %s", orDash(d.Proxy), tpl), Argv: []string{bin + "v-change-web-domain-proxy-tpl", d.User, d.Name, tpl}}}, nil
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func rcloneHost(s sys.Sys) (host, port string) {
	b, _ := s.ReadFile("/root/.config/rclone/rclone.conf")
	port = "23"
	in := false
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "[") {
			in = true // take the first remote with a host
			continue
		}
		if !in {
			continue
		}
		if k, v, ok := strings.Cut(l, "="); ok {
			switch strings.TrimSpace(k) {
			case "host":
				if host == "" {
					host = strings.TrimSpace(v)
				}
			case "port":
				if host != "" && port == "23" {
					port = strings.TrimSpace(v)
				}
			}
		}
	}
	return host, port
}

// Quote renders argv as a copy-pasteable shell line.
func Quote(argv []string) string {
	var parts []string
	for _, a := range argv {
		if a != "" && !strings.ContainsAny(a, " \t\n'\"$`\\;&|<>*?()[]{}!#~") {
			parts = append(parts, a)
		} else {
			parts = append(parts, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
		}
	}
	return strings.Join(parts, " ")
}
