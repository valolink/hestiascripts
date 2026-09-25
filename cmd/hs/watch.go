package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/check/box"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/state"
	"github.com/valolink/hestiascripts/internal/sys"
	"github.com/valolink/hestiascripts/internal/watch"
)

const watchConf = "/etc/hs/watch.conf"

// cmdWatch: the security checks, reporting only what is new since the last
// accepted state. Read-only on the box except its own state; --mail alerts.
func cmdWatch(ctx context.Context, env *check.Env, args []string) int {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	sweep := fs.Bool("sweep", false, "also run the filesystem-wide checks (hourly from cron)")
	baseline := fs.Bool("baseline", false, "accept everything currently found as known (review with --full first)")
	mail := fs.Bool("mail", false, "mail new findings (at most once a day each) to MAIL_TO in "+watchConf)
	full := fs.Bool("full", false, "list every finding, accepted ones too")
	quiet := fs.Bool("quiet", false, "print only new findings (and the JSON line)")
	fs.Parse(args)

	want := map[string]bool{}
	for _, id := range watch.Fast {
		want[id] = true
	}
	sweepIDs := map[string]bool{}
	for _, id := range watch.Sweep {
		sweepIDs[id] = true
		if *sweep || *baseline || *full {
			want[id] = true
		}
	}
	var checks []check.Check
	for _, c := range box.All() {
		if want[c.ID] {
			checks = append(checks, c)
		}
	}
	prev, _ := state.Load()
	rs := check.RunCached(ctx, env, checks, prev, false, nil)
	now := env.Sys.Now()
	host, _ := os.Hostname()
	if err := state.Save(host, check.Merge(prev, rs), now); err != nil && !*quiet {
		fmt.Fprintln(os.Stderr, "hs watch: could not save results:", err)
	}
	// Sweep findings between sweeps come from the last sweep's results.
	all := rs
	if !(*sweep || *baseline || *full) {
		for _, r := range prev {
			if sweepIDs[r.Check] {
				all = append(all, r)
			}
		}
	}
	findings := watch.Findings(all, now)

	dir := watch.Dir(state.Dir())
	st := watch.Load(dir)
	if *baseline {
		st.Accept(findings)
		st.Alerted = map[string]time.Time{}
		if err := st.Save(dir); err != nil {
			fmt.Fprintln(os.Stderr, "hs watch:", err)
			return 1
		}
		fmt.Printf("accepted %d finding(s) as known; from now on only new ones are reported\n", len(findings))
		for _, f := range findings {
			fmt.Printf("  %-4s %s %s — %s\n", f.State, f.Check, f.Subject, f.Summary)
		}
		return 0
	}
	newF := st.New(findings)
	st.Forget(findings)

	// A mail that failed last time is itself a finding: a broken relay must
	// not silence the watch.
	var mailErr string
	if st.MailErr != "" {
		newF = append(newF, watch.Finding{Key: "watch.mail", Check: "watch.mail", Summary: "the last alert mail could not be sent (" + st.MailAt.Format("2006-01-02 15:04") + "): " + st.MailErr, State: "warn"})
	}
	if *mail {
		if due := st.Due(newF, now); len(due) > 0 {
			to := hestia.ParseKV(strings.ReplaceAll(readFileString(watchConf), "\n", " "))["MAIL_TO"]
			if to == "" {
				to = watch.DefaultMailTo
			}
			subj, body := watch.Mail(host, due)
			if err := sendMail(ctx, to, subj, body); err != nil {
				st.MailErr, st.MailAt, mailErr = err.Error(), now, err.Error()
			} else {
				st.MailErr = ""
				for _, f := range due {
					st.Alerted[f.Key] = now
				}
			}
		}
	}
	st.Save(dir)

	show := newF
	if *full {
		show = findings
	}
	if !*quiet || len(newF) > 0 {
		if len(show) == 0 {
			fmt.Printf("%s: nothing new (%d known finding(s) accepted)\n", host, len(st.Baseline))
		}
		for _, f := range show {
			mark := "NEW "
			if st.Baseline[f.Key] {
				mark = "    "
			}
			fmt.Printf("%s%-4s %s", mark, strings.ToUpper(f.State), f.Check)
			if f.Subject != "" {
				fmt.Printf(" — %s", f.Subject)
			}
			fmt.Printf(": %s\n", f.Summary)
		}
	}
	sum := watch.Summarise(newF, len(st.Baseline), mailErr, now)
	b, _ := json.Marshal(sum)
	fmt.Println(string(b))
	switch {
	case sum.Critical > 0:
		return 2
	case sum.Warn > 0:
		return 1
	}
	return 0
}

func readFileString(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func sendMail(ctx context.Context, to, subject, body string) error {
	mctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if (sys.Real{}).Have("mail") {
		cmd = exec.CommandContext(mctx, "mail", "-s", subject, to)
	} else {
		cmd = exec.CommandContext(mctx, "sendmail", "-t")
		body = "To: " + to + "\nSubject: " + subject + "\n\n" + body
	}
	cmd.Stdin = strings.NewReader(body)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
