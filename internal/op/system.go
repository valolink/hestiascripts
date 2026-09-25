package op

// System: run.sh's Disk (14) and Maintenance (13) menus, and the package
// updates. Cleanups list exactly what they will empty (with sizes) in the
// plan; a live log is truncated in place, never unlinked — nginx and PHP-FPM
// hold it open, and an unlinked open file frees no disk until they reload.

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/valolink/hestiascripts/internal/aptinfo"
	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/check/box"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/logcap"
)

func human(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%d B", b)
}

func duBytes(ctx context.Context, env *check.Env, path string) int64 {
	out, _ := env.Sys.Run(ctx, "du", "-sb", path)
	n, _ := strconv.ParseInt(strings.Fields(out + " 0")[0], 10, 64)
	return n
}

func dirs(env *check.Env, pattern string) []string {
	m, _ := env.Sys.Glob(pattern)
	var out []string
	for _, d := range m {
		if st, err := env.Sys.Stat(d); err == nil && st.IsDir() {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

type logFile struct {
	Path string
	Size int64
}

// bigLogs: log-ish files ≥ minMB in site roots and Hestia's per-domain log
// directories, plus WP_DEBUG_LOG paths. Loose name patterns on purpose —
// plugins invent their own log names. Largest first.
func bigLogs(ctx context.Context, env *check.Env, minMB int, rotated bool) []logFile {
	roots := append(dirs(env, "/home/*/web/*/public_html"), dirs(env, "/home/*/web/*/private")...)
	size := fmt.Sprintf("+%dM", minMB)
	var raw string
	names := []string{"(", "-name", "*.log", "-o", "-name", "*.log.*", "-o", "-name", "*_log", "-o", "-name", "*-log",
		"-o", "-name", "error_log", "-o", "-name", "php_errorlog", "-o", "-name", "*.err", "-o", "-path", "*/logs/*", ")"}
	if len(roots) > 0 {
		args := append(append(append([]string{}, roots...), "-maxdepth", "4", "-xdev", "-type", "f"), names...)
		out, _ := env.Sys.Run(ctx, "find", append(args, "-size", size, "-printf", `%s\t%p\n`)...)
		raw += out
	}
	if lr := append(dirs(env, "/var/log/nginx/domains"), dirs(env, "/var/log/apache2/domains")...); len(lr) > 0 {
		out, _ := env.Sys.Run(ctx, "find", append(lr, "-maxdepth", "1", "-xdev", "-type", "f", "-size", size, "-printf", `%s\t%p\n`)...)
		raw += out
	}
	if rotated {
		out, _ := env.Sys.Run(ctx, "find", "/var/log", "-xdev", "-type", "f", "(", "-name", "*.gz", "-o", "-name", "*.[0-9]", "-o", "-name", "*.[0-9].log", "-o", "-name", "*.old", ")", "-size", size, "-printf", `%s\t%p\n`)
		raw += out
	}
	seen := map[string]bool{}
	var out []logFile
	for _, l := range strings.Split(raw, "\n") {
		sz, p, ok := strings.Cut(l, "\t")
		if !ok || seen[p] {
			continue
		}
		seen[p] = true
		n, _ := strconv.ParseInt(sz, 10, 64)
		if isRotated(p) == rotated {
			out = append(out, logFile{p, n})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Size > out[j].Size })
	return out
}

func isRotated(p string) bool {
	b := filepath.Base(p)
	if strings.HasSuffix(b, ".gz") || strings.HasSuffix(b, ".old") || strings.HasSuffix(b, ".zip") {
		return true
	}
	ext := strings.TrimPrefix(filepath.Ext(strings.TrimSuffix(b, ".log")), ".")
	_, err := strconv.Atoi(ext)
	return err == nil && ext != ""
}

// siteOf: the domain and owner of a path under /home/<user>/web/<domain>/.
func siteOf(p string) (user, domain string) {
	parts := strings.Split(p, "/")
	if len(parts) > 5 && parts[1] == "home" && parts[3] == "web" {
		return parts[2], parts[4]
	}
	return "", ""
}

type service struct {
	Label, Key string
	Pkgs       []string // any installed = present (clamav-daemon without the clamav CLI)
	Values     []string // hestia.conf values that mean this service
	Purge      []string
	Units      []string
	Impact     string
	Exim       []string // sed expressions commenting out its lines in Exim's template
}

var services = []service{
	{"Dovecot (IMAP/POP3)", "IMAP_SYSTEM", []string{"dovecot-core"}, []string{"dovecot"},
		[]string{"dovecot-core", "dovecot-imapd", "dovecot-pop3d", "dovecot-lmtpd"}, []string{"dovecot"},
		"Mailboxes can no longer be read over IMAP/POP3, and Exim SMTP AUTH stops (clients sending through this box). Fine when all mail goes via Resend.", nil},
	{"ClamAV (antivirus)", "ANTIVIRUS_SYSTEM", []string{"clamav-daemon", "clamav", "clamav-base"}, []string{"clamav-daemon", "clamav", "clamd"},
		[]string{"clamav", "clamav-daemon", "clamav-freshclam", "clamav-base"}, []string{"clamav-daemon", "clamav-freshclam"},
		"Incoming mail is no longer virus-scanned. Frees ~1.3 GB of resident memory. Exim's AV scanner lines are commented out and Exim restarted.",
		[]string{`s/^\(av_scanner\s*=.*\)$/#\1/`, `s/^\(\s*deny\s\+malware\s*=.*\)$/#\1/`, `s/^\(\s*message\s*=.*[Vv]irus.*\)$/#\1/`}},
	{"SpamAssassin", "ANTISPAM_SYSTEM", []string{"spamassassin"}, []string{"spamassassin", "spamd"},
		[]string{"spamassassin", "spamc"}, []string{"spamassassin"},
		"Incoming mail is no longer spam-scored. Exim's spamd lines are commented out and Exim restarted.",
		[]string{`s/^\(spamd_address\s*=.*\)$/#\1/`, `s/^\(\s*warn\s\+spam\s*=.*\)$/#\1/`, `s/^\(\s*add header.*X-Spam.*\)$/#\1/`}},
	{"vsftpd (plain FTP)", "FTP_SYSTEM", []string{"vsftpd"}, []string{"vsftpd"},
		[]string{"vsftpd"}, []string{"vsftpd"},
		"Clients using plain FTP lose access; SFTP (over SSH) keeps working.", nil},
}

func init() {
	cacheHow := "Each directory is emptied with `find DIR -mindepth 1 -delete` (the directory itself stays). Caches rebuild on the next requests; the plan lists every directory and its size."
	register(Op{
		ID: "disk-apt", Title: "Clean the apt package cache", Section: "system", Risk: Change,
		Note:    "Downloaded .deb files apt keeps after installing. Re-downloaded if ever needed.",
		How:     "`apt-get clean` empties /var/cache/apt/archives; `apt-get autoclean` removes stale package lists entries.",
		Undo:    "Nothing to undo — apt downloads packages again when it needs them.",
		Recheck: []string{"disk"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, _ Values) ([]Step, error) {
			return []Step{
				{Why: "downloaded packages: " + human(duBytes(ctx, env, "/var/cache/apt/archives")), Argv: []string{"apt-get", "clean"}},
				{Argv: []string{"apt-get", "autoclean", "-y"}},
			}, nil
		},
	})
	register(Op{
		ID: "disk-journal", Title: "Vacuum the systemd journal", Section: "system", Risk: Change,
		Note: "Deletes journal entries older than the chosen age.",
		How:  "`journalctl --vacuum-time` removes archived journal files older than the limit; the active file stays. The last step shows the usage after.",
		Undo: "None — older journal entries are gone (application logs in /var/log are untouched).",
		Fields: []Field{{Key: "keep", Label: "Keep", Kind: Choice,
			Current: func(ctx context.Context, env *check.Env, _ Target) string {
				out, _ := env.Sys.Run(ctx, "journalctl", "--disk-usage")
				return strings.TrimSpace(out)
			},
			Choices: func(context.Context, *check.Env, Target) [][2]string {
				return [][2]string{{"2weeks", "two weeks"}, {"1week", "one week"}, {"4weeks", "four weeks"}, {"3days", "three days"}}
			}}},
		Recheck: []string{"disk"},
		Plan: func(_ context.Context, _ *check.Env, _ Target, v Values) ([]Step, error) {
			return []Step{
				{Why: "drop entries older than " + v["keep"], Argv: []string{"journalctl", "--vacuum-time=" + v["keep"]}},
				{Why: "usage now", Argv: []string{"journalctl", "--disk-usage"}},
			}, nil
		},
	})
	register(Op{
		ID: "disk-wp-caches", Title: "Empty every site's wp-content/cache", Section: "system", Risk: Change,
		Note:    "Page caches (WP Rocket and others). Pages are slower until the cache refills.",
		How:     cacheHow,
		Undo:    "Nothing to undo — caches regenerate.",
		Recheck: []string{"disk"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, _ Values) ([]Step, error) {
			var steps []Step
			for _, d := range dirs(env, "/home/*/web/*/public_html/wp-content/cache") {
				if !exists(env, filepath.Dir(filepath.Dir(d))+"/wp-config.php") {
					continue
				}
				steps = append(steps, Step{Why: human(duBytes(ctx, env, d)), Argv: []string{"find", d, "-mindepth", "1", "-delete"}})
			}
			return steps, nil
		},
	})
	register(Op{
		ID: "disk-transients", Title: "Delete expired transients on every site", Section: "system", Risk: Change,
		Note: "Expired rows WordPress left in wp_options. Current transients are kept.",
		How:  "`wp transient delete --expired` per WordPress site, run as the site's user.",
		Undo: "Nothing to undo — only expired entries go.",
		Plan: func(_ context.Context, env *check.Env, _ Target, _ Values) ([]Step, error) {
			if !have(env, "wp") {
				return nil, fmt.Errorf("WP-CLI is not installed")
			}
			var steps []Step
			for _, d := range hestia.WebDomains(env.Sys) {
				if exists(env, d.DocRoot()+"/wp-config.php") {
					steps = append(steps, Step{Why: d.Name, Argv: wp(d, "transient", "delete", "--expired")})
				}
			}
			return steps, nil
		},
	})
	register(Op{
		ID: "disk-php-sessions", Title: "Delete PHP sessions older than a day", Section: "system", Risk: Change,
		Note:    "Visitors whose session is over a day old are logged out of whatever used it.",
		How:     "find /var/lib/php/sessions -name 'sess_*' -mtime +1 -delete.",
		Undo:    "None.",
		Recheck: []string{"disk"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, _ Values) ([]Step, error) {
			out, _ := env.Sys.Run(ctx, "find", "/var/lib/php/sessions/", "-maxdepth", "2", "-name", "sess_*", "-mtime", "+1")
			n := len(nonEmptyLines(out))
			if n == 0 {
				return nil, nil
			}
			return []Step{{Why: fmt.Sprintf("%d session file(s)", n), Argv: []string{"find", "/var/lib/php/sessions/", "-maxdepth", "2", "-name", "sess_*", "-mtime", "+1", "-delete"}}}, nil
		},
	})
	register(Op{
		ID: "disk-wpcli-cache", Title: "Clear WP-CLI download caches", Section: "system", Risk: Change,
		Note:    "Core, plugin and theme zips WP-CLI cached for root and every user.",
		How:     cacheHow,
		Undo:    "Nothing to undo — WP-CLI downloads again when needed.",
		Recheck: []string{"disk"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, _ Values) ([]Step, error) {
			var steps []Step
			for _, d := range append(dirs(env, "/root/.wp-cli/cache"), dirs(env, "/home/*/.wp-cli/cache")...) {
				steps = append(steps, Step{Why: human(duBytes(ctx, env, d)), Argv: []string{"find", d, "-mindepth", "1", "-delete"}})
			}
			return steps, nil
		},
	})
	register(Op{
		ID: "log-trim", Title: "Trim a large log", Section: "system", Risk: Change,
		Note: "Empties or trims one live log in place — the writer keeps its file handle, so the space is freed at once. Or caps it hourly instead (hs logcap).",
		How:  "truncate: `truncate -s 0` on the file. keep 500: the last 500 lines are copied out and written back into the same file (same inode). cap: `hs logcap add` — logrotate every hour at the chosen size, keeping one compressed previous file (copytruncate, as the file's owner). The file is never deleted or replaced: nginx and PHP-FPM hold logs open, and an unlinked open file frees nothing until they reload.",
		Undo: "None for the trimmed lines. A cap is removed with `hs logcap remove KEY`.",
		Fields: []Field{
			{Key: "file", Label: "Log", Kind: Choice, Help: "Logs of 1 MB and more in site roots, private/ and Hestia's domain logs, largest first.",
				Choices: func(ctx context.Context, env *check.Env, _ Target) [][2]string {
					var out [][2]string
					for _, l := range bigLogs(ctx, env, 1, false) {
						out = append(out, [2]string{l.Path, human(l.Size)})
					}
					return out
				}},
			{Key: "how", Label: "Do", Kind: Choice,
				Choices: func(context.Context, *check.Env, Target) [][2]string {
					return [][2]string{{"keep500", "keep the last 500 lines"}, {"truncate", "empty it"}, {"cap", "cap it at " + logcap.DefaultSize + ", hourly"}}
				}},
		},
		Recheck: []string{"disk"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			f := v["file"]
			if f == "" || !exists(env, f) {
				return nil, fmt.Errorf("no such log: %q", f)
			}
			tail, _ := env.Sys.Run(ctx, "tail", "-n", "3", f)
			why := "last lines now:\n" + strings.TrimRight(tail, "\n")
			switch v["how"] {
			case "truncate":
				return []Step{{Why: why, Argv: []string{"truncate", "-s", "0", f}}}, nil
			case "keep500":
				return []Step{{Why: why, Argv: []string{"sh", "-c", `t=$(mktemp) && tail -n 500 "$1" > "$t" && cat "$t" > "$1"; rc=$?; rm -f "$t"; ls -l "$1"; exit $rc`, "sh", f}}}, nil
			case "cap":
				user, domain := siteOf(f)
				if user == "" {
					user = "root"
				}
				key := strings.Trim(strings.NewReplacer("/", "_", ".", "_").Replace(strings.TrimPrefix(f, "/home/")), "_")
				if domain != "" {
					key = domain + "_" + filepath.Base(f)
				}
				return []Step{{Why: why + "\ncap hourly at " + logcap.DefaultSize + " — adds to /etc/hs/logcap.conf:\n" + strings.TrimRight(logcap.Block(key, f, user, logcap.DefaultSize), "\n"),
					Argv: []string{self(), "logcap", "add", key, f, user, logcap.DefaultSize}}}, nil
			}
			return nil, fmt.Errorf("unknown choice %q", v["how"])
		},
	})
	register(Op{
		ID: "logs-rotated", Title: "Delete rotated log archives", Section: "system", Risk: Change,
		Note:    "Old rotated logs (*.gz, *.1, *.old) of 1 MB and more. Nothing holds them open, so deleting them frees the space at once.",
		How:     "`rm -f -v` on each archive the plan lists (with its size). Live logs are not touched.",
		Undo:    "None — the archived lines are gone.",
		Recheck: []string{"disk"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, _ Values) ([]Step, error) {
			var steps []Step
			var total int64
			for _, l := range bigLogs(ctx, env, 1, true) {
				total += l.Size
				steps = append(steps, Step{Why: human(l.Size), Argv: []string{"rm", "-f", "-v", "--", l.Path}})
			}
			if len(steps) > 0 {
				steps[0].Why = "frees " + human(total) + " in total\n" + steps[0].Why
			}
			return steps, nil
		},
	})
	register(Op{
		ID: "ncdu", Title: "Browse disk usage (ncdu)", Section: "system", Risk: ReadOnly, Interactive: true, Resolves: "disk",
		Note: "ncdu takes the terminal; q returns here. Deleting inside ncdu (d) is its own, unlogged action.",
		How:  "Installs ncdu with apt if missing, then runs `ncdu -x PATH` (-x: stays on that filesystem).",
		Fields: []Field{{Key: "path", Label: "Directory", Kind: Choice,
			Choices: func(context.Context, *check.Env, Target) [][2]string {
				return [][2]string{{"/home", "sites and user files"}, {"/", "everything"}, {"/backup", "Hestia backups"}, {"/var/log", "logs"}, {"/var/lib/mysql", "databases"}}
			}}},
		Plan: func(_ context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			var steps []Step
			if !have(env, "ncdu") {
				steps = append(steps, aptInstall("ncdu is not installed", "ncdu"))
			}
			return append(steps, Step{Why: "q quits", Argv: []string{"ncdu", "-x", v["path"]}}), nil
		},
	})

	register(Op{
		ID: "remove-service", Title: "Remove an unneeded service", Section: "system", Risk: Destructive, Resolves: "services.idle",
		Note: "Purges a mail/FTP component a WordPress box does not use and clears its hestia.conf key, so Hestia stops trying to restart it (the \"restart failed\" mails).",
		How:  "Stops and disables the units, `apt-get remove --purge` the packages, `apt-get autoremove`, `systemctl reset-failed`, then `v-change-sys-config-value KEY ''` — only when hestia.conf names this service (a proftpd box never loses FTP_SYSTEM to the vsftpd path). For ClamAV and SpamAssassin, `sed` comments out their lines in /etc/exim4/exim4.conf.template, `exim4 -bV` checks the result, and Exim restarts.",
		Undo: "Reinstall the packages and set the key back (v-change-sys-config-value KEY VALUE); Exim's template lines are only commented, uncomment them.",
		Fields: []Field{{Key: "service", Label: "Service", Kind: Choice,
			Choices: func(ctx context.Context, env *check.Env, _ Target) [][2]string {
				conf := hestia.Conf(env.Sys)
				var out [][2]string
				for _, s := range services {
					switch {
					case box.PkgInstalled(ctx, env.Sys, s.Pkgs...):
						out = append(out, [2]string{s.Key, s.Label + " — installed"})
					case contains(s.Values, conf[s.Key]):
						out = append(out, [2]string{s.Key, s.Label + " — gone, but " + s.Key + "='" + conf[s.Key] + "' (config only)"})
					}
				}
				return out
			}}},
		Recheck: []string{"hestia.services", "services.idle"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			var s service
			for _, x := range services {
				if x.Key == v["service"] {
					s = x
				}
			}
			if s.Key == "" {
				return nil, fmt.Errorf("nothing to remove")
			}
			var steps []Step
			if box.PkgInstalled(ctx, env.Sys, s.Pkgs...) {
				steps = append(steps,
					Step{Why: s.Impact + "\nstop it", Argv: append([]string{"systemctl", "disable", "--now"}, s.Units...)},
					Step{Why: "purge the packages", Argv: append([]string{"env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "remove", "--purge", "-y"}, s.Purge...)},
					Step{Why: "their dependencies", Argv: []string{"env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "autoremove", "-y"}},
					Step{Argv: append([]string{"systemctl", "reset-failed"}, s.Units...)})
			}
			if cur := hestia.Conf(env.Sys)[s.Key]; contains(s.Values, cur) {
				steps = append(steps, Step{Why: "hestia.conf " + s.Key + "='" + cur + "' → '' (Hestia stops restarting it)", Argv: []string{bin + "v-change-sys-config-value", s.Key, ""}})
			}
			tpl := "/etc/exim4/exim4.conf.template"
			if len(s.Exim) > 0 && exists(env, tpl) {
				args := []string{"sed", "-i.hs-" + s.Units[0]}
				for _, e := range s.Exim {
					args = append(args, "-e", e)
				}
				steps = append(steps,
					Step{Why: "comment out Exim's " + s.Label + " lines (previous template kept as " + tpl + ".hs-" + s.Units[0] + ")", Argv: append(args, tpl)},
					Step{Why: "Exim accepts the result", Argv: []string{"exim4", "-bV"}},
					Step{Argv: []string{"systemctl", "restart", "exim4"}})
			}
			return steps, nil
		},
	})

	register(Op{
		ID: "updates-preview", Title: "Preview pending package updates", Section: "security", Risk: ReadOnly,
		Note:     "Refreshes package lists, then shows what an upgrade would do — each package classified (rebuild, patch, minor, major, data) with what it restarts. Nothing is installed.",
		How:      "`apt-get update`, then `apt-get -s upgrade` — apt's own dry run, so the preview cannot disagree with the real upgrade — read by hs.",
		Resolves: "updates.pending",
		Plan: func(context.Context, *check.Env, Target, Values) ([]Step, error) {
			return []Step{{Why: "refresh, then simulate", Argv: []string{self(), "apt", "preview", "--refresh"}}}, nil
		},
	})
	register(Op{
		ID: "updates-apply", Title: "Upgrade system packages", Section: "security", Risk: Change,
		Note:    "apt-get upgrade with the packages listed in the plan (from the last package-list refresh — preview first for a fresh list). Keeps your changed config files. Services named in the plan restart.",
		How:     "`apt-get upgrade -y` non-interactively, keeping locally modified configuration files (--force-confold) so nothing hs or you configured is replaced; never removes packages (that is dist-upgrade). The plan is apt's own simulation of the same transaction. Afterwards: whether a reboot is required.",
		Undo:    "No automatic downgrade. A specific package can be pinned back with apt-get install PKG=OLDVERSION if the old version is still in the archive.",
		Recheck: []string{"updates.pending", "updates.reboot"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, _ Values) ([]Step, error) {
			out, err := env.Sys.Run(ctx, "apt-get", "-s", "upgrade")
			if err != nil {
				return nil, fmt.Errorf("apt-get -s upgrade: %v", err)
			}
			p := aptinfo.Parse(out)
			if p.Empty() {
				return nil, nil
			}
			return []Step{
				{Why: strings.Join(p.Render(12), "\n"), Argv: []string{"env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "upgrade", "-y", "-o", "Dpkg::Options::=--force-confold"}},
				{Why: "reboot needed?", Argv: []string{"sh", "-c", "if [ -f /var/run/reboot-required ]; then echo 'yes — reboot required:'; cat /var/run/reboot-required.pkgs 2>/dev/null; else echo 'no reboot needed'; fi"}},
			}, nil
		},
	})
	register(Op{
		ID: "apt-changelog", Title: "Changelog of an upgradable package", Section: "security", Risk: ReadOnly,
		Note: "What changed in the new version — Debian packages carry it; third-party repositories often do not.",
		How:  "`apt-get changelog PKG`, first 80 lines.",
		Fields: []Field{{Key: "package", Label: "Package", Kind: Choice,
			Choices: func(ctx context.Context, env *check.Env, _ Target) [][2]string {
				out, _ := env.Sys.Run(ctx, "apt-get", "-s", "upgrade")
				var cs [][2]string
				for _, u := range aptinfo.Parse(out).Upgrades {
					cs = append(cs, [2]string{u.Name, u.Class + " " + aptinfo.Upstream(u.Cur) + " → " + aptinfo.Upstream(u.New)})
				}
				return cs
			}}},
		Plan: func(_ context.Context, _ *check.Env, _ Target, v Values) ([]Step, error) {
			return []Step{{Argv: []string{"sh", "-c", `apt-get changelog "$1" 2>&1 | head -80`, "sh", v["package"]}}}, nil
		},
	})
	register(Op{
		ID: "apt-repos", Title: "Check apt repositories", Section: "system", Risk: ReadOnly,
		Note: "Refreshes package lists and explains every repository that fails: what the error means, which file defines it, and the one rule for fixing it.",
		How:  "`apt-get update` (nothing is installed), each Err: line matched to its sources file and explained.",
		Plan: func(context.Context, *check.Env, Target, Values) ([]Step, error) {
			return []Step{{Argv: []string{self(), "apt", "repos"}}}, nil
		},
	})
	register(Op{
		ID: "filemanager-fix", Title: "Repair the Hestia file manager session handler", Section: "system", Risk: Change,
		Note: "Restores SessionStorage.php from the copy that ships with the installed Hestia version (run.sh downloaded whatever GitHub main held that day, and kept one backup).",
		How:  "`hs conf install` copies /usr/local/hestia/install/deb/filemanager/.../SessionStorage.php — the file Hestia's own v-add-sys-filemanager installs, matching this Hestia version — over the live one in /usr/local/hestia/web/fm, printing the changed lines and keeping the previous file; then owner hestiaweb. If the fix you need is newer than this Hestia, update Hestia first.",
		Undo: "cp -p the kept file back (path printed).",
		Plan: func(_ context.Context, env *check.Env, _ Target, _ Values) ([]Step, error) {
			rel := "/filemanager/filegator/backend/Services/Session/Adapters/SessionStorage.php"
			src := hestia.Root + "/install/deb" + rel
			dst := hestia.Root + "/web/fm/backend/Services/Session/Adapters/SessionStorage.php"
			if !exists(env, src) {
				return nil, fmt.Errorf("%s not found — this Hestia ships no file manager source", src)
			}
			if !exists(env, dst) {
				return nil, fmt.Errorf("the file manager is not installed (%s missing) — v-add-sys-filemanager installs it", dst)
			}
			if readFile(env, src) == readFile(env, dst) {
				return nil, nil
			}
			return []Step{confInstall("this Hestia's own copy", src, dst), {Argv: []string{"chown", "hestiaweb:hestiaweb", dst}}}, nil
		},
	})
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s && s != "" {
			return true
		}
	}
	return false
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
