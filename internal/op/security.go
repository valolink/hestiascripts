package op

// Security: run.sh's Fail2ban (4), Maldet (5) and Security (7) menus.

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/check/box"
	"github.com/valolink/hestiascripts/internal/conf"
)

const (
	f2bJail     = "/etc/fail2ban/jail.d/wordpress.conf"
	f2bFilter   = "/etc/fail2ban/filter.d/wordpress.conf"
	maldetConf  = "/usr/local/maldetect/conf.maldet"
	uuFile      = "/etc/apt/apt.conf.d/50unattended-upgrades"
	sshDropIn   = "/etc/ssh/sshd_config.d/00-hs-keys-only.conf"
	uuOriginSed = `[[:space:]]*"origin=Debian[^"]*,label=Debian";`
)

var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

func repoFile(env *check.Env, rel string) (string, error) {
	p := env.RepoDir + "/" + rel
	if env.RepoDir == "" || !exists(env, p) {
		return "", fmt.Errorf("%s not found — is the hestiascripts checkout next to hs?", rel)
	}
	return p, nil
}

func jailValue(env *check.Env, key string) string {
	v, _ := conf.Get(readFile(env, f2bJail), conf.Opts{Section: "wordpress"}, key)
	return v
}

func jailField(key, label, help string, min, max int) Field {
	return Field{Key: key, Label: label, Kind: Number, Min: min, Max: max, Help: help,
		Current: func(_ context.Context, env *check.Env, _ Target) string { return jailValue(env, key) },
		Default: func(_ context.Context, env *check.Env, _ Target) string { return jailValue(env, key) }}
}

// keyLogins: root logins by key and by password in the journal (30 days).
func keyLogins(ctx context.Context, env *check.Env) (keys, passwords []string) {
	out, _ := env.Sys.Run(ctx, "journalctl", "--since=-30d", "--no-pager", "-o", "cat", "-u", "ssh", "-u", "sshd", "--grep", "Accepted (publickey|password) for root")
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(l, "Accepted publickey for root"):
			keys = append(keys, strings.TrimSpace(l))
		case strings.Contains(l, "Accepted password for root"):
			passwords = append(passwords, strings.TrimSpace(l))
		}
	}
	return
}

func init() {
	register(Op{
		ID: "f2b-wp-jail", Title: "Fail2ban WordPress jail", Section: "security", Risk: Change,
		Resolves: "fail2ban", Applies: summaryHas("no WordPress jail"),
		Note:    "Bans IPs that brute-force wp-login.php or xmlrpc.php: 10 attempts within an hour → banned for a day. Never bans this box's own addresses.",
		How:     "`hs conf install` writes the filter and jail from templates/fail2ban/ (printing what changes, keeping previous files). An empty placeholder log in /var/log/nginx/domains keeps the log glob from matching nothing (fail2ban refuses to start then). `hs f2b repair` adds ignoreip for this box's own addresses — the nginx→apache hop is logged with them, and a wp-login flood once banned the server itself and 502'd every site (2026-07-05) — turns Hestia's per-ban mails off (they flooded the production inbox and stalled fail2ban's queue), and disables any jail whose log files do not exist here; each change is printed. `hs f2b reload` reloads, killing a stuck daemon after 3 s.",
		Undo:    "rm /etc/fail2ban/jail.d/wordpress.conf /etc/fail2ban/filter.d/wordpress.conf && hs f2b reload",
		Recheck: []string{"fail2ban"},
		Plan: func(_ context.Context, env *check.Env, _ Target, _ Values) ([]Step, error) {
			if !have(env, "fail2ban-client") {
				return nil, fmt.Errorf("fail2ban is not installed (Hestia installs it; check whether it was skipped)")
			}
			filter, err := repoFile(env, "templates/fail2ban/wordpress-filter.conf")
			if err != nil {
				return nil, err
			}
			jail, _ := repoFile(env, "templates/fail2ban/wordpress-jail.conf")
			return []Step{
				confInstall("the filter: POSTs to wp-login.php and xmlrpc.php", filter, f2bFilter),
				confInstall("the jail: 10 in 1 h → 24 h ban", jail, f2bJail),
				{Why: "the log glob must match at least one file", Argv: []string{"sh", "-c", "install -d /var/log/nginx/domains && touch /var/log/nginx/domains/wordpress-watch.log"}},
				{Why: "self-ban guard, quiet ban mails, jails without logs; config test", Argv: []string{self(), "f2b", "repair"}},
				{Why: "apply", Argv: []string{self(), "f2b", "reload"}},
				{Why: "the jail is live", Argv: []string{"timeout", "5", "fail2ban-client", "status", "wordpress"}},
			}, nil
		},
	})
	register(Op{
		ID: "f2b-restart", Title: "Repair and restart Fail2ban", Section: "security", Risk: Change,
		Resolves: "fail2ban", Applies: func(r check.Result) bool {
			return strings.Contains(r.Summary, "unresponsive") || strings.Contains(r.Summary, "own addresses")
		},
		Note:    "Applies the repairs, then restarts. In-memory bans are lost; jails refill from the logs.",
		How:     "`hs f2b repair`: self-ban guard in the WordPress jail, Hestia's per-ban mails off (ban-only action — the bans are the same), the xmlrpc line in an old filter, jails whose logs are missing disabled (jail.local sections in place, others by a jail.d/zzz-disable-<jail>.conf override); every change printed with its backup. `hs f2b restart`: systemctl restart, and if that does not return in 3 s the daemon's queue is stuck (a mail action blocked on SMTP) — it is killed and started again.",
		Undo:    "Restore the files listed as “previous file kept”.",
		Recheck: []string{"fail2ban"},
		Plan: func(_ context.Context, env *check.Env, _ Target, _ Values) ([]Step, error) {
			if !have(env, "fail2ban-client") {
				return nil, fmt.Errorf("fail2ban is not installed")
			}
			return []Step{
				{Why: "repairs and config test", Argv: []string{self(), "f2b", "repair"}},
				{Why: "restart (bounded)", Argv: []string{self(), "f2b", "restart"}},
			}, nil
		},
	})
	register(Op{
		ID: "f2b-jail-limits", Title: "Fail2ban WordPress jail limits", Section: "security", Risk: Change,
		Note: "How many failed logins, within what window, earn how long a ban.",
		How:  "`hs conf set` changes maxretry, findtime and bantime in the [wordpress] section of " + f2bJail + " (before → after printed, previous file kept); `hs f2b reload` applies them.",
		Undo: "Run it again with the previous values (shown as “now”).",
		Fields: []Field{
			jailField("maxretry", "Attempts before a ban", "Failed POSTs to wp-login.php / xmlrpc.php counted per IP.", 1, 1000),
			jailField("findtime", "Window (seconds)", "Attempts older than this stop counting. 3600 catches slow, paced guessing.", 60, 604800),
			jailField("bantime", "Ban (seconds)", "86400 = a day.", 60, 31536000),
		},
		Recheck: []string{"fail2ban"},
		Plan: func(_ context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			if !exists(env, f2bJail) {
				return nil, fmt.Errorf("the WordPress jail is not set up — Fail2ban WordPress jail first")
			}
			if jailValue(env, "maxretry") == v["maxretry"] && jailValue(env, "findtime") == v["findtime"] && jailValue(env, "bantime") == v["bantime"] {
				return nil, nil
			}
			return []Step{
				confSet("the limits", f2bJail, conf.Opts{Section: "wordpress", Style: conf.INI}, "maxretry", v["maxretry"], "findtime", v["findtime"], "bantime", v["bantime"]),
				{Why: "apply", Argv: []string{self(), "f2b", "reload"}},
			}, nil
		},
	})

	register(Op{
		ID: "maldet-install", Title: "Install Maldet (Linux Malware Detect)", Section: "security", Risk: Change,
		Resolves: "maldet", Applies: summaryHas("not installed"),
		Note:    "rfxn's installer, plus ed and inotify-tools, which its monitor mode needs (missing on four boxes in the fleet sweep).",
		How:     "apt installs ed and inotify-tools; the current release tarball is downloaded to /usr/local/src and its install.sh run (it installs to /usr/local/maldetect, adds a daily cron and the maldet service); the source is removed. Configure alerts afterwards — they stay detect-only.",
		Undo:    "/usr/local/maldetect/uninstall.sh (or remove /usr/local/maldetect, /etc/cron.daily/maldet and the maldet service).",
		Recheck: []string{"maldet"},
		Plan: func(_ context.Context, env *check.Env, _ Target, _ Values) ([]Step, error) {
			return []Step{
				aptInstall("what maldet's monitor mode needs", "ed", "inotify-tools"),
				{Why: "download", Argv: []string{"sh", "-c", "cd /usr/local/src && wget -q -O maldetect-current.tar.gz https://www.rfxn.com/downloads/maldetect-current.tar.gz && tar -xzf maldetect-current.tar.gz"}},
				{Why: "install", Argv: []string{"sh", "-c", "cd \"$(find /usr/local/src -maxdepth 1 -name 'maldetect-[0-9]*' -type d | head -1)\" && bash install.sh"}},
				{Why: "tidy", Argv: []string{"sh", "-c", "rm -rf /usr/local/src/maldetect-[0-9]* /usr/local/src/maldetect-current.tar.gz"}},
				{Why: "check", Argv: []string{"/usr/local/maldetect/maldet", "--version"}},
			}, nil
		},
	})
	register(Op{
		ID: "maldet-alerts", Title: "Maldet alerts (detect-only)", Section: "security", Risk: Change,
		Note: "Hits are mailed and left in place — never quarantined or cleaned automatically. LMD's signatures false-positive on legitimate WordPress files, and an unattended quarantine can pull a file from under PHP mid-update and take a customer site down at any hour. Review the alert, then `maldet -q SCANID` on purpose (`maldet -s SCANID` restores).",
		How:  "`hs conf set` in " + maldetConf + ": email_alert=\"1\", email_addr, quarantine_hits=\"0\", quarantine_clean=\"0\" — before → after printed, previous file kept.",
		Undo: "Set email_alert=\"0\" the same way.",
		Fields: []Field{{Key: "email", Label: "Alert address", Kind: Text, Pattern: emailRe,
			Current: func(_ context.Context, env *check.Env, _ Target) string {
				v, _ := conf.Get(readFile(env, maldetConf), conf.Opts{}, "email_addr")
				return v
			},
			Default: func(_ context.Context, env *check.Env, _ Target) string {
				v, _ := conf.Get(readFile(env, maldetConf), conf.Opts{}, "email_addr")
				if v == "you@domain.com" {
					return ""
				}
				return v
			}}},
		Recheck: []string{"maldet"},
		Plan: func(_ context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			if !exists(env, maldetConf) {
				return nil, fmt.Errorf("maldet is not installed")
			}
			return []Step{confSet("alert, detect only", maldetConf, conf.Opts{Style: conf.Shell},
				"email_alert", "1", "email_addr", v["email"], "quarantine_hits", "0", "quarantine_clean", "0")}, nil
		},
	})
	register(Op{
		ID: "maldet-scan", Title: "Scan every site for malware now", Section: "security", Risk: Change,
		Resolves: "maldet", Applies: summaryHas("never completed a scan"),
		Note: "Minutes of CPU and disk on a box with many sites; output streams here. Detect-only: nothing is moved (unless someone turned quarantine on — the plan shows the setting).",
		How:  "`maldet -a /home/?/web/?/public_html` — maldet expands the ? wildcards itself, so every site is one scan (run.sh passed a shell glob, and maldet scanned only the first path). The report is under /usr/local/maldetect/sess/; `maldet --report SCANID` shows it again.",
		Undo: "Nothing changes on disk except maldet's own report.",
		Plan: func(_ context.Context, env *check.Env, _ Target, _ Values) ([]Step, error) {
			if !exists(env, "/usr/local/maldetect/maldet") {
				return nil, fmt.Errorf("maldet is not installed")
			}
			q, _ := conf.Get(readFile(env, maldetConf), conf.Opts{}, "quarantine_hits")
			why := "detect-only (quarantine_hits=" + q + ")"
			if q == "1" {
				why = "WARNING: quarantine_hits=1 — hits WILL be moved out of the sites (Maldet alerts puts it back to detect-only)"
			}
			return []Step{{Why: why, Argv: []string{"/usr/local/maldetect/maldet", "-a", "/home/?/web/?/public_html"}}}, nil
		},
	})

	register(Op{
		ID: "unattended", Title: "Automatic security updates", Section: "security", Risk: Change,
		Resolves: "updates.unattended",
		Note:     "unattended-upgrades applying Debian security updates daily. The PHP, nginx, MariaDB and Hestia stack is left for a planned upgrade.",
		How:      "apt installs unattended-upgrades and apt-listchanges if missing. `hs conf install` writes /etc/apt/apt.conf.d/20auto-upgrades from templates/apt/ (the two lines `dpkg-reconfigure -plow unattended-upgrades` would write — no dialog). The scope is the Debian origin line in 50unattended-upgrades: security-only comments it out with `//`, all uncomments it (sed). The apt timers are enabled and listed.",
		Undo:     "systemctl disable --now apt-daily-upgrade.timer, or set both lines in 20auto-upgrades to \"0\".",
		Fields: []Field{{Key: "scope", Label: "Apply", Kind: Choice,
			Current: func(ctx context.Context, env *check.Env, _ Target) string { return box.UnattendedScope(ctx, env) },
			Choices: func(context.Context, *check.Env, Target) [][2]string {
				return [][2]string{{"security", "security updates only"}, {"all", "all Debian updates — can restart PHP, nginx, MariaDB unattended"}}
			}}},
		Recheck: []string{"updates.unattended"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			tpl, err := repoFile(env, "templates/apt/20auto-upgrades")
			if err != nil {
				return nil, err
			}
			var steps []Step
			if !box.PkgInstalled(ctx, env.Sys, "unattended-upgrades") {
				steps = append(steps, aptUpdate(), aptInstall("the package", "unattended-upgrades", "apt-listchanges"))
			}
			steps = append(steps, confInstall("run daily", tpl, "/etc/apt/apt.conf.d/20auto-upgrades"))
			switch cur := box.UnattendedScope(ctx, env); {
			case v["scope"] == "security" && cur == "security-only", v["scope"] == "all" && cur == "all":
				// scope already right
			case v["scope"] == "security":
				steps = append(steps, Step{Why: "security only: comment out the plain Debian origin", Argv: []string{"sed", "-i", `s|^\(` + uuOriginSed + `\)$|//\1|`, uuFile}})
			default:
				steps = append(steps, Step{Why: "all Debian updates: uncomment the plain Debian origin", Argv: []string{"sed", "-i", `s|^//\(` + uuOriginSed + `\)$|\1|`, uuFile}})
			}
			return append(steps,
				Step{Why: "origins now", Argv: []string{"sh", "-c", `grep -n 'origin=Debian' ` + uuFile}},
				Step{Why: "the timers that run it", Argv: []string{"systemctl", "enable", "--now", "apt-daily.timer", "apt-daily-upgrade.timer"}},
				Step{Argv: []string{"systemctl", "list-timers", "--no-pager", "apt-daily*"}},
			), nil
		},
	})
	register(Op{
		ID: "swap", Title: "Create a swapfile", Section: "system", Risk: Change,
		Resolves: "memory.swap", Applies: summaryHas("no swap"),
		Note: "Without swap the kernel OOM-kills at once instead of degrading.",
		How:  "fallocate reserves /swapfile, chmod 600, mkswap, swapon; one /swapfile line in /etc/fstab (added only if absent) brings it back at boot.",
		Undo: "swapoff /swapfile; remove its /etc/fstab line; rm /swapfile.",
		Fields: []Field{{Key: "size", Label: "Size", Kind: Text, Pattern: sizeRe, Help: "e.g. 2G or 1024M.",
			Current: func(ctx context.Context, env *check.Env, _ Target) string {
				out, _ := env.Sys.Run(ctx, "swapon", "--show", "--noheadings")
				if strings.TrimSpace(out) == "" {
					return "no swap · RAM " + mb(box.RAMMB(env.Sys))
				}
				return strings.Join(strings.Fields(out), " ")
			},
			Default: func(_ context.Context, env *check.Env, _ Target) string {
				if box.RAMMB(env.Sys) <= 4096 {
					return "2G"
				}
				return "1G"
			}}},
		Recheck: []string{"memory.swap"},
		Plan: func(_ context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			if exists(env, "/swapfile") {
				return nil, fmt.Errorf("/swapfile already exists — resizing means swapoff and recreating it, which is not automated")
			}
			return []Step{
				{Why: "reserve " + v["size"], Argv: []string{"fallocate", "-l", v["size"], "/swapfile"}},
				{Argv: []string{"chmod", "600", "/swapfile"}},
				{Argv: []string{"mkswap", "/swapfile"}},
				{Why: "use it now", Argv: []string{"swapon", "/swapfile"}},
				{Why: "and at boot", Argv: []string{"sh", "-c", "grep -q '^/swapfile ' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab; grep '^/swapfile ' /etc/fstab"}},
				{Argv: []string{"swapon", "--show"}},
			}, nil
		},
	})
	register(Op{
		ID: "ssh-keys-only", Title: "SSH: keys only, no passwords", Section: "security", Risk: Destructive,
		Resolves: "ssh.auth",
		Note:     "Refused unless root has authorized keys and has logged in with one in the last 30 days — and not if this session came in with a password. Keep this session open until a NEW key login works.",
		How:      "A drop-in, " + sshDropIn + " (PasswordAuthentication no, KbdInteractiveAuthentication no), is read before every other sshd_config.d file, so a cloud image's `PasswordAuthentication yes` there cannot override it (run.sh edited sshd_config, which those drop-ins silently beat). `sshd -t` must accept the config before `systemctl reload ssh`; open sessions are not affected. The last step prints the effective settings from `sshd -T`.",
		Undo:     "rm " + sshDropIn + " && systemctl reload ssh (from the open session, or the provider's console).",
		Recheck:  []string{"ssh.auth"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, _ Values) ([]Step, error) {
			ak := readFile(env, "/root/.ssh/authorized_keys")
			n := len(regexp.MustCompile(`(?m)^\s*(ssh-(ed25519|rsa|dss)|ecdsa-sha2-\S+|sk-\S+)\s`).FindAllString(ak, -1))
			if n == 0 {
				return nil, fmt.Errorf("refused: /root/.ssh/authorized_keys holds no key — this would lock root out")
			}
			keys, pws := keyLogins(ctx, env)
			if len(keys) == 0 {
				return nil, fmt.Errorf("refused: no root login with a key in the last 30 days of the journal — log in with your key once, then try again")
			}
			if c := strings.Fields(os.Getenv("SSH_CONNECTION")); len(c) >= 2 {
				for _, p := range pws {
					if strings.Contains(p, "from "+c[0]+" port "+c[1]) {
						return nil, fmt.Errorf("refused: this session logged in with a password (%s)", p)
					}
				}
			}
			if !strings.Contains(readFile(env, "/etc/ssh/sshd_config"), "sshd_config.d/*.conf") {
				return nil, fmt.Errorf("sshd_config does not Include sshd_config.d/*.conf — edit it by hand")
			}
			evidence := fmt.Sprintf("preflight: %d key(s) in /root/.ssh/authorized_keys; last key login: %s", n, keys[len(keys)-1])
			return []Step{
				{Why: evidence + "\nthe drop-in", Argv: []string{"sh", "-c", "printf '# hs: key-only SSH (hs op ssh-keys-only)\\nPasswordAuthentication no\\nKbdInteractiveAuthentication no\\n' > " + sshDropIn + " && cat " + sshDropIn}},
				{Why: "sshd accepts the config", Argv: []string{"sshd", "-t"}},
				{Why: "apply (open sessions stay)", Argv: []string{"systemctl", "reload", "ssh"}},
				{Why: "effective settings", Argv: []string{"sh", "-c", "sshd -T | grep -E '^(passwordauthentication|kbdinteractiveauthentication|pubkeyauthentication) '"}},
			}, nil
		},
	})
}
