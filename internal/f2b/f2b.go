// Package f2b is setup/fail2ban.sh's repair logic, one change at a time and
// each printed: the self-ban guard, the silenced ban mails, the xmlrpc
// filter line, and disabling jails whose log files do not exist on this box
// (fail2ban refuses to start on those, reporting one per test run).
package f2b

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/valolink/hestiascripts/internal/conf"
)

var (
	Dir      = "/etc/fail2ban"
	Jail     = "jail.d/wordpress.conf"
	Filter   = "filter.d/wordpress.conf"
	missing  = regexp.MustCompile(`any log file for (\S+) jail`)
	mailAct  = regexp.MustCompile(`%\(action_mwl?\)s`)
	sectionH = func(j string) *regexp.Regexp { return regexp.MustCompile(`(?m)^\[` + regexp.QuoteMeta(j) + `\]`) }
)

func path(rel string) string { return Dir + "/" + rel }

// Runner runs a command (tests replace it).
type Runner func(ctx context.Context, name string, args ...string) (string, error)

func Exec(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

func report(w io.Writer, what, bak string, err error) error {
	if err != nil {
		fmt.Fprintf(w, "! %s: %v\n", what, err)
		return err
	}
	fmt.Fprintf(w, "→ %s\n", what)
	if bak != "" {
		fmt.Fprintf(w, "  previous file kept: %s\n", bak)
	}
	return nil
}

// Repair applies every repair that is due and tests the config until it
// passes (at most 15 rounds — one missing-log jail per round).
func Repair(ctx context.Context, w io.Writer, run Runner, selfIPs string) error {
	// 1. Hestia writes `action = %(action_mwl)s` per jail: whois + log lines
	// mailed on every ban, to the production inbox, and the mail action
	// blocks fail2ban's command queue on SMTP. Ban-only keeps the bans.
	if b, err := os.ReadFile(path("jail.local")); err == nil && mailAct.Match(b) {
		out := mailAct.ReplaceAllString(string(b), "%(action_)s")
		bak, err := conf.Write(path("jail.local"), out, 0o644)
		if report(w, "jail.local: ban mails off (action_mwl/action_mw → action_, bans unchanged)", bak, err) != nil {
			return err
		}
	}
	// 2. Filters installed before xmlrpc.php was added.
	if b, err := os.ReadFile(path(Filter)); err == nil && !strings.Contains(string(b), "xmlrpc") {
		out := regexp.MustCompile(`(?m)^(.*wp-login\\\.php.*)$`).ReplaceAllString(string(b), "$1\n            ^<HOST> .* \"POST .*xmlrpc\\.php")
		bak, err := conf.Write(path(Filter), out, 0o644)
		if report(w, Filter+": also match POST xmlrpc.php", bak, err) != nil {
			return err
		}
	}
	// 3. Never ban the box itself: the nginx→apache hop is logged with the
	// host's own address, so a wp-login flood banned the server's IP and
	// 502'd every site (2026-07-05, 8dmeditaatiot + web1).
	if b, err := os.ReadFile(path(Jail)); err == nil {
		if _, ok := conf.Get(string(b), conf.Opts{Section: "wordpress"}, "ignoreip"); !ok {
			ch, bak, err := conf.SetFile(path(Jail), conf.Opts{Section: "wordpress", Style: conf.INI}, "ignoreip", strings.TrimSpace("127.0.0.1/8 ::1 "+selfIPs))
			what := Jail + ": never ban this box's own addresses"
			if err == nil && len(ch) > 0 {
				what += " (" + ch[0].New + ")"
			}
			if report(w, what, bak, err) != nil {
				return err
			}
		}
	}
	// 4. Test; disable each jail whose log files are missing.
	for i := 0; i < 15; i++ {
		tctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		out, err := run(tctx, "fail2ban-client", "-t")
		cancel()
		if err == nil {
			fmt.Fprintln(w, "→ fail2ban-client -t: configuration OK")
			return nil
		}
		m := missing.FindStringSubmatch(out)
		if m == nil {
			fmt.Fprintln(w, "! fail2ban-client -t still fails:")
			for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
				if !strings.Contains(l, "WARNING") {
					fmt.Fprintln(w, "    "+l)
				}
			}
			return fmt.Errorf("configuration test failed")
		}
		jail := m[1]
		var bak string
		// jail.d is read before jail.local: a jail defined in jail.local must be disabled there.
		if b, _ := os.ReadFile(path("jail.local")); sectionH(jail).Match(b) {
			_, bak, err = conf.SetFile(path("jail.local"), conf.Opts{Section: jail, Style: conf.INI}, "enabled", "false")
		} else {
			bak, err = conf.Write(path("jail.d/zzz-disable-"+jail+".conf"), "["+jail+"]\nenabled = false\n", 0o644)
		}
		os.Remove(path("jail.d/disable-" + jail + ".conf"))
		if report(w, "jail "+jail+" disabled: no log file for it on this box", bak, err) != nil {
			return err
		}
	}
	return fmt.Errorf("still failing after 15 rounds")
}

// Reload reloads, or restarts, fail2ban. A command that does not return in
// 3 s means its queue is stuck (typically a mail action blocked on SMTP):
// the server is killed and started again — in-memory bans are lost, the
// jails refill from the logs.
func Reload(ctx context.Context, w io.Writer, restart bool) error {
	verb := []string{"fail2ban-client", "reload"}
	if restart {
		verb = []string{"systemctl", "restart", "fail2ban"}
	}
	tctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	err := exec.CommandContext(tctx, verb[0], verb[1:]...).Run()
	cancel()
	if err == nil {
		fmt.Fprintf(w, "→ %s\n", strings.Join(verb, " "))
	} else {
		fmt.Fprintf(w, "! %s did not return in 3 s — killing fail2ban-server and starting it again (in-memory bans are lost; jails refill from the logs)\n", strings.Join(verb, " "))
		exec.Command("killall", "-9", "fail2ban-server").Run()
		time.Sleep(time.Second)
		if err := exec.CommandContext(ctx, "systemctl", "restart", "fail2ban").Run(); err != nil {
			return fmt.Errorf("fail2ban did not come back: journalctl -u fail2ban")
		}
	}
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(sctx, "fail2ban-client", "status").CombinedOutput()
	if err != nil {
		return fmt.Errorf("fail2ban is not answering: journalctl -u fail2ban")
	}
	fmt.Fprint(w, string(out))
	return nil
}
