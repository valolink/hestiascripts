package box

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	. "github.com/valolink/hestiascripts/internal/check"
)

func mailChecks() []Check {
	return []Check{
		{ID: "mail.queue", Section: "mail", Title: "Mail queue", Run: checkMailQueue},
		{ID: "mail.delivery", Section: "mail", Title: "Outbound mail", Timeout: 20 * time.Second, Run: checkMailDelivery},
	}
}

var queueIDRe = regexp.MustCompile(`^[A-F0-9]{8,}`)

// MailQueue returns the number of queued messages (postfix, exim fallback).
func MailQueue(ctx context.Context, env *Env) (int, bool) {
	if env.Sys.Have("mailq") {
		out, err := env.Sys.Run(ctx, "mailq")
		if err != nil && out == "" {
			return 0, false
		}
		n := 0
		for _, l := range strings.Split(out, "\n") {
			if queueIDRe.MatchString(l) {
				n++
			}
		}
		return n, true
	}
	if env.Sys.Have("exim") {
		out, err := env.Sys.Run(ctx, "exim", "-bpc")
		if err != nil {
			return 0, false
		}
		var n int
		fmt.Sscanf(strings.TrimSpace(out), "%d", &n)
		return n, true
	}
	return 0, false
}

func checkMailQueue(ctx context.Context, env *Env) []Result {
	n, ok := MailQueue(ctx, env)
	switch {
	case !ok:
		return []Result{New(NA, "no MTA")}
	case n > 50:
		return []Result{New(Warn, fmt.Sprintf("%d messages stuck", n)).
			Because("Alerts from cron, maldet, fail2ban and WordPress are not reaching anyone.").
			Fixed("mailq | tail ; run.sh → 8 (SMTP)")}
	}
	return []Result{New(OK, fmt.Sprintf("%d queued", n))}
}

// Relay returns postfix's relayhost ("" when none).
func Relay(ctx context.Context, env *Env) string {
	if !env.Sys.Have("postconf") {
		return ""
	}
	out, _ := env.Sys.Run(ctx, "postconf", "-h", "relayhost")
	r := strings.TrimSpace(out)
	if r == "[]" {
		return ""
	}
	return r
}

// Green needs a message actually delivered recently: a configured relay with
// bad credentials looks identical to a working one until you read the log.
func checkMailDelivery(ctx context.Context, env *Env) []Result {
	if !env.Sys.Have("postconf") {
		return []Result{New(NA, "postfix not installed")}
	}
	// hzdemolink 2026-09-24: postfix stopped, 59 messages queued, and the
	// relay line still read "configured".
	if unitExists(ctx, env.Sys, "postfix") && !active(ctx, env.Sys, "postfix") {
		n, _ := MailQueue(ctx, env)
		r := New(Fail, fmt.Sprintf("postfix is not running (%d messages waiting)", n))
		// hzweb1 / kuumalahde 2026-09-24: stopped for weeks unnoticed.
		for _, unit := range []string{"postfix@-", "postfix"} {
			if out, err := env.Sys.Run(ctx, "systemctl", "show", unit, "-p", "InactiveEnterTimestamp", "--value", "--timestamp=unix"); err == nil {
				var sec int64
				if _, e := fmt.Sscanf(strings.TrimPrefix(strings.TrimSpace(out), "@"), "%d", &sec); e == nil && sec > 0 {
					since := time.Unix(sec, 0)
					r = r.Ev(fmt.Sprintf("stopped since %s — %d days", since.Local().Format("2006-01-02 15:04"), int(env.Sys.Now().Sub(since).Hours()/24)))
					break
				}
			}
		}
		return []Result{r.
			Because("Nothing leaves the box: cron, fail2ban and maldet alerts and all WordPress mail sit in the queue.").
			Fixed("systemctl status postfix ; systemctl start postfix")}
	}
	relay := Relay(ctx, env)
	var rs []Result
	if relay == "" {
		rs = append(rs, New(Warn, "no SMTP relay configured").
			Because("Mail sent straight from the box lands in spam or is dropped.").Fixed("run.sh → 8 (SMTP)"))
	}
	lastSent, lastFail, failReason := lastDelivery(ctx, env)
	now := env.Sys.Now()
	switch {
	case !lastFail.IsZero() && lastFail.After(lastSent):
		rs = append(rs, New(Fail, "the most recent delivery attempt failed").
			Ev(failReason, "last success: "+stampOrNever(lastSent)).
			Because("Notifications and WordPress mail (orders, password resets) are not arriving.").
			Fixed("tail -50 /var/log/mail.log ; run.sh → 8 (SMTP) → 4 (test)"))
	case lastSent.IsZero():
		if relay != "" {
			rs = append(rs, New(Configured, "relay "+relay+", no delivery seen in the last 7 days").
				Fixed("run.sh → 8 (SMTP) → 4 (send a test)"))
		}
	case len(rs) == 0:
		age := now.Sub(lastSent)
		rs = append(rs, New(OK, fmt.Sprintf("delivered via %s, last %s ago", orDash(relay), shortAgo(age))).
			Ev("last status=sent "+lastSent.Format("2006-01-02 15:04")).Valid(7*24*time.Hour))
	}
	return rs
}

func stampOrNever(t time.Time) string {
	if t.IsZero() {
		return "none in the last 7 days"
	}
	return t.Format("2006-01-02 15:04")
}

func shortAgo(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 48*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

var statusRe = regexp.MustCompile(`status=(sent|bounced|deferred)`)

// lastDelivery reads postfix/smtp outcomes from the journal (always present
// on Debian 12, and rotation-proof), falling back to /var/log/mail.log.
func lastDelivery(ctx context.Context, env *Env) (sent, failed time.Time, reason string) {
	out, err := env.Sys.Run(ctx, "journalctl", "--since=-7d", "--no-pager", "-o", "short-unix",
		"SYSLOG_IDENTIFIER=postfix/smtp")
	if err == nil && strings.TrimSpace(out) != "" && !strings.Contains(out, "-- No entries --") {
		for _, l := range strings.Split(out, "\n") {
			m := statusRe.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			var sec float64
			fmt.Sscanf(l, "%f", &sec)
			ts := time.Unix(int64(sec), 0)
			if m[1] == "sent" {
				sent = ts
			} else {
				failed, reason = ts, trimReason(l)
			}
		}
		return
	}
	now := env.Sys.Now()
	for _, f := range []string{"/var/log/mail.log.1", "/var/log/mail.log"} {
		for _, l := range strings.Split(readString(env.Sys, f), "\n") {
			m := statusRe.FindStringSubmatch(l)
			if m == nil || !strings.Contains(l, "postfix/smtp[") {
				continue
			}
			ts, ok := syslogTime(l, now)
			if !ok || now.Sub(ts) > 7*24*time.Hour {
				continue
			}
			if m[1] == "sent" {
				sent = ts
			} else {
				failed, reason = ts, trimReason(l)
			}
		}
	}
	return
}

func trimReason(l string) string {
	if i := strings.Index(l, "status="); i >= 0 {
		l = l[i:]
	}
	if len(l) > 160 {
		l = l[:160] + "…"
	}
	return l
}

// syslogTime parses both rsyslog formats: RFC 3339 (Debian 12 default) and
// the traditional "Sep 24 07:34:04".
func syslogTime(l string, now time.Time) (time.Time, bool) {
	if f := strings.Fields(l); len(f) > 0 {
		if ts, err := time.Parse(time.RFC3339Nano, f[0]); err == nil {
			return ts, true
		}
	}
	if len(l) >= 15 {
		if ts, err := time.ParseInLocation("Jan _2 15:04:05", l[:15], time.Local); err == nil {
			ts = ts.AddDate(now.Year(), 0, 0)
			if ts.After(now.Add(24 * time.Hour)) {
				ts = ts.AddDate(-1, 0, 0)
			}
			return ts, true
		}
	}
	return time.Time{}, false
}
