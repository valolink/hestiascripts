package box

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/wpfiles"
)

func securityChecks() []Check {
	return []Check{
		{ID: "firewall", Section: "security", Title: "Firewall", Run: checkFirewall},
		{ID: "fail2ban", Section: "security", Title: "Fail2ban", Run: checkFail2ban},
		{ID: "ssh.auth", Section: "security", Title: "SSH authentication", Run: checkSSH},
		{ID: "ssh.private-keys", Section: "security", Title: "Private keys on the server", Run: checkPrivateKeys},
		{ID: "ssh.authorized-keys", Section: "security", Title: "Authorized SSH keys", Run: checkAuthorizedKeys},
		{ID: "updates.unattended", Section: "security", Title: "Unattended upgrades", Run: checkUnattended},
		{ID: "updates.pending", Section: "security", Title: "Security updates", Timeout: 30 * time.Second, Run: checkPendingUpdates},
		{ID: "updates.reboot", Section: "security", Title: "Reboot", Run: checkReboot},
		{ID: "hestia.version", Section: "security", Title: "HestiaCP version", Run: checkHestiaVersion},
		{ID: "hestia.web-terminal", Section: "security", Title: "Hestia web terminal", Run: checkWebTerminal},
		{ID: "hestia.advisories", Section: "security", Title: "Hestia security advisories", MinInterval: 12 * time.Hour, Run: checkAdvisories},
		{ID: "hestia.api", Section: "security", Title: "Hestia API", Run: checkAPI},
		{ID: "hestia.2fa", Section: "security", Title: "Panel two-factor login", Run: check2FA},
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
			Fixed("apt-get install fail2ban, then hs op f2b-wp-jail")}
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
			Fixed("hs op f2b-wp-jail"))
	}
	// The self-ban guard: the nginx→apache hop is logged with the box's own
	// address, so a wp-login flood without ignoreip bans the server itself
	// and every site 502s (2026-07-05). hzdemolink's jail lacked it.
	if jail := readString(s, "/etc/fail2ban/jail.d/wordpress.conf"); jail != "" && !regexp.MustCompile(`(?m)^\s*ignoreip\s*=`).MatchString(jail) {
		rs = append(rs, New(Warn, "WordPress jail can ban this box's own addresses").For("wordpress jail").
			Because("Proxied requests are logged with the server's own IP; a login flood bans it and every site on the box answers 502 (2026-07-05).").
			Fixed("hs op f2b-restart (adds ignoreip for this box's addresses)"))
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
	eff := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			eff[f[0]] = f[1]
		}
	}
	pa, ok := eff["passwordauthentication"]
	if !ok {
		return []Result{New(Unknown, "passwordauthentication not in sshd -T output")}
	}
	if pa != "no" {
		return []Result{New(Warn, "accepts password authentication").
			Ev("sshd -T: passwordauthentication " + pa).
			Because("Every exposed box gets continuous SSH brute force; keys remove the attack entirely.").
			Fixed("hs op ssh-keys-only   # refuses unless a key login is proven")}
	}
	// The rest of the key-only drop-in (hardening plan §3).
	var loose []string
	if v := eff["permitrootlogin"]; v == "yes" {
		loose = append(loose, "permitrootlogin yes")
	}
	if v := eff["kbdinteractiveauthentication"]; v == "yes" {
		loose = append(loose, "kbdinteractiveauthentication yes")
	}
	if v := eff["allowagentforwarding"]; v == "yes" {
		loose = append(loose, "allowagentforwarding yes")
	}
	if len(loose) > 0 {
		return []Result{New(Warn, "key-only, but "+strings.Join(loose, ", ")).Ev(loose...).
			Because("Root should log in with a key only, and a forwarded agent on a compromised box lends the attacker every key it holds.").
			Fixed("hs op ssh-keys-only")}
	}
	return []Result{New(OK, "key-only (effective sshd config)").Ev("sshd -T: passwordauthentication no, permitrootlogin " + eff["permitrootlogin"])}
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
			Because("Security patches wait for someone to remember.").Fixed("hs op unattended")}
	case "unknown":
		return []Result{New(Warn, "installed but has no config file").
			Because("The package is installed but applies nothing.").Fixed("hs op unattended")}
	case "all":
		return []Result{New(Fail, "applies all Debian updates, not only security").
			Because("Unattended non-security upgrades can restart or break Hestia's stack without anyone watching.").
			Fixed("hs op unattended scope=security")}
	}
	// Proof it runs: its log shows a completed run in the last 2 days.
	last := lastUnattendedRun(env)
	if last.IsZero() {
		return []Result{New(Warn, "security-only, but it has never run").
			Because("No completed run in its log: the apt timers are off or the package was never started, so patches wait for a person (soutuveneet: 49 pending).").
			Fixed("enable the apt timers")}
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
			Fixed(fmt.Sprintf(`v-change-sys-config-value %s ""   # or hs op remove-service`, d.Key)))
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
			Because("Nothing scans customer sites for injected shells.").Fixed("hs op maldet-install")}
	}
	var rs []Result
	if !active(ctx, s, "maldet") {
		cause, _ := s.Run(ctx, "journalctl", "-u", "maldet", "-n", "20", "--no-pager", "-o", "cat")
		r := New(Warn, "maldet.service is not running")
		for _, l := range strings.Split(cause, "\n") {
			if strings.Contains(l, "could not find") || strings.Contains(l, "dependency") {
				r = r.Ev(strings.TrimSpace(l))
				break
			}
		}
		rs = append(rs, r.
			Because("Monitor mode is off, so new files are not scanned as they land.").
			Fixed("systemctl status maldet   # missing deps are usually 'ed' or inotify-tools"))
	}
	stamp, ts := MaldetLastScan(env)
	if stamp == "" {
		return append(rs, New(Warn, "has never completed a scan").
			Because("Installed but unproven — no baseline and no idea of its false-positive rate here.").
			Fixed("hs op maldet-scan"))
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
	out, _ := s.Run(ctx, "find", append(append(roots, "-maxdepth", "2"), wpfiles.DumpPatterns...)...)
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
	known := map[string]map[string]int{}
	var doms []string
	for _, f := range nonEmpty(out) {
		dom := strings.Split(f, "/")[4]
		if _, ok := by[dom]; !ok && known[dom] == nil {
			doms = append(doms, dom)
		}
		rel := f[strings.Index(f, "/wp-content/uploads/")+len("/wp-content/uploads/"):]
		if p := wpfiles.KnownUploadsPHP(rel); p != "" {
			if known[dom] == nil {
				known[dom] = map[string]int{}
			}
			known[dom][p]++
			continue
		}
		by[dom] = append(by[dom], "uploads/"+rel)
	}
	var rs []Result
	for _, d := range doms {
		var kn []string
		for p, n := range known[d] {
			kn = append(kn, fmt.Sprintf("%d × %s (known, left alone)", n, p))
		}
		if len(by[d]) == 0 {
			rs = append(rs, New(Configured, "only known plugin files in uploads").For(d).Ev(kn...))
			continue
		}
		rs = append(rs, New(Warn, plural(len(by[d]), "unexpected PHP file in uploads", "unexpected PHP files in uploads")).
			For(d).Ev(append(by[d], kn...)...).
			Because("Uploads should hold media. A PHP file there that no known plugin explains is either a plugin quirk or a dropped shell — read each one.").
			Fixed("quarantine them (known plugin caches are skipped); block PHP execution in uploads in the web template"))
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

// The panel's browser terminal (/_shell/, hestia-web-terminal) was the entry
// point of the May 2026 compromise: 1.0.2 accepted a session that had only
// loaded /login/ as root. It stays off; a Hestia upgrade must not bring it
// back unnoticed, so on, running or listening are all Fail.
func checkWebTerminal(ctx context.Context, env *Env) []Result {
	s := env.Sys
	var on []string // evidence that it runs or will run
	if hestia.Conf(s)["WEB_TERMINAL"] == "true" {
		on = append(on, "hestia.conf: WEB_TERMINAL='true'")
	}
	if active(ctx, s, "hestia-web-terminal") {
		on = append(on, "hestia-web-terminal.service is active")
	}
	if out, _ := s.Run(ctx, "ss", "-Hltn", "sport = :8085"); strings.TrimSpace(out) != "" {
		on = append(on, "something listens on :8085: "+strings.Join(strings.Fields(out), " "))
	}
	ver, _ := s.Run(ctx, "dpkg-query", "-W", "-f=${Status} ${Version}", "hestia-web-terminal")
	installed, vulnerable := strings.HasPrefix(ver, "install ok installed"), false
	var pkg []string
	if installed {
		f := strings.Fields(ver)
		v := f[len(f)-1]
		pkg = append(pkg, "package hestia-web-terminal "+v+" installed")
		if versionLess(v, "1.0.3") {
			vulnerable = true
			pkg = append(pkg, "versions before 1.0.3 accept an unauthenticated session as root")
		}
	}
	switch {
	case len(on) > 0:
		return []Result{New(Fail, "the web terminal is on").Ev(append(on, pkg...)...).
			Because("The browser terminal on the panel port gives a root shell; its 1.0.2 build let unauthenticated visitors in, which is how three boxes were compromised in May 2026. Keep it off; enable it only for the minutes it is used.").
			Fixed("v-delete-sys-web-terminal")}
	case vulnerable:
		return []Result{New(Warn, "off, but the vulnerable package is still installed").Ev(pkg...).
			Because("Turning it on again (v-add-sys-web-terminal, or a Hestia upgrade re-enabling it) would start the exact build the May 2026 attackers used.").
			Fixed("apt-get purge hestia-web-terminal")}
	case installed:
		return []Result{New(Configured, "off (package still installed)").Ev(pkg...)}
	}
	return []Result{New(OK, "off and not installed")}
}

// The panel API runs any v-* command as the authenticated user. Hestia gates
// it by API/API_SYSTEM and API_ALLOWED_IP: an empty list denies every address
// (web/api/index.php compares the client IP with the list plus ""), only
// 'allow-all' skips the check.
func checkAPI(ctx context.Context, env *Env) []Result {
	c := hestia.Conf(env.Sys)
	on := c["API"] == "yes" || (c["API_SYSTEM"] != "" && c["API_SYSTEM"] != "0")
	allowed := c["API_ALLOWED_IP"]
	ev := fmt.Sprintf("API='%s' API_SYSTEM='%s' API_ALLOWED_IP='%s'", c["API"], c["API_SYSTEM"], allowed)
	switch {
	case !on:
		return []Result{New(OK, "API off").Ev(ev)}
	case allowed == "allow-all":
		return []Result{New(Fail, "API on and open to every address").Ev(ev).
			Because("Anyone who guesses or steals a panel password or access key can run Hestia commands from anywhere; nothing here uses the API.").
			Fixed("v-change-sys-api disable   # or v-change-sys-config-value API_ALLOWED_IP 'IP1,IP2'")}
	case allowed == "":
		return []Result{New(OK, "API on but no address allowed").Ev(ev)}
	}
	return []Result{New(Configured, "API on, limited to "+allowed).Ev(ev)}
}

// Hestia keeps a user's TOTP secret in TWOFA in user.conf. Every panel login
// without it is one password away from the panel — and from the root-level
// advisories that need only a login.
func check2FA(ctx context.Context, env *Env) []Result {
	root := hestia.Conf(env.Sys)["ROOT_USER"]
	if root == "" {
		root = "admin"
	}
	var admins, users []string
	total := 0
	for _, u := range hestia.Users(env.Sys) {
		kv := hestia.ParseKV(strings.ReplaceAll(readString(env.Sys, hestia.UsersDir+"/"+u+"/user.conf"), "\n", " "))
		if kv["SUSPENDED"] == "yes" {
			continue
		}
		total++
		if kv["TWOFA"] != "" {
			continue
		}
		if u == root || kv["ROLE"] == "admin" {
			admins = append(admins, u)
		} else {
			users = append(users, u)
		}
	}
	why := "A stolen or guessed password is a panel login, and most 2026 Hestia advisories turn any login into root."
	var rs []Result
	for _, u := range admins {
		rs = append(rs, New(Fail, "administrator without two-factor login").For(u).Because(why).
			Fixed("log in as "+u+" → Edit user → Two-factor authentication"))
	}
	if len(users) > 0 {
		rs = append(rs, New(Warn, fmt.Sprintf("%d of %d panel users without two-factor login", len(users), total)).For("users").
			Ev(strings.Join(users, " ")).Because(why+" Accounts nobody logs in to are safest with login disabled.").
			Fixed("per user: Edit user → Two-factor authentication, or v-change-user-password USER to an unknown random value"))
	}
	if len(rs) == 0 {
		return []Result{New(OK, fmt.Sprintf("all %d active panel users have two-factor login", total))}
	}
	return rs
}
