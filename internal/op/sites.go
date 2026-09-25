package op

// Per-site operations: the wp-config editor (v-wp-config's registry as a
// form), clone, staging and its teardown, finishing a migration — the
// scripts that prompted over SSH, now driven by a form and their flags.
// Backups: restic onboarding as a form (the Storage Box password masked).

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/hestia"
)

var domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}$`)

// wpDefine reads a define's value from wp-config.php ("" = not set); quotes
// are stripped, so 'minor' and true both come back bare.
func wpDefine(src, name string) (string, bool) {
	m := regexp.MustCompile(`(?m)^\s*define\s*\(\s*['"]` + regexp.QuoteMeta(name) + `['"]\s*,\s*(.+?)\s*\)\s*;`).FindStringSubmatch(src)
	if m == nil {
		return "", false
	}
	return strings.Trim(m[1], `'"`), true
}

type wpDef struct {
	Name, Kind, Default, Rec, Help string // Kind: bool int intfalse str core
}

// The registry v-wp-config.sh shows, in its order.
var wpDefs = []wpDef{
	{"WP_POST_REVISIONS", "intfalse", "unlimited", "5", "Revisions kept per post — false disables them; 5–10 keeps the database lean."},
	{"AUTOSAVE_INTERVAL", "int", "60", "120", "Autosave interval in seconds."},
	{"WP_MEMORY_LIMIT", "str", "40M", "256M", "Front-end PHP memory limit."},
	{"WP_MAX_MEMORY_LIMIT", "str", "256M", "512M", "wp-admin and WP-CLI memory limit."},
	{"DISALLOW_FILE_EDIT", "bool", "false", "true", "Removes the theme/plugin code editor from wp-admin."},
	{"DISALLOW_FILE_MODS", "bool", "false", "false", "Blocks ALL file changes including updates — removes the update UI."},
	{"FORCE_SSL_ADMIN", "bool", "false", "true", "wp-admin and wp-login.php over HTTPS only."},
	{"WP_DEBUG", "bool", "false", "false", "Debug mode — false in production (or keep it on with the log capped: fix “keep the debug log”)."},
	{"WP_DEBUG_LOG", "str", "false", "false", "true = wp-content/debug.log (web-served!), a path = that file, false = off."},
	{"WP_DEBUG_DISPLAY", "bool", "true", "false", "Shows PHP errors to visitors — false in production."},
	{"DISABLE_WP_CRON", "bool", "false", "-", "Stops WordPress running cron on page loads — only with a real system cron."},
	{"WP_CRON_LOCK_TIMEOUT", "int", "60", "60", "Seconds before a cron lock expires."},
	{"WP_AUTO_UPDATE_CORE", "core", "minor", "minor", "Core auto-updates: minor = security and maintenance releases, true = all, false = none."},
	{"AUTOMATIC_UPDATER_DISABLED", "bool", "false", "false", "Turns off every automatic background update."},
	{"EMPTY_TRASH_DAYS", "int", "30", "14", "Days before trash is emptied (0 disables the trash)."},
	{"MEDIA_TRASH", "bool", "false", "false", "Deleted media goes to the trash instead of being deleted at once."},
	{"WP_CACHE", "bool", "false", "true", "Needed by page caches (WP Rocket) — set by them."},
}

var intRe = regexp.MustCompile(`^[0-9]+$`)

func wpConfigFields() []Field {
	var fs []Field
	for _, d := range wpDefs {
		d := d
		f := Field{Key: d.Name, Label: d.Name, Optional: true, Help: d.Help,
			Current: func(_ context.Context, env *check.Env, t Target) string {
				v, ok := wpDefine(readFile(env, t.Domain.DocRoot()+"/wp-config.php"), d.Name)
				s := v
				if !ok {
					s = "not set (WordPress default " + d.Default + ")"
				}
				if d.Rec != "-" && v != d.Rec {
					s += " · recommended " + d.Rec
				}
				return s
			},
			Default: func(_ context.Context, env *check.Env, t Target) string {
				v, _ := wpDefine(readFile(env, t.Domain.DocRoot()+"/wp-config.php"), d.Name)
				return v
			}}
		leave := [2]string{"", "not set — WordPress default " + d.Default}
		// A value written by hand (WP_DEBUG 1) stays selectable as it is.
		withCurrent := func(env *check.Env, t Target, cs [][2]string) [][2]string {
			cur, set := wpDefine(readFile(env, t.Domain.DocRoot()+"/wp-config.php"), d.Name)
			for _, c := range cs {
				if c[0] == cur {
					return cs
				}
			}
			if set {
				cs = append(cs, [2]string{cur, "as written in wp-config.php"})
			}
			return cs
		}
		switch d.Kind {
		case "bool":
			f.Kind = Choice
			f.Choices = func(_ context.Context, env *check.Env, t Target) [][2]string {
				return withCurrent(env, t, [][2]string{leave, {"true", ""}, {"false", ""}})
			}
		case "core":
			f.Kind = Choice
			f.Choices = func(_ context.Context, env *check.Env, t Target) [][2]string {
				return withCurrent(env, t, [][2]string{leave, {"minor", "security and maintenance releases"}, {"true", "all releases"}, {"false", "none"}})
			}
		case "int":
			f.Kind, f.Pattern = Text, intRe
		case "intfalse":
			f.Kind = Text
			f.Validate = func(v string) error {
				if v != "false" && !intRe.MatchString(v) {
					return fmt.Errorf("%s: a number or false", d.Name)
				}
				return nil
			}
		default:
			f.Kind = Text
		}
		fs = append(fs, f)
	}
	return fs
}

func stagingOf(env *check.Env, d hestia.Domain) string {
	b, _ := env.Sys.ReadFile("/root/.hestia-staging-urls/" + d.User + "_" + d.Name)
	s := strings.TrimSpace(string(b))
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	s, _, _ = strings.Cut(s, "/")
	return s
}

// liveOf finds the live site a staging domain was built from (its sidecar).
func liveOf(env *check.Env, staging string) (user, domain string, ok bool) {
	files, _ := env.Sys.Glob("/root/.hestia-staging-urls/*")
	for _, f := range files {
		b, _ := env.Sys.ReadFile(f)
		if strings.Contains(string(b), staging) {
			u, d, found := strings.Cut(filepath.Base(f), "_")
			return u, d, found
		}
	}
	return "", "", false
}

func domainExists(env *check.Env, name string) bool {
	for _, d := range hestia.WebDomains(env.Sys) {
		if d.Name == name {
			return true
		}
	}
	return false
}

func usersField(help string) Field {
	return Field{Key: "dest-user", Label: "Owner (Hestia user)", Kind: Choice, Help: help,
		Choices: func(_ context.Context, env *check.Env, t Target) [][2]string {
			out := [][2]string{{t.Domain.User, "same user as the source"}}
			for _, u := range hestia.Users(env.Sys) {
				if u != t.Domain.User {
					out = append(out, [2]string{u, ""})
				}
			}
			return out
		}}
}

func overwriteField(what string) Field {
	return Field{Key: "overwrite", Label: "Overwrite if it exists", Kind: Bool,
		Help: "Only when " + what + " already exists: yes replaces its files and database (typed confirmation)."}
}

func overwriteRisk(v Values) Risk {
	if v.Bool("overwrite") {
		return Destructive
	}
	return Change
}

func init() {
	register(Op{
		ID: "wp-config", Title: "wp-config constants", Section: "sites", Site: true, WPOnly: true, Risk: Change,
		Note:    "Every common define with its value now and the recommendation. Only fields you change are written; empty a field to remove the define.",
		How:     "`wp config set NAME VALUE --type=constant` for each changed field (true/false/numbers with --raw, so they are PHP literals; 'minor' stays a string), `wp config delete` for an emptied one — as the site's user, so wp-config.php keeps its owner. wp-cli keeps the rest of the file as it is.",
		Undo:    "Run it again with the previous values (shown as “now”).",
		Fields:  wpConfigFields(),
		Recheck: []string{"site.debug", "site.http"},
		Plan: func(_ context.Context, env *check.Env, t Target, v Values) ([]Step, error) {
			src := readFile(env, t.Domain.DocRoot()+"/wp-config.php")
			if src == "" {
				return nil, fmt.Errorf("no wp-config.php in %s", t.Domain.DocRoot())
			}
			var steps []Step
			for _, d := range wpDefs {
				cur, set := wpDefine(src, d.Name)
				nv := v[d.Name]
				switch {
				case nv == cur:
				case nv == "" && set:
					steps = append(steps, Step{Why: d.Name + ": " + cur + " → removed (WordPress default " + d.Default + ")", Argv: wp(*t.Domain, "config", "delete", d.Name, "--type=constant")})
				case nv != "":
					args := []string{"config", "set", d.Name, nv, "--type=constant"}
					raw := d.Kind == "bool" || d.Kind == "int" || (d.Kind == "intfalse") || (d.Kind == "core" && nv != "minor") ||
						(d.Name == "WP_DEBUG_LOG" && (nv == "true" || nv == "false"))
					if raw {
						args = append(args, "--raw")
					}
					was := cur
					if !set {
						was = "not set"
					}
					steps = append(steps, Step{Why: d.Name + ": " + was + " → " + nv, Argv: wp(*t.Domain, args...)})
				}
			}
			return steps, nil
		},
	})
	register(Op{
		ID: "clone", Title: "Clone this site", Section: "sites", Site: true, WPOnly: true, Risk: Change, RiskOf: overwriteRisk,
		Note: "A full copy under a new domain: files, a new database, fresh salts and Redis prefix, URLs replaced. The source is only read.",
		How:  "v-wp-clone-site with every answer as a flag (its prompts never appear): pre-flight (shell access, PHP exec, disk space) → web domain (SSL only if DNS already points here) → `wp db export` of the source → rsync without caches/backups/debug.log → new database → wp-config rewritten (credentials, salts, own Redis database and prefix) → import → search-replace of every URL form and absolute path → cache flush. Creating a new Hestia user is not part of it — add the user in Hestia first.",
		Undo: "Delete the new web domain and its database in Hestia (v-delete-web-domain, v-delete-database).",
		Fields: []Field{
			usersField("Must exist in Hestia already."),
			{Key: "new-domain", Label: "New domain", Kind: Text, Pattern: domainRe, Help: "e.g. copy.demolink.fi — DNS can follow later; SSL is issued only when it already points here."},
			overwriteField("the new domain"),
		},
		Recheck: []string{"site.http"},
		Plan: func(_ context.Context, env *check.Env, t Target, v Values) ([]Step, error) {
			nd := v["new-domain"]
			if nd == t.Domain.Name {
				return nil, fmt.Errorf("the clone needs a different domain")
			}
			args := []string{bin + "v-wp-clone-site", "--src-user=" + t.Domain.User, "--src-domain=" + t.Domain.Name, "--dest-user=" + v["dest-user"], "--new-domain=" + nd}
			why := "new site " + nd + " for " + v["dest-user"]
			if domainExists(env, nd) {
				if !v.Bool("overwrite") {
					return nil, fmt.Errorf("%s already exists — set “Overwrite if it exists” to replace it", nd)
				}
				args = append(args, "--force")
				why = "REPLACES " + nd + "'s files and database"
			}
			return []Step{{Why: why, Argv: args}}, nil
		},
	})
	register(Op{
		ID: "staging", Title: "Create / refresh staging copy", Section: "sites", Site: true, WPOnly: true, Risk: Change, RiskOf: overwriteRisk,
		Note: "Isolated copy: built in public_html.setup and swapped in only after it verifies, live uploads mounted read-only, cron and file changes off. Live is only cache-flushed.",
		How:  "v-wp-staging-create with flags: builds in public_html.setup (the staging URL 404s until everything checks out), rewrites wp-config (WP_HOME/SITEURL, WP_ENVIRONMENT_TYPE=staging, DISABLE_WP_CRON, DISALLOW_FILE_MODS, a random Redis prefix), imports, search-replaces, runs an 8-point verification, swaps it in, bind-mounts live uploads read-only (write-probed, kept across reboots in /etc/fstab) and flushes caches on both sides. On failure its trap removes the half-built copy and prints the retry and teardown commands. The staging domain is remembered in /root/.hestia-staging-urls, not in live's wp-config.",
		Undo: "Tear down staging (on the staging site's screen).",
		Fields: []Field{
			usersField(""),
			{Key: "new-domain", Label: "Staging domain", Kind: Text, Pattern: domainRe, Help: "Pre-filled with the one remembered for this site.",
				Current: func(_ context.Context, env *check.Env, t Target) string {
					if s := stagingOf(env, *t.Domain); s != "" {
						return "remembered: " + s
					}
					return "none yet"
				},
				Default: func(_ context.Context, env *check.Env, t Target) string { return stagingOf(env, *t.Domain) }},
			overwriteField("the staging domain"),
		},
		Plan: func(_ context.Context, env *check.Env, t Target, v Values) ([]Step, error) {
			nd := v["new-domain"]
			if nd == t.Domain.Name {
				return nil, fmt.Errorf("staging needs its own domain")
			}
			args := []string{bin + "v-wp-staging-create", "--src-user=" + t.Domain.User, "--src-domain=" + t.Domain.Name, "--dest-user=" + v["dest-user"], "--new-domain=" + nd}
			why := "new staging site " + nd
			if domainExists(env, nd) {
				if !v.Bool("overwrite") {
					return nil, fmt.Errorf("%s already exists — set “Overwrite if it exists” to rebuild it", nd)
				}
				args = append(args, "--force")
				why = "REBUILDS " + nd + " (its files and database are replaced)"
			}
			return []Step{{Why: why, Argv: args}}, nil
		},
	})
	register(Op{
		ID: "staging-teardown", Title: "Tear down this staging site", Section: "sites", Site: true, Risk: Destructive,
		Note: "Deletes this staging site: web domain, database, the read-only uploads mount. Live's own files are never touched.",
		How:  "v-wp-staging-create --teardown: unmounts the uploads bind mount (and its /etc/fstab line), v-delete-web-domain, v-delete-database; with the live site known (from /root/.hestia-staging-urls) it also forgets the remembered URL and flushes live's caches.",
		Undo: "None — build it again with Create / refresh staging copy.",
		Plan: func(_ context.Context, env *check.Env, t Target, _ Values) ([]Step, error) {
			args := []string{bin + "v-wp-staging-create", "--teardown", "--dest-user=" + t.Domain.User, "--new-domain=" + t.Domain.Name, "--force"}
			u, d, ok := liveOf(env, t.Domain.Name)
			why := "no live site remembers this domain — only the staging side is removed"
			if ok {
				args = append(args, "--src-user="+u, "--src-domain="+d)
				why = "staging of " + d + " (" + u + ")"
			} else if cfg := readFile(env, t.Domain.DocRoot()+"/wp-config.php"); !strings.Contains(cfg, "staging") {
				return nil, fmt.Errorf("%s does not look like a staging site (no WP_ENVIRONMENT_TYPE staging, no live site remembers it) — refusing", t.Domain.Name)
			}
			return []Step{{Why: why, Argv: args}}, nil
		},
	})
	register(Op{
		ID: "migrate-finish", Title: "Finish a migration (import dump, fix URLs)", Section: "sites", Site: true, Risk: Change,
		Note: "For a site rsynced from another host with its SQL dump in public_html: a new database, wp-config rewritten, dump imported, old URL replaced. The dump file is deleted at the end (by the script).",
		How:  "v-wp-migrate-site with flags: ownership and permissions fixed, a stale auto_prepend_file removed from .user.ini → new database (random suffix — a second run makes another) → wp-config credentials, salts, Redis prefix, WP_DEBUG off, WP_HOME/SITEURL → import → search-replace old → new URL (http and https; the old URL is read from the dump's home option unless given) → caches flushed → the SQL file deleted.",
		Undo: "Delete the new database in Hestia; the files are as rsynced (wp-config was rewritten — keep your own copy).",
		Fields: []Field{
			{Key: "sql", Label: "SQL dump", Kind: Choice,
				Choices: func(_ context.Context, env *check.Env, t Target) [][2]string {
					m, _ := env.Sys.Glob(t.Domain.DocRoot() + "/*.sql")
					var out [][2]string
					for _, p := range m {
						out = append(out, [2]string{p, ""})
					}
					return out
				}},
			{Key: "old-url", Label: "Old URL", Kind: Text, Optional: true, Pattern: regexp.MustCompile(`^https?://[^\s/]+$`),
				Help: "e.g. https://old.example.fi — empty: read from the dump's home option."},
		},
		Recheck: []string{"site.http", "site.core"},
		Plan: func(_ context.Context, env *check.Env, t Target, v Values) ([]Step, error) {
			if v["sql"] == "" {
				return nil, fmt.Errorf("no *.sql file in %s — upload the dump there first", t.Domain.DocRoot())
			}
			args := []string{bin + "v-wp-migrate-site", "--user=" + t.Domain.User, "--domain=" + t.Domain.Name, "--sql=" + v["sql"], "--force"}
			if v["old-url"] != "" {
				args = append(args, "--old-url="+v["old-url"])
			}
			return []Step{{Why: "import " + filepath.Base(v["sql"]) + " into a new database", Argv: args}}, nil
		},
	})

	register(Op{
		ID: "restic-setup", Title: "Set up restic to a Storage Box", Section: "backups", Risk: Change, Interactive: true,
		Resolves: "backups.setup", Applies: summaryHas("not set up"),
		Note: "Onboards this box: host key pinned, rclone remote, a per-box repository, per-user incremental on, nightly + hourly schedule. A subaccount needs the password and SSH enabled on it in the Hetzner console. One connection attempt only — the Storage Box bans an IP after repeated failed logins.",
		How:  "setup-restic-backup.sh with the form's answers as flags. Password mode: the password travels in SB_PASS (typed masked here; never in argv or the log) and is stored obscured in rclone.conf. Key mode: a dedicated key /root/.ssh/storagebox; if the Storage Box does not know it yet, the script runs install-ssh-key, which asks for the Storage Box password in this terminal. The terminal is handed over for that reason.",
		Undo: "rm /etc/cron.d/hestia-restic; v-delete-backup-host restic. The repository on the Storage Box stays.",
		Fields: []Field{
			{Key: "host", Label: "Storage Box host", Kind: Text, Pattern: regexp.MustCompile(`^[a-z0-9.-]+$`), Help: "uXXXXX.your-storagebox.de, or uXXXXX-subN.your-storagebox.de for a subaccount."},
			{Key: "user", Label: "Storage Box user", Kind: Text, Pattern: regexp.MustCompile(`^u[0-9]+(-sub[0-9]+)?$`)},
			{Key: "auth", Label: "Authentication", Kind: Choice, Choices: func(context.Context, *check.Env, Target) [][2]string {
				return [][2]string{{"password", "password (required for subaccounts)"}, {"key", "SSH key (main account)"}}
			}},
			{Key: "password", Label: "Storage Box password", Kind: Secret, Optional: true, Help: "Password mode only."},
			{Key: "path", Label: "Repository path", Kind: Text, Optional: true, Help: "Relative to the Storage Box home; empty = hestia-<this host>/. Never “/” (an empty path breaks every user's backup)."},
			{Key: "hourly", Label: "Hourly database snapshots", Kind: Bool, Default: func(context.Context, *check.Env, Target) string { return "yes" }},
		},
		Recheck: []string{"backups.setup", "backups.nightly"},
		Plan: func(_ context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			script, err := repoFile(env, "setup-restic-backup.sh")
			if err != nil {
				return nil, err
			}
			if p := strings.Trim(v["path"], "/"); v["path"] != "" && p == "" {
				return nil, fmt.Errorf("an empty repository path is the hzweb1 failure — give a per-box directory")
			}
			args := []string{"--host", v["host"], "--user", v["user"]}
			if v["path"] != "" {
				args = append(args, "--path", v["path"])
			}
			if !v.Bool("hourly") {
				args = append(args, "--no-hourly")
			}
			if v["auth"] == "password" {
				if v["password"] == "" {
					return nil, fmt.Errorf("password mode needs the Storage Box password")
				}
				args = append(args, "--password")
				return []Step{{Why: "onboard (password from the environment, never on screen)",
					Argv: append([]string{"sh", "-c", `SB_PASS="$` + SecretEnv("password") + `" exec bash "$0" "$@"`, script}, args...)}}, nil
			}
			return []Step{{Why: "onboard with the box's own key", Argv: append([]string{"bash", script}, args...)}}, nil
		},
	})
}
