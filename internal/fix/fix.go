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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/logcap"
	"github.com/valolink/hestiascripts/internal/plan"
	"github.com/valolink/hestiascripts/internal/redisinfo"
	"github.com/valolink/hestiascripts/internal/sys"
	"github.com/valolink/hestiascripts/internal/wpfiles"
)

type Risk int

const (
	ReadOnly    Risk = iota // diagnosis: runs on enter
	Change                  // reversible change: y
	Destructive             // hard to undo: type the name
)

// Step is one command and why it is in the plan (shared with operations).
type Step = plan.Step

type Fix struct {
	ID    string
	Title string
	Check string // the check it resolves
	// Scope of the subject: "site" (a domain), "all" (no subject — every
	// site), or "" (the result's own subject, e.g. a hestia.conf key).
	Scope   string
	Risk    Risk
	Note    string // what it changes, one line
	How     string // how it works: what each command does, what changes on disk
	Undo    string // how to reverse it ("" = nothing to undo)
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

// For returns the fixes that apply to a result: fixes that resolve it first,
// read-only diagnoses after — Enter should offer the fix when one is known.
func For(r check.Result) []Fix {
	var out []Fix
	for _, f := range all {
		if f.Check == r.Check && (f.Applies == nil || f.Applies(r)) {
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Risk != ReadOnly && out[j].Risk == ReadOnly })
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

var dumpPatterns = wpfiles.DumpPatterns

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
		How:     "How: `find` lists *.sql, *.sql.gz and wp-config backups in public_html and one directory below. Each is moved with `mv -n` (never overwrites) into private/hs-moved-<date>/, a directory Hestia never serves; subdirectory paths become a__b__file.sql so names cannot collide. Owner and dates are kept.",
		Undo:    "Undo: mv the files back from /home/<user>/web/<domain>/private/hs-moved-<date>/ to public_html. Delete them there once you are sure nothing needs them.",
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
		How:     "How: the same find-and-move for every web domain of every user, each into its own site's private/hs-moved-<date>/.",
		Undo:    "Undo: per site, mv the files back from private/hs-moved-<date>/.",
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
		ID: "debug-log", Title: "Turn debug off and move the log out", Check: "web.exposure", Scope: "site", Risk: Change,
		Note:    "Sets WP_DEBUG false (so the log stops growing), then moves wp-content/debug*.log to private/. Nothing is deleted — remove it there once read.",
		How:     "How: `wp config set WP_DEBUG false` (run as the site's user, edits wp-config.php) stops new log lines; then wp-content/debug*.log moves to private/hs-moved-<date>/ with `mv -n`.",
		Undo:    "Undo: `wp config set WP_DEBUG true --raw` in the site shell, and mv the log back if you want it in place.",
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
		ID: "debug-keep", Title: "Keep debug on: move the log out and cap its size", Check: "web.exposure", Scope: "site", Risk: Change,
		Note:    "For sites where you want the debug log: it moves to private/wp-debug.log (not served), errors stop printing to visitors, and it is rotated hourly at " + logcap.DefaultSize + " so it cannot grow without bound.",
		How:     "wp config set (as the site's user) points WP_DEBUG_LOG at /home/<user>/web/<domain>/private/wp-debug.log and sets WP_DEBUG_DISPLAY false. The current log is archived to private/hs-moved-<date>/. `hs logcap add` writes one logrotate block (shown below) to /etc/hs/logcap.conf and ensures /etc/cron.d/hs-logcap runs `logrotate` on that file hourly with its own state file — the system's daily logrotate is untouched. copytruncate keeps PHP's open handle valid; one compressed previous log is kept.",
		Undo:    "`hs logcap remove <domain>` stops the rotation; `wp config delete WP_DEBUG_LOG` puts the log back at wp-content/debug.log (served — don't).",
		Applies: func(r check.Result) bool { return strings.Contains(r.Summary, "debug") },
		Plan: func(ctx context.Context, env *check.Env, subject string) ([]Step, error) {
			return debugKeepSteps(env, subject)
		},
	})
	if k, ok := ByID("debug-keep"); ok {
		k.ID, k.Check = "debug-keep-on", "site.debug"
		k.Applies = func(r check.Result) bool { return r.State != check.OK }
		register(k)
	}
	register(Fix{
		ID: "wp-debug-off", Title: "Turn WP_DEBUG off", Check: "site.debug", Scope: "site", Risk: Change,
		Note: "wp config set WP_DEBUG false, as the site's user.",
		How:  "`wp config set WP_DEBUG false --raw`, run as the site's user — WP-CLI rewrites that one define in wp-config.php.",
		Undo: "`wp config set WP_DEBUG true --raw` in the site shell.",
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
		How:  "`find` lists .php/.phtml/.phar files under wp-content/uploads (index.php stubs excluded). Each moves with `mv -n` to /root/hs-quarantine/<date>/<domain>/ under its original path, readable by root only, so the site can no longer execute it and you can inspect it.",
		Undo: "mv a file back to the same path under public_html (the quarantine keeps the path), then chown it to the site's user.",
		Plan: func(ctx context.Context, env *check.Env, subject string) ([]Step, error) {
			d, err := domainByName(env.Sys, subject)
			if err != nil {
				return nil, err
			}
			all := find(ctx, env.Sys, d.DocRoot()+"/wp-content/uploads", "-type", "f", "(", "-iname", "*.php", "-o", "-iname", "*.phtml",
				"-o", "-iname", "*.phar", ")", "!", "-name", "index.php")
			var files []string
			for _, f := range all {
				if wpfiles.KnownUploadsPHP(strings.TrimPrefix(f, d.DocRoot()+"/wp-content/uploads/")) == "" {
					files = append(files, f) // known plugin caches (WPML twig, Sucuri…) stay
				}
			}
			return quarantineSteps(files, d), nil
		},
	})
	register(Fix{
		ID: "wp-core", Title: "Restore WordPress core files", Check: "site.core", Scope: "site", Risk: Destructive,
		Note:    "Quarantines files that should not exist in core, then re-downloads the same WordPress version over wp-admin/ and wp-includes/ (content untouched). Take a backup first if unsure.",
		How:     "How: `wp core verify-checksums` (as the site's user) lists files that differ from the official release. Files that should not exist are quarantined under their path in /root/hs-quarantine/<date>/<domain>/. Then `wp core download --force --skip-content --version=<same>` rewrites wp-admin/ and wp-includes/ plus the root core files with the release; wp-content/ and wp-config.php are not touched.",
		Undo:    "Undo: restore the files from the quarantine, or the site from backup (Site → Restore). Modified core files are overwritten, so a backup first is the safe order.",
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
		ID: "letsencrypt", Title: "Issue a Let's Encrypt certificate", Check: "web.ssl", Scope: "site", Risk: Change,
		Note: "Fails if the domain's DNS does not point here (see the site's DNS column) — then it was a stale copy, not an outage.",
		How:  "stock `v-add-letsencrypt-domain` requests a certificate for the domain and its aliases via HTTP validation, installs it and rebuilds the web config.",
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
		How:  "stock `v-change-sys-config-value KEY \"\"` empties that one key in /usr/local/hestia/conf/hestia.conf.",
		Undo: "`v-change-sys-config-value KEY <old value>`.",
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
		How:     "How: `systemctl start postfix`, then `postqueue -f` asks it to retry everything queued, then prints the unit status so you see whether it stayed up.",
		Undo:    "Undo: `systemctl stop postfix`.",
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
		How:     "How: reads Hestia's firewall rules (/usr/local/hestia/data/firewall/rules.conf) and deletes each rule opening port 19999 with stock `v-delete-firewall-rule`, which reloads iptables.",
		Undo:    "Undo: re-add it in Hestia → Server → Firewall (restrict the source IP this time).",
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
		ID: "unit-diagnose", Title: "Show why the unit failed", Check: "systemd.failed", Scope: "", Risk: ReadOnly,
		Applies: func(r check.Result) bool { return r.State == check.Fail },
		Note:    "Diagnosis only: restarting blindly hides the cause. The unit's state, exit code and last journal lines.",
		How:     "`systemctl status` for the unit (exit 3 there just means \"not running\"), then its last 30 journal lines. Changes nothing.",
		Plan: func(ctx context.Context, env *check.Env, unit string) ([]Step, error) {
			if !regexp.MustCompile(`^[A-Za-z0-9@._:-]+\.(service|timer|socket|mount)$`).MatchString(unit) {
				return nil, fmt.Errorf("not a unit name: %q", unit)
			}
			return []Step{
				{Why: unit + " status", Argv: []string{"systemctl", "--no-pager", "-n", "0", "status", unit}},
				{Why: unit + " journal", Argv: []string{"journalctl", "-u", unit, "-n", "30", "--no-pager", "-o", "short-iso"}},
			}, nil
		},
	})
	register(Fix{
		ID: "web-terminal-update", Title: "Update Hestia's web terminal package", Check: "systemd.failed", Scope: "", Risk: Change,
		Applies: func(r check.Result) bool {
			return r.Subject == "hestia-web-terminal.service" && strings.Contains(strings.Join(r.Evidence, " "), "node-pty")
		},
		Note: "hzdemolink 2026-09-25: hestia-web-terminal 1.0.3 shipped without its node_modules (node-pty), so the panel's browser terminal never started; apt already offers a newer build.",
		How:  "apt-cache policy shows the installed and candidate versions; apt-get installs the candidate (the upstream build bundles node_modules with `npm ci`); the unit is restarted and its status printed. Only this one package changes.",
		Undo: "apt-get install hestia-web-terminal=<old version> (shown by the first step).",
		Plan: func(ctx context.Context, env *check.Env, _ string) ([]Step, error) {
			return []Step{
				{Why: "installed vs available", Argv: []string{"apt-cache", "policy", "hestia-web-terminal"}},
				{Why: "install the candidate build", Argv: []string{"apt-get", "install", "-y", "hestia-web-terminal"}},
				{Why: "start it", Argv: []string{"systemctl", "restart", "hestia-web-terminal"}},
				{Why: "did it stay up", Argv: []string{"systemctl", "--no-pager", "-n", "5", "status", "hestia-web-terminal"}},
			}, nil
		},
	})
	register(Fix{
		ID: "quotacheck-reset", Title: "Clear quotacheck's failed state", Check: "systemd.failed", Scope: "", Risk: Change,
		Applies: func(r check.Result) bool {
			return r.Subject == "systemd-quotacheck.service" && r.State == check.Configured
		},
		Note: "Cosmetic: removes the unit from `systemctl --failed` until the next boot. Quotas are on; nothing is repaired because nothing is broken.",
		How:  "Hestia's v-add-sys-quota installs /etc/cron.daily/quotacheck, which touches /forcequotacheck every day; at the next boot systemd-quotacheck is forced to run, cannot remount / read-only on a live system, and fails. Journaled ext4 quotas do not need that check. `systemctl reset-failed` clears the recorded failure only; it comes back after a reboot unless the daily cron is removed (worth raising upstream).",
		Undo: "Nothing to undo.",
		Plan: func(ctx context.Context, env *check.Env, _ string) ([]Step, error) {
			return []Step{
				{Why: "quotas are on (for the record)", Argv: []string{"quotaon", "-pa"}},
				{Why: "forget the recorded boot-time failure", Argv: []string{"systemctl", "reset-failed", "systemd-quotacheck.service"}},
			}, nil
		},
	})
	register(Fix{
		ID: "storagebox-test", Title: "Test the Storage Box connection", Check: "backups.nightly", Scope: "", Risk: ReadOnly,
		Applies: func(r check.Result) bool { return strings.Contains(r.Summary, "restic unreachable") },
		Note:    "TCP to the Storage Box's port 23 from this box. Refused here but open from another box = this IP is banned for failed logins (Hetzner lifts it after a while).",
		How:     "How: reads the Storage Box host and port from /root/.config/rclone/rclone.conf and opens a TCP connection with `nc -vz` — no login, so it cannot add to a ban.",
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
		How:  "stock `v-change-web-domain-proxy-tpl user domain wp-secure` records the template in the user's web.conf and rebuilds that domain's nginx config from it, then reloads nginx.",
		Undo: "the same command with the previous template name (shown in the plan).",
		Plan: func(ctx context.Context, env *check.Env, subject string) ([]Step, error) {
			return proxySteps(env, subject, "wp-secure")
		},
	})
	register(Fix{
		ID: "proxy-wp-rocket", Title: "Put the site on the wp-rocket template", Check: "web.template-usage", Scope: "site", Risk: Change,
		Note: "Hardened + serves WP Rocket's cached pages straight from nginx. Only for sites running WP Rocket.",
		How:  "stock `v-change-web-domain-proxy-tpl user domain wp-rocket` — same as wp-secure, with the WP Rocket cache rules.",
		Undo: "the same command with the previous template name (shown in the plan).",
		Plan: func(ctx context.Context, env *check.Env, subject string) ([]Step, error) {
			return proxySteps(env, subject, "wp-rocket")
		},
	})
}

// DebugLogPath is where a deliberately kept debug log lives: the site's
// private/ — never served, inside the account and its open_basedir.
func DebugLogPath(d hestia.Domain) string {
	return "/home/" + d.User + "/web/" + d.Name + "/private/wp-debug.log"
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
func Quote(argv []string) string { return plan.Quote(argv) }

func debugKeepSteps(env *check.Env, subject string) ([]Step, error) {
	d, err := domainByName(env.Sys, subject)
	if err != nil {
		return nil, err
	}
	logPath := DebugLogPath(d)
	var steps []Step
	if _, err := env.Sys.Stat(d.DocRoot() + "/wp-content/plugins/debug-log-config-tool"); err == nil {
		steps = append(steps, Step{Why: "NOTE: this site has the debug-log-config-tool plugin, which sets its own log path and may\n" +
			"override WP_DEBUG_LOG. If a wp-content/debug-<hash>.log keeps appearing, turn its logging off or deactivate it.",
			Argv: []string{"ls", "-la", d.DocRoot() + "/wp-content/plugins/debug-log-config-tool"}})
	}
	logs, _ := env.Sys.Glob(d.DocRoot() + "/wp-content/debug*.log")
	steps = append(steps, moveSteps(logs, d.DocRoot(), private(d), d.User)...)
	steps = append(steps,
		Step{Why: "write the log to private/ from now on", Argv: wp(d, "config", "set", "WP_DEBUG_LOG", logPath)},
		Step{Why: "never print errors to visitors", Argv: wp(d, "config", "set", "WP_DEBUG_DISPLAY", "false", "--raw")},
		Step{Why: "rotate hourly at " + logcap.DefaultSize + " — adds to /etc/hs/logcap.conf:\n" +
			strings.TrimRight(logcap.Block(d.Name, logPath, d.User, logcap.DefaultSize), "\n") +
			"\nand /etc/cron.d/hs-logcap:\n" + strings.TrimRight(logcap.CronLine(), "\n"),
			Argv: []string{selfExe(), "logcap", "add", d.Name, logPath, d.User, logcap.DefaultSize}},
	)
	return steps, nil
}

func selfExe() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "hs"
}

func init() {
	fw := Fix{
		ID: "firewall-restore", Title: "Install iptables and restore Hestia's firewall", Check: "firewall", Scope: "", Risk: Change,
		Note: "soutuveneet 2026-09-24: no iptables binary — the firewall held no rules and every fail2ban ban failed.",
		How:  "apt installs the iptables package if it is missing; restarting hestia-iptables re-applies Hestia's rules.conf (the rules shown in the panel); restarting fail2ban lets its jails re-create their chains. The last step prints how many rules are now loaded.",
		Undo: "Nothing to undo — this restores the intended state. Firewall rules are edited in Hestia → Server → Firewall.",
		Plan: func(ctx context.Context, env *check.Env, _ string) ([]Step, error) {
			var steps []Step
			if !env.Sys.Have("iptables") {
				steps = append(steps, Step{Why: "the firewall binary is missing", Argv: []string{"apt-get", "install", "-y", "iptables"}})
			}
			return append(steps,
				Step{Why: "re-apply Hestia's rules", Argv: []string{"systemctl", "restart", "hestia-iptables"}},
				Step{Why: "let fail2ban re-create its chains", Argv: []string{"systemctl", "restart", "fail2ban"}},
				Step{Why: "how many rules are loaded now (expect dozens)", Argv: []string{"sh", "-c", "iptables -S | wc -l"}},
			), nil
		},
	}
	register(fw)
	fw.ID, fw.Check = "firewall-restore-f2b", "fail2ban"
	fw.Applies = func(r check.Result) bool { return strings.Contains(r.Summary, "failed") }
	register(fw)

	maldetDeps := Fix{
		ID: "maldet-deps", Title: "Install maldet's monitor dependencies and start it", Check: "maldet", Scope: "", Risk: Change,
		Applies: func(r check.Result) bool { return strings.Contains(r.Summary, "not running") },
		Note:    "Found on four boxes 2026-09-24: maldet's monitor mode needs `ed` and `inotify-tools`, and dies at start without them.",
		How:     "apt installs the two packages (small, no services of their own), then maldet.service is restarted and its status printed so you see it stayed up.",
		Undo:    "systemctl stop maldet; apt-get remove ed inotify-tools.",
		Plan: func(ctx context.Context, env *check.Env, _ string) ([]Step, error) {
			return []Step{
				{Why: "monitor mode dependencies", Argv: []string{"apt-get", "install", "-y", "ed", "inotify-tools"}},
				{Why: "start monitoring", Argv: []string{"systemctl", "restart", "maldet"}},
				{Why: "did it stay up", Argv: []string{"systemctl", "--no-pager", "-n", "5", "status", "maldet"}},
			}, nil
		},
	}
	register(maldetDeps)
	maldetDeps.ID, maldetDeps.Check = "maldet-deps-unit", "systemd.failed"
	maldetDeps.Applies = func(r check.Result) bool { return r.Subject == "maldet.service" }
	register(maldetDeps)

	register(Fix{
		ID: "apt-timers", Title: "Turn on automatic security updates", Check: "updates.unattended", Scope: "", Risk: Change,
		Applies: func(r check.Result) bool {
			return strings.Contains(r.Summary, "never run") || strings.Contains(r.Summary, "last run was")
		},
		Note: "soutuveneet 2026-09-24: unattended-upgrades installed and security-only, but it had never run — 49 security updates waiting.",
		How:  "`systemctl enable --now` starts apt's two daily timers (refresh lists, then upgrade). The last step is unattended-upgrade's own --dry-run, which changes nothing and shows what the next run will install.",
		Undo: "systemctl disable --now apt-daily.timer apt-daily-upgrade.timer.",
		Plan: func(ctx context.Context, env *check.Env, _ string) ([]Step, error) {
			return []Step{
				{Why: "daily list refresh and upgrade", Argv: []string{"systemctl", "enable", "--now", "apt-daily.timer", "apt-daily-upgrade.timer"}},
				{Why: "when they fire next", Argv: []string{"systemctl", "list-timers", "--no-pager", "apt-daily*"}},
				{Why: "what the next run will install (dry run, changes nothing)", Argv: []string{"sh", "-c", "unattended-upgrade --dry-run -v 2>&1 | tail -25"}},
			}, nil
		},
	})

	register(Fix{
		ID: "wp-core-noise", Title: "Quarantine error logs and leftovers from core directories", Check: "site.core", Scope: "site", Risk: Change,
		Applies: func(r check.Result) bool { return r.State == check.Warn && !strings.Contains(r.Summary, "incomplete") },
		Note:    "Moves PHP error logs and leftover files out of wp-admin/ and wp-includes/ — no core download needed.",
		How:     "`wp core verify-checksums` (as the site's user) lists files core does not ship; each moves with `mv -n` to /root/hs-quarantine/<date>/<domain>/ under its original path. Truncated names from an interrupted update are included; if core files were also missing, use Restore WordPress core files instead.",
		Undo:    "mv the files back from the quarantine to the same path.",
		Plan: func(ctx context.Context, env *check.Env, subject string) ([]Step, error) {
			d, err := domainByName(env.Sys, subject)
			if err != nil {
				return nil, err
			}
			out, err := env.Sys.Run(ctx, "runuser", "-u", d.User, "--", "env", "HOME=/home/"+d.User, "wp", "--path="+d.DocRoot(), "core", "verify-checksums")
			detail := out
			if ee, ok := err.(*sys.ExitError); ok {
				detail += "\n" + ee.Stderr
			}
			var files []string
			for _, l := range lines(detail) {
				if f, ok := strings.CutPrefix(l, "Warning: File should not exist: "); ok && wpfiles.ClassifyCoreExtra(f) != wpfiles.ExtraSuspicious {
					files = append(files, d.DocRoot()+"/"+f)
				}
			}
			return quarantineSteps(files, d), nil
		},
	})
}

// --- Redis: the layout the redis-cache plugin documents (internal/redisinfo)

func redisSite(env *check.Env, domain string) (redisinfo.Site, error) {
	for _, st := range redisinfo.Sites(env.Sys) {
		if st.Domain.Name == domain {
			return st, nil
		}
	}
	return redisinfo.Site{}, fmt.Errorf("%s has no Redis object cache", domain)
}

// clearOldKeys removes a site's keys without touching anyone else's: on a
// database of its own a background FLUSHDB; on a shared one SCAN + UNLINK
// by prefix (incremental, never blocks Redis the way the plugin's Lua flush
// does). A site with no prefix on a shared database cannot be told apart —
// its old keys are left to allkeys-lru eviction.
func clearOldKeys(st redisinfo.Site, why string) Step {
	db := strconv.Itoa(st.DB)
	switch {
	case len(st.SharesWith) == 0:
		return Step{Why: why + " — its own database, so a background FLUSHDB", Argv: []string{"redis-cli", "-n", db, "flushdb", "async"}}
	case st.Prefix != "":
		pat := st.Prefix + "*"
		return Step{Why: why + " — shared database: delete only keys starting with " + st.Prefix,
			Argv: []string{"sh", "-c", "redis-cli -n " + db + " --scan --pattern " + Quote([]string{pat}) +
				" | xargs -r -d '\\n' -n 500 redis-cli -n " + db + " unlink | awk '{n+=$1} END {print n+0, \"keys removed\"}'"}}
	}
	return Step{Why: why + " — no prefix on a shared database: its old keys cannot be told from the others', left to LRU eviction",
		Argv: []string{"true"}}
}

func maxTTLSteps(st redisinfo.Site) []Step {
	return []Step{
		{Why: st.Domain.Name + ": keys expire within a day", Argv: wp(st.Domain, "config", "set", "WP_REDIS_MAXTTL", "86400", "--raw", "--type=constant")},
		clearOldKeys(st, st.Domain.Name+": drop the keys written without a TTL"),
	}
}

func init() {
	register(Fix{
		ID: "redis-own-db", Title: "Give the site its own Redis database", Check: "redis.sites", Scope: "site", Risk: Change,
		Applies: func(r check.Result) bool { return r.Data["shared"] == "true" },
		Note:    "The plugin's FAQ: every site needs its own WP_REDIS_DATABASE and prefix. While sites share one, any site's `wp cache flush` (FLUSHDB) empties all of them.",
		How:     "Picks the lowest database no wp-config on the box names and that holds no keys (the same rule as v-wp-redis-install), writes WP_REDIS_DATABASE (and a prefix if missing, MAXTTL if missing) with `wp config set` as the site's user, drops SELECTIVE_FLUSH (pointless with a database of its own), then deletes the site's old keys from the shared database by prefix with SCAN + UNLINK — other sites' keys are not touched. The site's next request starts on an empty cache.",
		Undo:    "wp config delete WP_REDIS_DATABASE (back to database 0), in the site shell.",
		Plan: func(ctx context.Context, env *check.Env, subject string) ([]Step, error) {
			st, err := redisSite(env, subject)
			if err != nil {
				return nil, err
			}
			if len(st.SharesWith) == 0 {
				return nil, nil
			}
			n, ok := redisinfo.FreeDB(ctx, env.Sys)
			if !ok {
				return nil, fmt.Errorf("no free Redis database — raise `databases` in /etc/redis/redis.conf first")
			}
			d := st.Domain
			var steps []Step
			old := st
			if st.Prefix == "" {
				b := make([]byte, 4)
				rand.Read(b)
				p := redisinfo.NewPrefix(d.Name, hex.EncodeToString(b))
				steps = append(steps, Step{Why: "a prefix of its own", Argv: wp(d, "config", "set", "WP_REDIS_PREFIX", p, "--type=constant")})
			}
			steps = append(steps, Step{Why: fmt.Sprintf("database %d (was %d, shared with %s)", n, st.DB, strings.Join(st.SharesWith, ", ")),
				Argv: wp(d, "config", "set", "WP_REDIS_DATABASE", strconv.Itoa(n), "--raw", "--type=constant")})
			if st.Selective {
				steps = append(steps, Step{Why: "selective flush is unsupported and pointless with a database of its own", Argv: wp(d, "config", "delete", "WP_REDIS_SELECTIVE_FLUSH")})
			}
			if st.MaxTTL == "" || st.MaxTTL == "0" {
				steps = append(steps, Step{Why: "keys expire within a day", Argv: wp(d, "config", "set", "WP_REDIS_MAXTTL", "86400", "--raw", "--type=constant")})
			}
			return append(steps, clearOldKeys(old, "remove its keys from the old database")), nil
		},
	})
	register(Fix{
		ID: "redis-maxttl", Title: "Make the site's cache keys expire", Check: "redis.sites", Scope: "site", Risk: Change,
		Applies: func(r check.Result) bool {
			return strings.Contains(r.Summary, "never expire") && r.Data["shared"] != "true"
		},
		Note: "WP_REDIS_MAXTTL 86400: every key lives at most a day, so the keyspace stays bounded and scans stay cheap. The plugin FAQ's own answer to a growing Redis.",
		How:  "`wp config set WP_REDIS_MAXTTL 86400` as the site's user, then the keys already written without a TTL are cleared — on its own database a background FLUSHDB (`flushdb async`), which returns at once. The site runs on a cold cache for a few requests.",
		Undo: "wp config delete WP_REDIS_MAXTTL.",
		Plan: func(ctx context.Context, env *check.Env, subject string) ([]Step, error) {
			st, err := redisSite(env, subject)
			if err != nil {
				return nil, err
			}
			return maxTTLSteps(st), nil
		},
	})
	register(Fix{
		ID: "redis-maxttl-all", Title: "Make cache keys expire on every object-cache site", Check: "redis", Scope: "all", Risk: Change,
		Applies: func(r check.Result) bool { return strings.Contains(r.Summary, "TTL") },
		Note:    "WP_REDIS_MAXTTL for every site missing it, and each site's old keys cleared without touching the others'.",
		How:     "Per site without MAXTTL: `wp config set WP_REDIS_MAXTTL 86400` as its user; then its old keys go — a background FLUSHDB where it has its own database, SCAN + UNLINK by its prefix where the database is shared.",
		Undo:    "wp config delete WP_REDIS_MAXTTL per site.",
		Plan: func(ctx context.Context, env *check.Env, _ string) ([]Step, error) {
			var steps []Step
			for _, st := range redisinfo.Sites(env.Sys) {
				if st.MaxTTL == "" || st.MaxTTL == "0" {
					steps = append(steps, maxTTLSteps(st)...)
				}
			}
			return steps, nil
		},
	})
	register(Fix{
		ID: "redis-no-selective", Title: "Turn off selective flushing", Check: "redis.sites", Scope: "site", Risk: Change,
		Applies: func(r check.Result) bool {
			return strings.Contains(r.Summary, "SELECTIVE_FLUSH") && r.Data["shared"] != "true"
		},
		Note: "The plugin README lists WP_REDIS_SELECTIVE_FLUSH as unsupported (\"terribly slow Lua script\"). With a database of its own the site does not need it: a flush becomes an instant FLUSHDB of that one database.",
		How:  "`wp config delete WP_REDIS_SELECTIVE_FLUSH` as the site's user. Nothing else changes; the next flush is FLUSHDB of the site's own database instead of a scan.",
		Undo: "wp config set WP_REDIS_SELECTIVE_FLUSH true --raw --type=constant.",
		Plan: func(ctx context.Context, env *check.Env, subject string) ([]Step, error) {
			st, err := redisSite(env, subject)
			if err != nil {
				return nil, err
			}
			if len(st.SharesWith) > 0 {
				return nil, fmt.Errorf("%s shares its database — give it its own first (that fix also removes this)", subject)
			}
			return []Step{{Why: "flush becomes FLUSHDB of its own database", Argv: wp(st.Domain, "config", "delete", "WP_REDIS_SELECTIVE_FLUSH")}}, nil
		},
	})
	register(Fix{
		ID: "redis-dropin", Title: "Update the Redis drop-in", Check: "redis.sites", Scope: "site", Risk: Change,
		Applies: func(r check.Result) bool { return strings.Contains(r.Summary, "drop-in") },
		Note:    "object-cache.php is copied from the plugin when the cache is enabled and does not follow plugin updates on its own.",
		How:     "`wp redis update-dropin` as the site's user copies the installed plugin's drop-in over wp-content/object-cache.php.",
		Undo:    "Update or downgrade the plugin, then run the same command.",
		Plan: func(ctx context.Context, env *check.Env, subject string) ([]Step, error) {
			st, err := redisSite(env, subject)
			if err != nil {
				return nil, err
			}
			return []Step{{Why: fmt.Sprintf("drop-in %s → plugin %s", st.DropinVersion, st.PluginVersion), Argv: wp(st.Domain, "redis", "update-dropin")}}, nil
		},
	})
	register(Fix{
		ID: "redis-group-flush", Title: "Stop group flushes from scanning Redis", Check: "redis", Scope: "all", Risk: Change,
		Applies: func(r check.Result) bool { return strings.Contains(r.Summary, "flush scans") },
		Note:    "Only for sites with a database of their own: WP_REDIS_DISABLE_GROUP_FLUSH makes a group flush an instant FLUSHDB of that one database instead of a Lua scan that blocks every other request (kuumalahde's 504s). Sites on a shared database are skipped — for them FLUSHDB would empty their neighbours.",
		How:     "Per site with its own database and no selective flush: `wp config set WP_REDIS_DISABLE_GROUP_FLUSH true --raw --type=constant` as its user. The cost is more cache misses after a group flush (the whole site's cache instead of one group); never stale data.",
		Undo:    "wp config delete WP_REDIS_DISABLE_GROUP_FLUSH per site.",
		Plan: func(ctx context.Context, env *check.Env, _ string) ([]Step, error) {
			var steps []Step
			for _, st := range redisinfo.Sites(env.Sys) {
				switch {
				case st.GroupFlushOff:
				case len(st.SharesWith) > 0 || st.Selective:
					steps = append(steps, Step{Why: st.Domain.Name + ": skipped — needs its own database and no selective flush first", Argv: []string{"true"}})
				default:
					steps = append(steps, Step{Why: st.Domain.Name, Argv: wp(st.Domain, "config", "set", "WP_REDIS_DISABLE_GROUP_FLUSH", "true", "--raw", "--type=constant")})
				}
			}
			return steps, nil
		},
	})
	register(Fix{
		ID: "redis-lazyfree", Title: "Let Redis flush in the background", Check: "redis", Scope: "", Risk: Change,
		Applies: func(r check.Result) bool { return strings.Contains(r.Summary, "lazyfree") },
		Note:    "lazyfree-lazy-user-flush yes (Redis 6.2+, recommended in the plugin FAQ): FLUSHDB returns at once and memory is freed in the background, so a site's `wp cache flush` never stalls the other sites.",
		How:     "`redis-cli config set` applies it to the running Redis immediately (no restart, no data lost); the same line is written to /etc/redis/redis.conf so it survives a restart — replacing an existing lazyfree-lazy-user-flush line or appending one. The last step shows the running value.",
		Undo:    "redis-cli config set lazyfree-lazy-user-flush no, and set the line in /etc/redis/redis.conf back to no.",
		Plan: func(ctx context.Context, env *check.Env, _ string) ([]Step, error) {
			conf := "/etc/redis/redis.conf"
			if _, err := env.Sys.Stat(conf); err != nil {
				return nil, fmt.Errorf("%s not found", conf)
			}
			return []Step{
				{Why: "apply to the running Redis", Argv: []string{"redis-cli", "config", "set", "lazyfree-lazy-user-flush", "yes"}},
				{Why: "keep it across restarts", Argv: []string{"sh", "-c", "grep -q '^lazyfree-lazy-user-flush' " + conf +
					" && sed -i 's/^lazyfree-lazy-user-flush.*/lazyfree-lazy-user-flush yes/' " + conf +
					" || echo 'lazyfree-lazy-user-flush yes' >> " + conf + "; grep -n '^lazyfree-lazy-user-flush' " + conf}},
				{Why: "running value", Argv: []string{"redis-cli", "config", "get", "lazyfree-lazy-user-flush"}},
			}, nil
		},
	})
	register(Fix{
		ID: "redis-databases", Title: "Raise the number of Redis databases", Check: "redis.sites", Scope: "", Risk: Change,
		Applies: func(r check.Result) bool { return r.Subject == "redis databases" },
		Note:    "More object-cache sites than Redis databases. Raises `databases` to 64 in /etc/redis/redis.conf; needs a Redis restart, which empties the cache for every site on the box (Hestia's Redis is cache-only).",
		How:     "sed changes (or appends) the `databases` line in /etc/redis/redis.conf, then `systemctl restart redis-server`. Sites reconnect on their next request with a cold cache.",
		Undo:    "Set the line back and restart Redis — only after sites using databases above the old limit have been moved.",
		Plan: func(ctx context.Context, env *check.Env, _ string) ([]Step, error) {
			conf := "/etc/redis/redis.conf"
			return []Step{
				{Why: "64 databases", Argv: []string{"sh", "-c", "grep -q '^databases' " + conf + " && sed -i 's/^databases.*/databases 64/' " + conf +
					" || echo 'databases 64' >> " + conf + "; grep -n '^databases' " + conf}},
				{Why: "apply (cache restarts empty)", Argv: []string{"systemctl", "restart", "redis-server"}},
				{Why: "check", Argv: []string{"redis-cli", "config", "get", "databases"}},
			}, nil
		},
	})
}
