package box

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	. "github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/sys"
)

func systemChecks() []Check {
	return []Check{
		{ID: "systemd.failed", Section: "system", Title: "systemd units", Run: checkFailedUnits},
		{ID: "services.core", Section: "system", Title: "Core services", Run: checkCoreServices},
		{ID: "disk", Section: "system", Title: "Disk", Run: checkDisk},
		{ID: "memory.fpm-ceiling", Section: "system", Title: "PHP-FPM memory ceiling", Run: checkFPMCeiling},
		{ID: "memory.swap", Section: "system", Title: "Swap", Run: checkSwap},
		{ID: "services.idle", Section: "system", Title: "Idle services", Run: checkIdleServices},
	}
}

func checkFailedUnits(ctx context.Context, env *Env) []Result {
	out, err := env.Sys.Run(ctx, "systemctl", "--failed", "--no-legend", "--plain")
	if err != nil {
		return []Result{New(Unknown, "systemctl --failed did not answer")}
	}
	var rs []Result
	for _, l := range nonEmpty(out) {
		unit := strings.Fields(l)[0]
		if unit == "systemd-quotacheck.service" {
			rs = append(rs, quotacheckResult(ctx, env))
			continue
		}
		rs = append(rs, New(Fail, "failed").For(unit).Ev(unitCause(ctx, env, unit)...).
			Because("Something the box is configured to run is not running, and nothing else surfaces it."))
	}
	if len(rs) == 0 {
		return []Result{New(OK, "no failed units")}
	}
	return rs
}

// unitCause pulls the lines that say why from the unit's last journal lines —
// the ones naming a missing dependency, an error or an exit code.
func unitCause(ctx context.Context, env *Env, unit string) []string {
	out, _ := env.Sys.Run(ctx, "journalctl", "-u", unit, "-n", "30", "--no-pager", "-o", "cat")
	var why []string
	seen := map[string]bool{}
	for _, l := range nonEmpty(out) {
		if seen[l] {
			continue
		}
		seen[l] = true
		ll := strings.ToLower(l)
		if strings.Contains(ll, "could not") || strings.Contains(ll, "error") || strings.Contains(ll, "not found") ||
			strings.Contains(ll, "no such file") || strings.Contains(ll, "permission denied") || strings.Contains(ll, "failed with result") {
			why = append(why, strings.TrimSpace(l))
		}
	}
	if len(why) > 4 {
		why = why[len(why)-4:]
	}
	return why
}

// systemd-quotacheck fails at every boot on Hestia boxes with journaled ext4
// quotas, by upstream design: v-add-sys-quota installs /etc/cron.daily/
// quotacheck, which touches /forcequotacheck daily, forcing a check at boot
// that cannot remount / read-only. Quotas themselves stay on. Report it as
// that — and warn only if quotas are actually off.
func quotacheckResult(ctx context.Context, env *Env) Result {
	out, _ := env.Sys.Run(ctx, "quotaon", "-pa")
	userOn := strings.Contains(out, "user quota on / ") && strings.Contains(out, ") is on")
	daily := exists(env.Sys, "/etc/cron.daily/quotacheck")
	if !userOn {
		return New(Warn, "quota check failed and user quotas are off").For("systemd-quotacheck.service").Ev(nonEmpty(out)...).
			Because("Hestia's per-user disk limits are not enforced.").Fixed("v-add-sys-quota")
	}
	r := New(Configured, "fails at every boot by Hestia's design; quotas are on").For("systemd-quotacheck.service").
		Ev(nonEmpty(out)...)
	if daily {
		r = r.Ev("/etc/cron.daily/quotacheck (from v-add-sys-quota) recreates /forcequotacheck daily; the boot-time check cannot remount / read-only")
	}
	return r
}

// CoreServices is the list v-server-health reports: web server, hestia,
// the database flavour present, and the first php-fpm unit.
func CoreServices(ctx context.Context, env *Env) []string {
	out, _ := env.Sys.Run(ctx, "systemctl", "list-unit-files", "--no-legend", "--type=service")
	var units []string
	for _, l := range nonEmpty(out) {
		units = append(units, strings.TrimSuffix(strings.Fields(l)[0], ".service"))
	}
	has := func(prefix string) bool {
		for _, u := range units {
			if strings.HasPrefix(u, prefix) {
				return true
			}
		}
		return false
	}
	svcs := []string{"nginx", "hestia"}
	if has("mariadb") {
		svcs = append(svcs, "mariadb")
	}
	if has("mysql") {
		svcs = append(svcs, "mysql")
	}
	for _, u := range units {
		if strings.HasPrefix(u, "php") && strings.HasSuffix(u, "-fpm") {
			svcs = append(svcs, u)
			break
		}
	}
	return svcs
}

func checkCoreServices(ctx context.Context, env *Env) []Result {
	s := env.Sys
	var rs []Result
	if !active(ctx, s, "nginx") && !active(ctx, s, "apache2") {
		rs = append(rs, New(Fail, "no web server is running").Because("Every hosted site is down.").
			Fixed("systemctl status nginx apache2"))
	}
	for _, svc := range []string{"hestia", "mariadb"} {
		if unitExists(ctx, s, svc) && !active(ctx, s, svc) {
			rs = append(rs, New(Fail, svc+" is not running").For(svc).Fixed("systemctl status "+svc))
		}
	}
	// Every PHP version that serves a pool must be up, not only the newest.
	var up []string
	for _, v := range PHPVersions(s) {
		pools, _ := s.Glob("/etc/php/" + v + "/fpm/pool.d/*.conf")
		unit := "php" + v + "-fpm"
		if len(pools) == 0 || !unitExists(ctx, s, unit) {
			continue
		}
		if !active(ctx, s, unit) {
			rs = append(rs, New(Fail, unit+" is down with "+plural(len(pools), "pool", "pools")+" configured").For(unit).
				Because("Sites on this PHP version return 502.").Fixed("systemctl status "+unit))
		} else {
			up = append(up, unit)
		}
	}
	if len(rs) > 0 {
		return rs
	}
	return []Result{New(OK, "web server, hestia, database and "+strings.Join(up, ", ")+" active")}
}

func checkDisk(ctx context.Context, env *Env) []Result {
	out, err := env.Sys.Run(ctx, "df", "--output=target,pcent,ipcent,used,size",
		"-x", "tmpfs", "-x", "devtmpfs", "-x", "overlay", "-x", "squashfs", "-x", "efivarfs")
	if err != nil && out == "" {
		return []Result{New(Unknown, "df failed")}
	}
	var rs []Result
	var fine []string
	lines := nonEmpty(out)
	for _, l := range lines[min(1, len(lines)):] {
		f := strings.Fields(l)
		if len(f) < 5 {
			continue
		}
		mount := f[0]
		pct, _ := strconv.Atoi(strings.TrimSuffix(f[1], "%"))
		ipct, _ := strconv.Atoi(strings.TrimSuffix(f[2], "%"))
		switch {
		case pct >= 90:
			rs = append(rs, New(Fail, fmt.Sprintf("%s is %d%% full", mount, pct)).For(mount).
				Because("At 100% MariaDB stops writing and sites start erroring.").Fixed("hs op ncdu; cleanups: hs op disk-apt / disk-journal / log-trim / logs-rotated"))
		case pct >= 80:
			rs = append(rs, New(Warn, fmt.Sprintf("%s is %d%% full", mount, pct)).For(mount).
				Because("Headroom is getting thin.").Fixed("hs op ncdu; cleanups: hs op disk-apt / disk-journal / log-trim / logs-rotated"))
		}
		if ipct >= 85 {
			rs = append(rs, New(Warn, fmt.Sprintf("%s has used %d%% of its inodes", mount, ipct)).For(mount+" inodes").
				Because("Writes fail with 'No space left on device' while df -h still shows free space.").
				Fixed("df -i "+mount+"  →  hunt small-file directories (sessions, cache)"))
		}
		if pct < 80 && ipct < 85 {
			fine = append(fine, fmt.Sprintf("%s %d%%", mount, pct))
		}
	}
	if len(rs) > 0 {
		return rs
	}
	return []Result{New(OK, strings.Join(fine, " · "))}
}

// FPMPoolChildren sums pm.max_children over every pool file — what PHP-FPM
// is allowed to fork, box-wide, as configured today.
func FPMPoolChildren(s sys.Sys) (children, pools int) {
	files, _ := s.Glob("/etc/php/*/fpm/pool.d/*.conf")
	for _, p := range files {
		pools++
		children += poolInt(readString(s, p), "pm.max_children")
	}
	return
}

// FPMWorkerMemory measures what one more PHP worker costs. Not RSS — every
// worker maps the same opcache SHM, so children × RSS counts that block once
// per worker. With N workers, sum(PSS) = shared + N×private and RSS ≈ shared
// + private solve for both (kB); only private is paid again per child.
func FPMWorkerMemory(s sys.Sys) (private, shared int64, workers int) {
	var n, rssSum, pssSum int64
	procs, _ := s.Glob("/proc/[0-9]*")
	for _, p := range procs {
		comm := strings.TrimSpace(readString(s, p+"/comm"))
		if !strings.HasPrefix(comm, "php") {
			continue
		}
		var rss, pss int64
		for _, l := range strings.Split(readString(s, p+"/smaps_rollup"), "\n") {
			f := strings.Fields(l)
			if len(f) < 2 {
				continue
			}
			v, _ := strconv.ParseInt(f[1], 10, 64)
			switch f[0] {
			case "Rss:":
				rss = v
			case "Pss:":
				pss = v
			}
		}
		if rss > 0 {
			n++
			rssSum += rss
			pssSum += pss
		}
	}
	if n == 0 {
		return 0, 0, 0
	}
	avg := rssSum / n
	private, shared = avg, 0
	if n > 1 && pssSum > avg {
		private = (pssSum - avg) / (n - 1)
		shared = max(avg-private, 0)
	}
	if private < 1 {
		private = avg
	}
	return private, shared, int(n)
}

// FPMTemplate is a php-fpm pool template Hestia renders into one pool per
// domain on it (v-rebuild-web-domain, v-change-web-domain-backend-tpl) —
// so the template, not the pool file, is where a limit lasts.
type FPMTemplate struct {
	Name, Path                             string
	Version                                string // from a PHP-X_Y suffix; "" = Hestia's default PHP
	Mode                                   string
	MaxChildren, Start, MinSpare, MaxSpare int
	IdleTimeout                            string
	Domains                                []hestia.Domain
}

// Ceiling is the workers every domain on the template may fork together.
func (t FPMTemplate) Ceiling() int { return len(t.Domains) * t.MaxChildren }

var fpmVersionRe = regexp.MustCompile(`PHP-(\d+)_(\d+)$`)

func poolValue(text, key string) string {
	m := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(key) + `\s*=\s*(\S+)`).FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return m[1]
}

func poolInt(text, key string) int {
	n, _ := strconv.Atoi(poolValue(text, key))
	return n
}

// FPMTemplates lists the backend templates in use, biggest ceiling first.
func FPMTemplates(s sys.Sys) []FPMTemplate {
	byName := map[string]*FPMTemplate{}
	var out []*FPMTemplate
	for _, d := range hestia.WebDomains(s) {
		name := d.Backend
		if name == "" {
			name = "default"
		}
		t, ok := byName[name]
		if !ok {
			t = &FPMTemplate{Name: name, Path: hestia.FpmTpl + "/" + name + ".tpl"}
			if m := fpmVersionRe.FindStringSubmatch(name); m != nil {
				t.Version = m[1] + "." + m[2]
			}
			text := readString(s, t.Path)
			t.Mode, t.IdleTimeout = poolValue(text, "pm"), poolValue(text, "pm.process_idle_timeout")
			t.MaxChildren, t.Start = poolInt(text, "pm.max_children"), poolInt(text, "pm.start_servers")
			t.MinSpare, t.MaxSpare = poolInt(text, "pm.min_spare_servers"), poolInt(text, "pm.max_spare_servers")
			byName[name] = t
			out = append(out, t)
		}
		t.Domains = append(t.Domains, d)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Ceiling() != out[j].Ceiling() {
			return out[i].Ceiling() > out[j].Ceiling()
		}
		return out[i].Name < out[j].Name
	})
	var ts []FPMTemplate
	for _, t := range out {
		ts = append(ts, *t)
	}
	return ts
}

// PSI thresholds, the same as templates/netdata/health.d/hs-pressure.conf:
// "some" = at least one task stalled on memory for that share of the last
// minute, "full" = every task. Both read 0.00 on a healthy web box.
const (
	PSISomeWarn = 20.0
	PSIFullFail = 5.0
)

// The check that would have predicted the web1 OOM (2026-07-06): what PHP-FPM
// may allocate under load, not what it uses at rest. And, from PSI, whether
// the box is stalling on memory right now — the number a full swap file does
// not give (kuumalahde 2026-09-30: 94 % used, nothing moving, 3 GB available).
func checkFPMCeiling(ctx context.Context, env *Env) []Result {
	s := env.Sys
	ramMB := meminfo(s)["MemTotal"] / 1024
	children, _ := FPMPoolChildren(s)
	private, shared, n := FPMWorkerMemory(s)
	if children == 0 || n == 0 {
		return []Result{New(Unknown, "no PHP-FPM pools or workers to measure")}
	}
	worstMB := (shared + int64(children)*private) / 1024
	ev := []string{fmt.Sprintf("%d max_children × %d MB private + %d MB shared (measured over %d workers)",
		children, private/1024, shared/1024, n)}
	for _, t := range FPMTemplates(s) {
		var names []string
		for _, d := range t.Domains {
			names = append(names, d.Name)
		}
		ev = append(ev, fmt.Sprintf("%s: %s × max_children %d = %d workers (%s)", t.Name, plural(len(t.Domains), "domain", "domains"), t.MaxChildren, t.Ceiling(), joinMax(names, 4)))
	}
	var rs []Result
	if p := psi(s, "memory"); p != nil {
		now := fmt.Sprintf("memory pressure now (PSI, last 60 s): some %.1f %%, full %.1f %%", p["some60"], p["full60"])
		switch {
		case p["full60"] > PSIFullFail:
			rs = append(rs, New(Fail, fmt.Sprintf("every task stalled on memory %.0f %% of the last minute", p["full60"])).For("now").Ev(now).
				Because("Full memory pressure is the box thrashing, not degrading: requests hang while the kernel reclaims, and the OOM killer is next.").
				Fixed("hs op fpm-pool-size (lower the biggest pool's max_children); v-server-memory for the whole picture"))
		case p["some60"] > PSISomeWarn:
			rs = append(rs, New(Warn, fmt.Sprintf("tasks stalled on memory %.0f %% of the last minute", p["some60"])).For("now").Ev(now).
				Because("The working set no longer fits: the kernel is reclaiming and swapping under load, which is where the ceiling below becomes real.").
				Fixed("hs op fpm-pool-size"))
		default:
			ev = append(ev, now+" — none")
		}
	}
	if worstMB > ramMB {
		return append(rs, New(Warn, fmt.Sprintf("PHP-FPM can request %d MB on a %d MB box", worstMB, ramMB)).Ev(ev...).
			Because("A traffic spike OOM-kills MariaDB before PHP notices.").
			Fixed("hs op fpm-pool-size — lower max_children in the pool template with the biggest ceiling (Hestia regenerates the pools from it)"))
	}
	return append(rs, New(OK, fmt.Sprintf("worst case %d MB of %d MB RAM", worstMB, ramMB)).Ev(ev...))
}

// SwapArea is one line of /proc/swaps (sizes in kB).
type SwapArea struct {
	Path, Type     string
	SizeKB, UsedKB int64
}

func SwapAreas(s sys.Sys) []SwapArea {
	var out []SwapArea
	for _, l := range nonEmpty(readString(s, "/proc/swaps")) {
		f := strings.Fields(l)
		if len(f) < 4 || f[0] == "Filename" {
			continue
		}
		size, _ := strconv.ParseInt(f[2], 10, 64)
		used, _ := strconv.ParseInt(f[3], 10, 64)
		out = append(out, SwapArea{Path: f[0], Type: f[1], SizeKB: size, UsedKB: used})
	}
	return out
}

// Sysctl reads a running kernel setting ("" when unreadable).
func Sysctl(s sys.Sys, key string) string {
	return strings.TrimSpace(readString(s, "/proc/sys/"+strings.ReplaceAll(key, ".", "/")))
}

// A full swap is not pressure: pages parked there during an old peak stay
// until something needs them. Whether anything is stalling on memory is
// PSI's question (the ceiling check asks it); this is the swap's own state.
func checkSwap(ctx context.Context, env *Env) []Result {
	m := meminfo(env.Sys)
	total, free := m["SwapTotal"]/1024, m["SwapFree"]/1024
	used := total - free
	if total == 0 {
		return []Result{New(Warn, "no swap configured").
			Because("Without swap the kernel OOM-kills immediately instead of degrading.").
			Fixed("hs op swap")}
	}
	swappiness := orDash(Sysctl(env.Sys, "vm.swappiness"))
	ev := []string{fmt.Sprintf("%d of %d MB in use · vm.swappiness %s · MemAvailable %d MB", used, total, swappiness, m["MemAvailable"]/1024)}
	for _, a := range SwapAreas(env.Sys) {
		ev = append(ev, fmt.Sprintf("%s (%s) %d MB, %d MB used", a.Path, a.Type, a.SizeKB/1024, a.UsedKB/1024))
	}
	p := psi(env.Sys, "memory")
	if p != nil {
		ev = append(ev, fmt.Sprintf("memory pressure now (PSI, last 60 s): some %.1f %%, full %.1f %%", p["some60"], p["full60"]))
	}
	r := New(OK, fmt.Sprintf("%d of %d MB in use", used, total))
	switch {
	case p != nil && p["some60"] > PSISomeWarn && used*100/total >= 90:
		r = New(Warn, fmt.Sprintf("swap is %d %% full and memory is under pressure", used*100/total)).
			Because(fmt.Sprintf("Nothing is left to park into and tasks are already stalling on memory (some %.0f %% of the last minute): the next spike goes to the OOM killer.", p["some60"])).
			Fixed("hs op fpm-pool-size; a bigger file with hs op swap")
	case used == 0:
		r.Summary = fmt.Sprintf("%d MB, unused — no memory pressure since boot", total)
	case p != nil && p["some60"] <= PSISomeWarn:
		r.Summary += " — parked, no memory pressure"
	}
	return []Result{r.With("swappiness", swappiness).Ev(ev...)}
}

// Big resident services nothing on this box consumes, and PHP past EOL.
func checkIdleServices(ctx context.Context, env *Env) []Result {
	s := env.Sys
	var rs []Result
	if !pkgInstalled(ctx, s, "exim4", "dovecot-core") && active(ctx, s, "clamav-daemon") {
		rs = append(rs, New(Warn, "clamd is running with no mail stack to serve").For("clamav").
			Because("ClamAV under Hestia only scans inbound mail; with exim4 and dovecot gone it scans nothing and holds ~1 GB.").
			Fixed("hs op remove-service service=clamav-daemon"))
	}
	// Hardening plan §14: turn off what a box does not serve.
	anyKV := func(glob, key string) bool {
		files, _ := s.Glob(glob)
		for _, f := range files {
			for _, l := range strings.Split(readString(s, f), "\n") {
				if hestia.ParseKV(l)[key] != "" {
					return true
				}
			}
		}
		return false
	}
	if active(ctx, s, "proftpd") && !anyKV(hestia.UsersDir+"/*/web.conf", "FTP_USER") {
		rs = append(rs, New(Warn, "ProFTPD is running with no FTP users").For("proftpd").
			Because("An open plain-FTP port with nothing to serve is attack surface; SFTP over SSH covers file access.").
			Fixed("hs op remove-service service=proftpd"))
	}
	if active(ctx, s, "named") && !anyKV(hestia.UsersDir+"/*/dns.conf", "DOMAIN") {
		rs = append(rs, New(Warn, "BIND is running with no DNS zones").For("named").
			Because("A DNS server that hosts nothing still answers queries — and can be abused for amplification.").
			Fixed("hs op remove-service service=named"))
	}
	var eol []string
	for _, v := range PHPVersions(s) {
		if (strings.HasPrefix(v, "5.") || strings.HasPrefix(v, "7.") || v == "8.0" || v == "8.1") && active(ctx, s, "php"+v+"-fpm") {
			pools, _ := s.Glob("/etc/php/" + v + "/fpm/pool.d/*.conf")
			var names []string
			for _, p := range pools {
				if b := filepath.Base(p); b != "www.conf" && b != "dummy.conf" {
					names = append(names, strings.TrimSuffix(b, ".conf"))
				}
			}
			eol = append(eol, fmt.Sprintf("%s (%s)", v, joinMax(names, 4)))
		}
	}
	if len(eol) > 0 {
		rs = append(rs, New(Warn, "end-of-life PHP still running: "+strings.Join(eol, "; ")).For("php-eol").
			Because("No security patches upstream; sites on it are the box's soft spot.").
			Fixed("move the domains to a supported version, then stop the old pool"))
	}
	if len(rs) > 0 {
		return rs
	}
	return []Result{New(OK, "no idle resident services, no EOL PHP running")}
}
