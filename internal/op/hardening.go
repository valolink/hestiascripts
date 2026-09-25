package op

// Box hardening (docs/security-hardening.md §10, §11, §14): log retention,
// an audit trail, no programs from /tmp, PHP without shell functions,
// phpMyAdmin only for admin networks.

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/check/box"
	"github.com/valolink/hestiascripts/internal/conf"
)

const (
	journaldDropIn = "/etc/systemd/journald.conf.d/hs-retention.conf"
	auditRules     = "/etc/audit/rules.d/hs.rules"
	aptTmpConf     = "/etc/apt/apt.conf.d/50hs-tmp"
)

// AuditRules: a minimal trail, not a full-disk watch (§11).
func AuditRules(env *check.Env) string {
	lines := []string{
		"## hs: audit trail (hs op audit, docs/security-hardening.md §11). Keys start with hs_; search with ausearch -k hs_…",
		"-w /etc/systemd/system -p wa -k hs_units",
		"-w /usr/lib/systemd/system -p wa -k hs_units",
		"-w /etc/ld.so.preload -p wa -k hs_preload",
		"-w /etc/cron.d -p wa -k hs_cron",
		"-w /etc/crontab -p wa -k hs_cron",
		"-w /var/spool/cron -p wa -k hs_cron",
		"-w /etc/passwd -p wa -k hs_accounts",
		"-w /etc/shadow -p wa -k hs_accounts",
		"-w /etc/sudoers -p wa -k hs_accounts",
		"-w /etc/sudoers.d -p wa -k hs_accounts",
		"-w /root/.ssh -p wa -k hs_keys",
	}
	homes, _ := env.Sys.Glob("/home/*/.ssh")
	for _, h := range homes {
		lines = append(lines, "-w "+h+" -p wa -k hs_keys")
	}
	// No "exec from /tmp" rule: `-F dir=/tmp` with execve, and `-w /tmp -p x`,
	// both loaded but recorded nothing on Debian 12 (tested on hzdemolink
	// 2026-09-25, while a plain execve rule did record the same exec). Running
	// programs from /tmp is covered by tmp-noexec and the persist.processes check.
	lines = append(lines, "-a always,exit -F arch=b64 -S init_module,finit_module,delete_module -k hs_modules")
	return strings.Join(lines, "\n") + "\n"
}

func init() {
	register(Op{
		ID: "log-retention", Title: "Keep logs long enough to answer questions", Section: "security", Risk: Change,
		Resolves: "logs.retention",
		Note:     "Web and panel logs kept for half a year (rotated weekly, compressed), the journal for up to a year within a size cap. Without it an intrusion noticed after a month cannot be traced back to its start.",
		How:      "A journald drop-in, " + journaldDropIn + " (Storage=persistent, SystemMaxUse, MaxRetentionSec=1year), then journald restarts (nothing is lost). `hs conf set` changes `rotate` in /etc/logrotate.d/nginx, apache2 and hestia — in place, because a second logrotate file for the same logs is a logrotate error; each change printed, previous files kept. The last step checks the logrotate configuration.",
		Undo:     "rm " + journaldDropIn + " && systemctl restart systemd-journald; set rotate back (previous files kept under /var/lib/hs/backups).",
		Fields: []Field{
			{Key: "journal-max", Label: "Journal size cap", Kind: Text, Pattern: sizeRe, Help: "e.g. 2G. Never below what the journal uses today, or history is deleted to fit.",
				Current: func(ctx context.Context, env *check.Env, _ Target) string {
					out, _ := env.Sys.Run(ctx, "journalctl", "--disk-usage")
					return strings.TrimSpace(out)
				},
				Default: func(context.Context, *check.Env, Target) string { return "2G" }},
			{Key: "weeks", Label: "Web and panel logs (weeks)", Kind: Number, Min: 4, Max: 104,
				Default: func(context.Context, *check.Env, Target) string { return strconv.Itoa(box.RetentionWeeks) }},
		},
		Recheck: []string{"logs.retention"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			out, _ := env.Sys.Run(ctx, "journalctl", "--disk-usage")
			if m := regexp.MustCompile(`([0-9.]+)([KMGT])`).FindStringSubmatch(out); m != nil {
				f, _ := strconv.ParseFloat(m[1], 64)
				used := int64(f * float64(map[string]int64{"K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40}[m[2]]))
				if used > sizeBytes(v["journal-max"]) {
					return nil, fmt.Errorf("the journal already uses %s%s — a %s cap would delete that history to fit; choose at least that", m[1], m[2], v["journal-max"])
				}
			}
			text := "# hs: journal retention (hs op log-retention)\n[Journal]\nStorage=persistent\nSystemMaxUse=" + v["journal-max"] + "\nMaxRetentionSec=1year\n"
			steps := []Step{
				{Why: "journald: persistent, capped at " + v["journal-max"] + ", at most a year", Argv: []string{"sh", "-c", `install -d /etc/systemd/journald.conf.d && printf '%s' "$1" > ` + journaldDropIn + ` && cat ` + journaldDropIn, "sh", text}},
				{Argv: []string{"systemctl", "restart", "systemd-journald"}},
			}
			for _, f := range box.LogrotateFiles {
				if exists(env, f) {
					steps = append(steps, confSet(f, f, conf.Opts{Style: conf.Space}, "rotate", v["weeks"]))
				}
			}
			return append(steps, Step{Why: "logrotate accepts the configuration", Argv: []string{"sh", "-c", "logrotate -d /etc/logrotate.conf >/dev/null 2>&1 && echo 'logrotate configuration OK'"}}), nil
		},
	})
	register(Op{
		ID: "audit", Title: "Audit trail (auditd)", Section: "security", Risk: Change,
		Resolves: "audit.trail",
		Note:     "Records who changed systemd units, cron, accounts, sudoers and authorized_keys, and kernel module loads — the \"who ran what, when\" that bash history only half answered in May 2026. A small rule set, not a whole-disk watch.",
		How:      "apt installs auditd; the rules go to " + auditRules + " (listed in the plan; each authorized_keys directory by name); `augenrules --load` activates them now and at boot. Search with `ausearch -k hs_units` (hs_cron, hs_keys, hs_accounts, hs_modules, hs_preload) — in a script, add `--input /var/log/audit/audit.log` (without a terminal ausearch reads stdin); logs in /var/log/audit/. Programs run from /tmp are not audited (the directory rule records nothing on Debian 12): tmp-noexec and the Suspicious processes check cover that.",
		Undo:     "rm " + auditRules + " && augenrules --load (or apt purge auditd).",
		Recheck:  []string{"audit.trail"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, _ Values) ([]Step, error) {
			var steps []Step
			if !have(env, "auditctl") {
				steps = append(steps, aptInstall("the audit daemon", "auditd"))
			}
			rules := AuditRules(env)
			return append(steps,
				Step{Why: "the rules:\n" + strings.TrimSpace(rules), Argv: []string{"sh", "-c", `install -d /etc/audit/rules.d && printf '%s' "$1" > ` + auditRules, "sh", rules}},
				Step{Why: "load them", Argv: []string{"augenrules", "--load"}},
				Step{Why: "active", Argv: []string{"sh", "-c", "auditctl -l | grep -c hs_ | sed 's/$/ hs rules loaded/'"}},
			), nil
		},
	})
	register(Op{
		ID: "tmp-noexec", Title: "No programs from /tmp and /dev/shm", Section: "security", Risk: Change,
		Resolves: "tmp.noexec",
		Note:     "Mounts /tmp and /dev/shm noexec,nosuid,nodev — automated attack tooling (the May 2026 /tmp/a.elf) fails there. /dev/shm changes now; /tmp becomes its own RAM-backed filesystem at the next reboot. apt is pointed at /var/tmp for the one thing it runs from a temp dir.",
		How:      "`mount -o remount,noexec,nosuid,nodev /dev/shm` now, and an fstab line for it; an fstab line `tmpfs /tmp tmpfs defaults,noexec,nosuid,nodev,size=…,mode=1777` used from the next boot (mounting over /tmp live would hide files programs have open there); " + aptTmpConf + " sets APT::ExtractTemplates::TempDir to /var/tmp. The fstab changes are appended only if /tmp and /dev/shm have no line yet.",
		Undo:     "Remove the fstab lines after the `# hs: noexec` comments and " + aptTmpConf + "; `mount -o remount,exec /dev/shm`.",
		Fields: []Field{{Key: "size", Label: "/tmp size", Kind: Text, Pattern: sizeRe, Help: "RAM-backed; used only as files are written. 1G suits a web box.",
			Default: func(context.Context, *check.Env, Target) string { return "1G" }}},
		Recheck: []string{"tmp.noexec"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			fstab := readFile(env, "/etc/fstab")
			has := func(t string) bool {
				for _, l := range strings.Split(fstab, "\n") {
					f := strings.Fields(l)
					if len(f) >= 2 && !strings.HasPrefix(f[0], "#") && f[1] == t {
						return true
					}
				}
				return false
			}
			steps := []Step{{Why: "/dev/shm now", Argv: []string{"mount", "-o", "remount,noexec,nosuid,nodev", "/dev/shm"}}}
			if !has("/dev/shm") {
				steps = append(steps, Step{Why: "/dev/shm at boot", Argv: []string{"sh", "-c", "printf '# hs: noexec (hs op tmp-noexec)\\ntmpfs /dev/shm tmpfs defaults,noexec,nosuid,nodev 0 0\\n' >> /etc/fstab"}})
			}
			if has("/tmp") {
				steps = append(steps, Step{Why: "/tmp already has an fstab line — change its options by hand", Argv: []string{"grep", "-n", " /tmp ", "/etc/fstab"}})
			} else {
				steps = append(steps, Step{Why: "/tmp from the next boot", Argv: []string{"sh", "-c", `printf '# hs: noexec (hs op tmp-noexec)\ntmpfs /tmp tmpfs defaults,noexec,nosuid,nodev,size=%s,mode=1777 0 0\n' "$1" >> /etc/fstab`, "sh", v["size"]}})
			}
			return append(steps,
				Step{Why: "apt extracts its package scripts to /var/tmp instead", Argv: []string{"sh", "-c", `echo 'APT::ExtractTemplates::TempDir "/var/tmp";' > ` + aptTmpConf}},
				Step{Why: "fstab still parses", Argv: []string{"findmnt", "--verify", "--tab-file", "/etc/fstab"}},
			), nil
		},
	})
	register(Op{
		ID: "php-functions", Title: "PHP-FPM: disable shell functions", Section: "security", Risk: Change,
		Resolves: "php.functions",
		Note:     "Adds exec, system, passthru, shell_exec, proc_open, popen and pcntl_exec to disable_functions in each FPM php.ini that lacks them — a webshell or vulnerable plugin can no longer run commands. Hestia sets the same for versions it installs. Plugins that call external programs (some image optimisers, some backup plugins) stop working; WP-CLI is unaffected (it uses the CLI php.ini).",
		How:      "`hs conf set` rewrites the disable_functions line of /etc/php/<ver>/fpm/php.ini with the existing list plus the missing functions (before → after printed, previous file kept); `systemctl try-reload-or-restart php<ver>-fpm` applies it gracefully.",
		Undo:     "cp -p the kept php.ini back and reload php<ver>-fpm.",
		Recheck:  []string{"php.functions"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, _ Values) ([]Step, error) {
			var steps []Step
			for _, ver := range fpmVersions(env) {
				ini := "/etc/php/" + ver + "/fpm/php.ini"
				cur, _ := conf.Get(readFile(env, ini), conf.Opts{}, "disable_functions")
				have := map[string]bool{}
				var list []string
				for _, f := range strings.Split(cur, ",") {
					if f = strings.TrimSpace(f); f != "" && !have[f] {
						have[f] = true
						list = append(list, f)
					}
				}
				var add []string
				for _, f := range box.ShellFunctions {
					if !have[f] {
						add = append(add, f)
						list = append(list, f)
					}
				}
				if len(add) == 0 {
					continue
				}
				steps = append(steps,
					confSet("PHP "+ver+": also disable "+strings.Join(add, ", "), ini, conf.Opts{Style: conf.INI, Comment: ";"}, "disable_functions", strings.Join(list, ",")),
					Step{Argv: []string{"systemctl", "try-reload-or-restart", "php" + ver + "-fpm"}})
			}
			return steps, nil
		},
	})
	register(Op{
		ID: "pma-restrict", Title: "phpMyAdmin only from admin networks", Section: "security", Risk: Change,
		Resolves: "web.phpmyadmin",
		Note:     "phpMyAdmin answers at /phpmyadmin on every site domain; its login is a database-password prompt anyone can brute-force. This lets only the listed networks reach it (everyone else gets 403).",
		How:      "`hs conf block` puts `allow …; deny all;` at the top of `location /phpmyadmin` in /etc/nginx/conf.d/phpmyadmin.inc (included in every domain) and `Require ip …` in phpMyAdmin's <Directory> in /etc/apache2/conf.d/phpmyadmin.inc, between `# hs:pma` markers so re-running replaces them; previous files kept. `nginx -t` / `apache2ctl configtest` must pass before each reload. Hestia rewrites these files only when the phpMyAdmin alias is changed — the phpMyAdmin check notices if the rules are gone.",
		Undo:     "hs conf block FILE '<anchor>' hs:pma   (no lines removes the block), then reload nginx/apache2.",
		Fields: []Field{{Key: "sources", Label: "Allowed from", Kind: Text, Help: "Space-separated addresses/ranges (IPv4 or IPv6), e.g. the office, 100.64.0.0/10 for Tailscale.",
			Default: func(_ context.Context, env *check.Env, _ Target) string {
				if hasTailscale(env) {
					return "100.64.0.0/10"
				}
				return ""
			}}},
		Recheck: []string{"web.phpmyadmin"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			nets, err := parseCIDRs(v["sources"])
			if err != nil {
				return nil, err
			}
			if len(nets) == 0 {
				return nil, fmt.Errorf("list at least one allowed address or range")
			}
			var ngx, ap []string
			var ips []string
			for _, n := range nets {
				ngx = append(ngx, "allow "+n.String()+";")
				ips = append(ips, n.String())
			}
			ngx = append(ngx, "deny all;")
			ap = append(ap, "Require ip "+strings.Join(ips, " "))
			var steps []Step
			if f := "/etc/nginx/conf.d/phpmyadmin.inc"; exists(env, f) {
				steps = append(steps,
					Step{Why: "nginx: only " + strings.Join(ips, ", "), Argv: append([]string{self(), "conf", "block", f, `^\s*location /phpmyadmin \{`, box.PMAMarker}, ngx...)},
					Step{Why: "nginx accepts it", Argv: []string{"nginx", "-t"}},
					Step{Argv: []string{"systemctl", "reload", "nginx"}})
			}
			if f := "/etc/apache2/conf.d/phpmyadmin.inc"; exists(env, f) {
				steps = append(steps,
					Step{Why: "apache: the same", Argv: append([]string{self(), "conf", "block", f, `^\s*<Directory /usr/share/phpmyadmin>`, box.PMAMarker}, ap...)},
					Step{Why: "apache accepts it", Argv: []string{"apache2ctl", "configtest"}},
					Step{Argv: []string{"systemctl", "reload", "apache2"}})
			}
			if len(steps) == 0 {
				return nil, fmt.Errorf("phpMyAdmin's include files are not here")
			}
			return steps, nil
		},
	})
}
