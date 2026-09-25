package op

// The watch (docs/security-hardening.md): cron, mail address, baseline, and
// accepting what the persistence checks list for review.

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/check/box"
	"github.com/valolink/hestiascripts/internal/conf"
	"github.com/valolink/hestiascripts/internal/state"
	"github.com/valolink/hestiascripts/internal/watch"
)

var absPath = regexp.MustCompile(`^/[^\s]+$`)

const (
	watchConf = "/etc/hs/watch.conf"
	watchCron = "/etc/cron.d/hs-watch"
)

func watchCronText(hs string) string {
	return "# hs: security watch (hs op watch-enable) — mails only new findings, at most once a day each\n" +
		"MAILTO=\"\"\n" +
		"*/15 * * * * root flock -n /run/hs-watch.lock " + hs + " watch --mail --quiet >/dev/null 2>&1\n" +
		"7 * * * * root flock -w 600 /run/hs-watch.lock " + hs + " watch --sweep --mail --quiet >/dev/null 2>&1\n"
}

func init() {
	register(Op{
		ID: "watch-enable", Title: "Security watch: run and alert", Section: "security", Risk: Change,
		Note: "Runs the security checks every 15 minutes (the filesystem sweeps hourly) and mails findings that are new since you last accepted the box's state — at most once a day each. Until you accept the current state, everything found counts as new.",
		How:  "Writes MAIL_TO to " + watchConf + " (hs conf set) and " + watchCron + ": `hs watch --mail` every 15 minutes and `hs watch --sweep --mail` hourly, each under flock so runs never overlap. Mail goes through the box's own mail setup (the SMTP relay); a failed send becomes a finding in the next run, so a broken relay cannot silence the watch. The last step runs the watch once and lists what it finds.",
		Undo: "rm " + watchCron,
		Fields: []Field{{Key: "mail-to", Label: "Alerts to", Kind: Text, Pattern: emailRe,
			Default: func(_ context.Context, env *check.Env, _ Target) string {
				v, _ := conf.Get(readFile(env, watchConf), conf.Opts{}, "MAIL_TO")
				if v == "" {
					return watch.DefaultMailTo
				}
				return v
			}}},
		Recheck: []string{"mail.delivery"},
		Plan: func(_ context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			return []Step{
				{Why: "watch settings", Argv: []string{"sh", "-c", "install -d -m 700 /etc/hs && [ -e " + watchConf + " ] || install -m 600 /dev/null " + watchConf}},
				confSet("alerts to "+v["mail-to"], watchConf, conf.Opts{Style: conf.Shell}, "MAIL_TO", v["mail-to"]),
				{Why: "schedule", Argv: []string{"sh", "-c", `printf '%s' "$1" > ` + watchCron + ` && chmod 644 ` + watchCron + ` && cat ` + watchCron, "sh", watchCronText("/usr/local/bin/hs")}},
				{Why: "one run now: what it finds (accept the known state with Security watch: accept current findings)", Argv: []string{self(), "watch", "--sweep", "--full"}},
			}, nil
		},
	})
	register(Op{
		ID: "watch-baseline", Title: "Security watch: accept current findings", Section: "security", Risk: Change,
		Note: "Records every current watch finding as known. From then on only new findings are reported and mailed. Accept only after reading the list below — a real intrusion accepted here goes quiet.",
		How:  "`hs watch --baseline` runs every watch check (sweeps included) and stores the keys of what it finds in /var/lib/hs/watch/state.json. Findings keep showing in hs check until they are fixed; the watch just stops calling them new.",
		Undo: "Accept again later; or rm /var/lib/hs/watch/state.json to treat everything as new.",
		Plan: func(_ context.Context, env *check.Env, _ Target, _ Values) ([]Step, error) {
			rs, _ := state.Load()
			want := map[string]bool{}
			for _, id := range append(append([]string{}, watch.Fast...), watch.Sweep...) {
				want[id] = true
			}
			var sel []check.Result
			for _, r := range rs {
				if want[r.Check] {
					sel = append(sel, r)
				}
			}
			var list []string
			for _, f := range watch.Findings(sel, env.Sys.Now()) {
				list = append(list, strings.ToUpper(f.State)+" "+f.Check+" "+f.Subject+" — "+f.Summary)
			}
			why := "accept what the watch finds now"
			if len(list) > 0 {
				why += "; from the last saved results:\n" + strings.Join(list, "\n")
			}
			return []Step{{Why: why, Argv: []string{self(), "watch", "--baseline"}}}, nil
		},
	})
	register(Op{
		ID: "cron-accept", Title: "Accept the current cron entries", Section: "security", Risk: Change,
		Resolves: "persist.cron", Applies: summaryHas("none reviewed"),
		Note:    "Marks every cron entry now on the box (except Hestia's own v-* jobs) as reviewed; a new one becomes a finding. Read the list first.",
		How:     "Writes " + box.AcceptedCron + ": one line per entry — a short hash of file + line, then the line itself for reading.",
		Undo:    "rm " + box.AcceptedCron,
		Recheck: []string{"persist.cron"},
		Plan: func(_ context.Context, env *check.Env, _ Target, _ Values) ([]Step, error) {
			es := box.CronEntries(env)
			if len(es) == 0 {
				return nil, nil
			}
			args := []string{"sh", "-c", `install -d -m 700 /etc/hs && umask 077 && printf '%s\n' "$@" > ` + box.AcceptedCron + ` && cat ` + box.AcceptedCron, "sh"}
			var list []string
			for _, e := range es {
				args = append(args, e.ID()+" "+e.Where+": "+e.Line)
				list = append(list, e.Where+": "+e.Line)
			}
			return []Step{{Why: "accepting:\n" + strings.Join(list, "\n"), Argv: args}}, nil
		},
	})
	register(Op{
		ID: "persist-allow", Title: "Allow a known unpackaged file", Section: "security", Risk: Change,
		Note:    "For a binary, unit, sudoers file or packaged file you know and changed on purpose (a hand-installed tool, restic after self-update). The persistence checks stop reporting it. Never for something you cannot explain.",
		How:     "Appends the path (or a directory ending in /) to /etc/hs/unowned.allowed; every persistence check skips it.",
		Undo:    "Remove the line from /etc/hs/unowned.allowed.",
		Fields:  []Field{{Key: "path", Label: "Path", Kind: Text, Pattern: absPath, Help: "Absolute; end with / for a whole directory."}},
		Recheck: []string{"persist.unowned", "persist.units", "persist.processes", "persist.accounts", "pkg.integrity"},
		Plan: func(_ context.Context, env *check.Env, _ Target, v Values) ([]Step, error) {
			if !exists(env, strings.TrimSuffix(v["path"], "/")) {
				return nil, fmt.Errorf("%s does not exist", v["path"])
			}
			return []Step{{Why: "allow " + v["path"], Argv: []string{"sh", "-c", `install -d -m 700 /etc/hs && echo "$1" >> /etc/hs/unowned.allowed && cat /etc/hs/unowned.allowed`, "sh", v["path"]}}}, nil
		},
	})
}
