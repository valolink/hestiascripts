package box

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/hestia"
)

func securityChecks() []Check {
	return []Check{
		{ID: "firewall", Section: "security", Title: "Firewall", Run: checkFirewall},
		{ID: "fail2ban", Section: "security", Title: "Fail2ban", Run: checkFail2ban},
		{ID: "ssh.auth", Section: "security", Title: "SSH authentication", Run: checkSSH},
		{ID: "updates.unattended", Section: "security", Title: "Unattended upgrades", Run: checkUnattended},
		{ID: "updates.pending", Section: "security", Title: "Security updates", Timeout: 30 * time.Second, Run: checkPendingUpdates},
		{ID: "updates.reboot", Section: "security", Title: "Reboot", Run: checkReboot},
		{ID: "hestia.version", Section: "security", Title: "HestiaCP version", Run: checkHestiaVersion},
		{ID: "hestia.services", Section: "system", Title: "hestia.conf services", Run: checkServiceDrift},
		{ID: "maldet", Section: "security", Title: "Maldet", Run: checkMaldet},
		{ID: "web.exposure", Section: "security", Title: "Exposed files in web roots", Timeout: 30 * time.Second, Run: checkExposure},
		{ID: "web.uploads-php", Section: "security", Title: "PHP files in uploads", Timeout: 30 * time.Second, Run: checkUploadsPHP},
	}
}

// kuumalahde 2026-08-28: fail2ban green, every ban failing — no iptables
// binary, so the firewall held zero rules and nothing said so anywhere.
func checkFirewall(ctx context.Context, env *Env) []Result {
	s := env.Sys
	if !s.Have("iptables") {
		return []Result{New(Fail, "iptables is not installed").
			Because("Hestia's firewall cannot load any rules and fail2ban cannot ban anything — every port is open.").
			Fixed("apt install -y iptables && systemctl start hestia-iptables && systemctl restart fail2ban")}
	}
	out, err := s.Run(ctx, "iptables", "-S")
	if err != nil {
		return []Result{New(Unknown, "iptables -S failed: "+err.Error())}
	}
	rules := len(strings.Split(strings.TrimSpace(out), "\n"))
	var rs []Result
	if rules < 5 {
		rs = append(rs, New(Fail, fmt.Sprintf("loaded but empty (%d rules)", rules)).
			Because("Hestia's rules.conf is not applied, so every port is reachable regardless of what the panel shows.").
			Fixed("systemctl start hestia-iptables   # or: v-update-firewall"))
	}
	if unitExists(ctx, s, "hestia-iptables") && !active(ctx, s, "hestia-iptables") {
		rs = append(rs, New(Fail, "hestia-iptables.service is not active").
			Because("Firewall rules are not reapplied at boot, so a reboot silently drops the firewall.").
			Fixed("systemctl status hestia-iptables"))
	}
	if len(rs) > 0 {
		return rs
	}
	return []Result{New(OK, fmt.Sprintf("%d rules loaded", rules)).Ev("iptables -S: " + strconv.Itoa(rules) + " lines")}
}

// Green needs a ban that went through since the daemon started: "running" is
// not "able to ban". Only log lines since the last start count, or a fixed
// fault would keep reporting for weeks.
func checkFail2ban(ctx context.Context, env *Env) []Result {
	s := env.Sys
	if !s.Have("fail2ban-client") {
		return []Result{New(Warn, "not installed").
			Because("Brute-force attempts against SSH and WordPress logins go unthrottled.").
			Fixed("run.sh → 4 (Fail2ban)")}
	}
	if !active(ctx, s, "fail2ban") {
		return []Result{New(Fail, "installed but not running").
			Because("Brute-force attempts against SSH, WordPress and FTP go unthrottled.").
			Fixed("systemctl start fail2ban")}
	}
	var rs []Result
	// IPC to the daemon blocks when an action is stalled — bound it tightly.
	jctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_, jerr := s.Run(jctx, "fail2ban-client", "status", "wordpress")
	stalled := jctx.Err() == context.DeadlineExceeded
	cancel()
	switch {
	case stalled:
		rs = append(rs, New(Warn, "daemon IPC unresponsive").
			Because("fail2ban-client did not answer in 3s — usually a ban or mail action stalled on SMTP or rDNS.").
			Fixed("systemctl restart fail2ban; tail /var/log/fail2ban.log"))
	case jerr != nil:
		rs = append(rs, New(Warn, "no WordPress jail").
			Because("wp-login.php and xmlrpc.php brute force is not throttled.").
			Fixed("run.sh → 4 (Fail2ban) → 1"))
	}

	since := activeSince(ctx, s, "fail2ban")
	bans, failures := 0, 0
	var lastBan string
	for _, line := range strings.Split(readString(s, "/var/log/fail2ban.log"), "\n") {
		if len(line) < 19 {
			continue
		}
		ts, err := time.ParseInLocation("2006-01-02 15:04:05", line[:19], time.Local)
		if err != nil || (!since.IsZero() && ts.Before(since)) {
			continue
		}
		switch {
		case strings.Contains(line, "Failed to execute ban"):
			failures++
		case strings.Contains(line, "] Ban ") || strings.Contains(line, "] Restore Ban "):
			bans++
			lastBan = line[:19]
		}
	}
	started := "unknown"
	if !since.IsZero() {
		started = since.Format("2006-01-02 15:04")
	}
	if failures > 0 {
		return append(rs, New(Fail, fmt.Sprintf("%s failed since it started", plural(failures, "ban action", "ban actions"))).
			Ev("daemon started "+started, fmt.Sprintf("%d bans, %d failed", bans, failures)).
			Because("The jails detect attacks and then silently fail to block them — the daemon still reports itself healthy.").
			Fixed("grep 'Failed to execute ban' /var/log/fail2ban.log | tail -3"))
	}
	if len(rs) > 0 {
		return rs
	}
	if bans == 0 {
		return []Result{New(Configured, "running, WordPress jail active, no ban since it started").
			Ev("daemon started " + started).
			Because("Nothing has been banned since the last start, so ban actions are unproven.")}
	}
	return []Result{New(OK, fmt.Sprintf("%d bans since start, none failed", bans)).
		Ev("daemon started "+started, "last ban "+lastBan).Valid(7 * 24 * time.Hour)}
}

func checkSSH(ctx context.Context, env *Env) []Result {
	out, err := env.Sys.Run(ctx, "sshd", "-T")
	if err != nil {
		return []Result{New(Unknown, "sshd -T failed")}
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "passwordauthentication" {
			if f[1] == "no" {
				return []Result{New(OK, "key-only (effective sshd config)").Ev("sshd -T: passwordauthentication no")}
			}
			return []Result{New(Warn, "accepts password authentication").
				Ev("sshd -T: passwordauthentication " + f[1]).
				Because("Every exposed box gets continuous SSH brute force; keys remove the attack entirely.").
				Fixed("run.sh → 7 (Security) → 3   # confirm your key works BEFORE disabling passwords")}
		}
	}
	return []Result{New(Unknown, "passwordauthentication not in sshd -T output")}
}

// UnattendedScope is "missing", "unknown" (no config), "all" or "security-only".
// Shared with the setup-status compat output.
func UnattendedScope(ctx context.Context, env *Env) string {
	if !pkgInstalled(ctx, env.Sys, "unattended-upgrades") {
		return "missing"
	}
	conf := readString(env.Sys, "/etc/apt/apt.conf.d/50unattended-upgrades")
	if conf == "" {
		return "unknown"
	}
	for _, line := range strings.Split(conf, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, `"origin=Debian`) && strings.HasSuffix(t, `,label=Debian";`) {
			return "all"
		}
	}
	return "security-only"
}

func checkUnattended(ctx context.Context, env *Env) []Result {
	switch UnattendedScope(ctx, env) {
	case "missing":
		return []Result{New(Warn, "not installed").
			Because("Security patches wait for someone to remember.").Fixed("run.sh → 7 (Security) → 1")}
	case "unknown":
		return []Result{New(Warn, "installed but has no config file").
			Because("The package is installed but applies nothing.").Fixed("run.sh → 7 (Security) → 1")}
	case "all":
		return []Result{New(Fail, "applies all Debian updates, not only security").
			Because("Unattended non-security upgrades can restart or break Hestia's stack without anyone watching.").
			Fixed("run.sh → 7 (Security) → 1 → 2 (security updates only)")}
	}
	// Proof it runs: its log shows a completed run in the last 2 days.
	last := lastUnattendedRun(env)
	if last.IsZero() {
		return []Result{New(Configured, "security-only, no completed run found in its log")}
	}
	age := env.Sys.Now().Sub(last)
	if age > 48*time.Hour {
		return []Result{New(Warn, fmt.Sprintf("security-only, but the last run was %d days ago", int(age.Hours()/24))).
			Because("The apt timers are not firing, so patches are not applied.").
			Fixed("systemctl status apt-daily-upgrade.timer")}
	}
	return []Result{New(OK, "security-only, last ran "+last.Format("2006-01-02 15:04")).Valid(48 * time.Hour)}
}

func lastUnattendedRun(env *Env) time.Time {
	var last time.Time
	for _, line := range strings.Split(readString(env.Sys, "/var/log/unattended-upgrades/unattended-upgrades.log"), "\n") {
		if len(line) < 19 || !strings.Contains(line, "INFO") {
			continue
		}
		if ts, err := time.ParseInLocation("2006-01-02 15:04:05", line[:19], time.Local); err == nil && ts.After(last) {
			last = ts
		}
	}
	return last
}

func checkPendingUpdates(ctx context.Context, env *Env) []Result {
	out, err := env.Sys.Run(ctx, "apt-get", "-s", "upgrade")
	if err != nil {
		return []Result{New(Unknown, "apt-get -s upgrade failed: "+err.Error())}
	}
	var sec []string
	total := 0
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "Inst ") {
			continue
		}
		total++
		if strings.Contains(line, "security") {
			sec = append(sec, strings.Fields(line)[1])
		}
	}
	if len(sec) > 0 {
		return []Result{New(Warn, plural(len(sec), "security update pending", "security updates pending")).
			Ev(joinMax(sec, 8)).
			Because("unattended-upgrades is security-only, so anything still pending needs a hand.").
			Fixed("apt-get upgrade   (review first: apt-get -s upgrade | grep ^Inst)")}
	}
	return []Result{New(OK, fmt.Sprintf("no security updates pending (%d other)", total))}
}

func checkReboot(ctx context.Context, env *Env) []Result {
	fi, err := env.Sys.Stat("/var/run/reboot-required")
	if err != nil {
		return []Result{New(OK, "no reboot required")}
	}
	pkgs := strings.Fields(readString(env.Sys, "/var/run/reboot-required.pkgs"))
	return []Result{New(Warn, fmt.Sprintf("reboot required since %s", fi.ModTime().Format("2006-01-02"))).
		Ev(joinMax(pkgs, 6)).
		Because("A kernel or core library was updated; the running system is still on the old one.").
		Fixed("safe-reboot   # staged, guarded reboot (SSH only)")}
}

// HestiaLatest asks GitHub for the latest HestiaCP release ("" when offline).
func HestiaLatest(ctx context.Context, env *Env) string {
	hctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	code, body, err := env.Sys.HTTPGet(hctx, "https://api.github.com/repos/hestiacp/hestiacp/releases/latest", nil)
	if err != nil || code != 200 {
		return ""
	}
	var rel struct {
		Tag string `json:"tag_name"`
	}
	if json.Unmarshal(body, &rel) != nil {
		return ""
	}
	return strings.TrimPrefix(rel.Tag, "v")
}

func checkHestiaVersion(ctx context.Context, env *Env) []Result {
	conf := hestia.Conf(env.Sys)
	cur := conf["VERSION"]
	if cur == "" {
		return []Result{New(Unknown, "installed version not found in hestia.conf")}
	}
	var rs []Result
	latest := HestiaLatest(ctx, env)
	switch {
	case latest == "":
		rs = append(rs, New(Unknown, cur+" installed; latest release unknown (GitHub unreachable)"))
	case latest != cur:
		rs = append(rs, New(Warn, cur+" is behind "+latest).
			Because("Panel security fixes ship in point releases.").Fixed("v-update-sys-hestia-all"))
	default:
		rs = append(rs, New(OK, cur+" (latest)"))
	}
	return rs
}

func checkServiceDrift(ctx context.Context, env *Env) []Result {
	if rs := ServiceDriftResults(ctx, env); len(rs) > 0 {
		return rs
	}
	return []Result{New(OK, "every *_SYSTEM service in hestia.conf is installed")}
}

// Drift is a *_SYSTEM key in hestia.conf naming a service whose package is gone.
type Drift struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	Package string `json:"package"`
}

var driftTable = []struct {
	key    string
	values []string
	pkgs   []string
}{
	{"IMAP_SYSTEM", []string{"dovecot"}, []string{"dovecot-core"}},
	{"ANTIVIRUS_SYSTEM", []string{"clamav-daemon", "clamav", "clamd"}, []string{"clamav-daemon", "clamav", "clamav-base"}},
	{"ANTISPAM_SYSTEM", []string{"spamassassin", "spamd"}, []string{"spamassassin"}},
	{"FTP_SYSTEM", []string{"vsftpd"}, []string{"vsftpd"}},
	{"FTP_SYSTEM", []string{"proftpd"}, []string{"proftpd"}},
	{"MAIL_SYSTEM", []string{"exim4"}, []string{"exim4"}},
}

func ServiceDrift(ctx context.Context, env *Env) []Drift {
	conf := hestia.Conf(env.Sys)
	var out []Drift
	for _, d := range driftTable {
		cur := conf[d.key]
		if cur == "" {
			continue
		}
		for _, v := range d.values {
			if cur == v && !pkgInstalled(ctx, env.Sys, d.pkgs...) {
				out = append(out, Drift{Key: d.key, Value: cur, Package: d.pkgs[0]})
			}
		}
	}
	return out
}

func ServiceDriftResults(ctx context.Context, env *Env) []Result {
	var rs []Result
	for _, d := range ServiceDrift(ctx, env) {
		rs = append(rs, New(Warn, fmt.Sprintf("hestia.conf %s='%s' but %s is not installed", d.Key, d.Value, d.Package)).
			For(d.Key).
			Because("Hestia keeps trying to restart a service that no longer exists and emails root every time.").
			Fixed(fmt.Sprintf(`v-change-sys-config-value %s ""   # or run.sh → 13 → 6`, d.Key)))
	}
	return rs
}

// MaldetLastScan returns the last completed scan as the event log's first two
// fields ("Sep 24" — the shape setup-status has always reported) and, when the
// line carries a time as well, the parsed moment.
func MaldetLastScan(env *Env) (stamp string, at time.Time) {
	for _, line := range strings.Split(readString(env.Sys, "/usr/local/maldetect/logs/event_log"), "\n") {
		if !strings.Contains(line, "scan completed") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		stamp = f[0] + " " + f[1]
		at, _ = parseMaldetStamp(strings.Join(f[:min(4, len(f))], " "), env.Sys.Now())
	}
	return stamp, at
}

func checkMaldet(ctx context.Context, env *Env) []Result {
	s := env.Sys
	if !exists(s, "/usr/local/maldetect/maldet") {
		return []Result{New(Warn, "not installed").
			Because("Nothing scans customer sites for injected shells.").Fixed("run.sh → 5 (Maldet) → 1")}
	}
	var rs []Result
	if !active(ctx, s, "maldet") {
		rs = append(rs, New(Warn, "maldet.service is not running").
			Because("Monitor mode is off, so new files are not scanned as they land.").
			Fixed("systemctl status maldet   # missing deps are usually 'ed' or inotify-tools"))
	}
	stamp, ts := MaldetLastScan(env)
	if stamp == "" {
		return append(rs, New(Warn, "has never completed a scan").
			Because("Installed but unproven — no baseline and no idea of its false-positive rate here.").
			Fixed("run.sh → 5 (Maldet) → 3"))
	}
	if ts.IsZero() {
		return append(rs, New(Configured, "last scan "+stamp+" (time not parseable)"))
	}
	age := s.Now().Sub(ts)
	if age > 8*24*time.Hour {
		return append(rs, New(Warn, fmt.Sprintf("last scan was %d days ago", int(age.Hours()/24))).
			Because("The daily cron is not completing.").
			Fixed("cat /etc/cron.daily/maldet ; maldet -a /home/*/web/*/public_html/"))
	}
	if len(rs) > 0 {
		return rs
	}
	return []Result{New(OK, "last scan "+ts.Format("2006-01-02 15:04")).Valid(8 * 24 * time.Hour)}
}

func parseMaldetStamp(v string, now time.Time) (time.Time, bool) {
	// maldet 1.6 writes "Sep 24 2026 06:30:23"; older builds drop the year.
	fields := strings.Fields(v)
	for _, layout := range []string{"Jan 2 2006 15:04:05", "Jan 2 15:04:05"} {
		n := len(strings.Fields(layout))
		if len(fields) < n {
			continue
		}
		ts, err := time.ParseInLocation(layout, strings.Join(fields[:n], " "), time.Local)
		if err != nil {
			continue
		}
		if ts.Year() == 0 {
			ts = ts.AddDate(now.Year(), 0, 0)
			if ts.After(now.Add(24 * time.Hour)) {
				ts = ts.AddDate(-1, 0, 0)
			}
		}
		return ts, true
	}
	return time.Time{}, false
}

// kuumalahde 2026-08-28: a 52 MB debug.log served over HTTPS. alavus
// 2026-09-24: two 593 MB .sql dumps in the docroot. Check for the files,
// not for a deny rule that may or may not be deployed.
func checkExposure(ctx context.Context, env *Env) []Result {
	s := env.Sys
	var rs []Result
	logs, _ := s.Glob("/home/*/web/*/public_html/wp-content/debug*.log")
	for _, f := range logs {
		fi, err := s.Stat(f)
		if err != nil {
			continue
		}
		dom := strings.Split(f, "/")[4]
		state := Warn
		if fi.Size() > 1<<30 {
			state = Fail
		}
		rs = append(rs, New(state, fmt.Sprintf("%s is %s inside the web root", filepath.Base(f), humanBytes(fi.Size()))).
			For(dom).Ev(f).
			Because("Debug logs carry paths, plugin internals and tokens, are served unless a deny rule is actually deployed, and grow without bound.").
			Fixed("set WP_DEBUG false (or point WP_DEBUG_LOG at <domain>/private/), then remove the file"))
	}

	roots := webRoots(env)
	if len(roots) == 0 {
		return []Result{New(NA, "no web roots")}
	}
	out, _ := s.Run(ctx, "find", append(roots, "-maxdepth", "2", "(",
		"-name", "wp-config*.bak*", "-o", "-name", "wp-config*.save", "-o", "-name", "wp-config*.old",
		"-o", "-name", "*.sql", "-o", "-name", "*.sql.gz", ")", "-type", "f")...)
	dumps := map[string][]string{}
	var doms []string
	for _, f := range nonEmpty(out) {
		dom := strings.Split(f, "/")[4]
		if _, seen := dumps[dom]; !seen {
			doms = append(doms, dom)
		}
		dumps[dom] = append(dumps[dom], f[strings.Index(f, "/public_html/")+len("/public_html/"):])
	}
	for _, dom := range doms {
		rs = append(rs, New(Fail, plural(len(dumps[dom]), "database dump or wp-config backup", "database dumps or wp-config backups")+" in the web root").
			For(dom).Ev(dumps[dom]...).
			Because("They hand database credentials or the whole dataset to anyone who guesses the name, and scanners guess constantly.").
			Fixed("move them outside public_html (or delete them after checking)"))
	}
	if len(rs) == 0 {
		return []Result{New(OK, "no debug logs or dumps in any web root")}
	}
	return rs
}

// alavus 2026-09-24: PHP executed from uploads/, the second half of an
// unauthenticated upload → RCE chain. Any .php there other than index.php
// stubs is suspect.
func checkUploadsPHP(ctx context.Context, env *Env) []Result {
	var roots []string
	for _, r := range webRoots(env) {
		if exists(env.Sys, r+"/wp-content/uploads") {
			roots = append(roots, r+"/wp-content/uploads")
		}
	}
	if len(roots) == 0 {
		return []Result{New(NA, "no WordPress uploads directories")}
	}
	out, _ := env.Sys.Run(ctx, "find", append(roots, "-type", "f", "(", "-iname", "*.php", "-o", "-iname", "*.phtml", "-o", "-iname", "*.phar", ")", "!", "-name", "index.php")...)
	by := map[string][]string{}
	var doms []string
	for _, f := range nonEmpty(out) {
		dom := strings.Split(f, "/")[4]
		if _, ok := by[dom]; !ok {
			doms = append(doms, dom)
		}
		by[dom] = append(by[dom], strings.TrimPrefix(f, "/home/"))
	}
	var rs []Result
	for _, d := range doms {
		rs = append(rs, New(Warn, plural(len(by[d]), "PHP file in uploads", "PHP files in uploads")).
			For(d).Ev(by[d]...).
			Because("Uploads should hold media only. A PHP file there is either a plugin's test file or a dropped shell — check each one.").
			Fixed("inspect the files; block PHP execution in uploads in the web template"))
	}
	if len(rs) == 0 {
		return []Result{New(OK, "no PHP files in any uploads directory")}
	}
	return rs
}

func webRoots(env *Env) []string {
	var out []string
	for _, d := range hestia.WebDomains(env.Sys) {
		if exists(env.Sys, d.DocRoot()) {
			out = append(out, d.DocRoot())
		}
	}
	return out
}

func nonEmpty(out string) []string {
	var r []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			r = append(r, l)
		}
	}
	return r
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%d MB", n>>20)
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}
