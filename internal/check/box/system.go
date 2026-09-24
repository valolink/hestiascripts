package box

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	. "github.com/valolink/hestiascripts/internal/check"
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
	var failed []string
	for _, l := range nonEmpty(out) {
		failed = append(failed, strings.Fields(l)[0])
	}
	if len(failed) > 0 {
		return []Result{New(Fail, plural(len(failed), "failed unit", "failed units")+": "+joinMax(failed, 6)).
			Because("Something the box is configured to run is not running, and nothing else surfaces it.").
			Fixed("systemctl --failed  →  systemctl status <unit>")}
	}
	return []Result{New(OK, "no failed units")}
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
				Because("At 100% MariaDB stops writing and sites start erroring.").Fixed("run.sh → 14 (Disk)"))
		case pct >= 80:
			rs = append(rs, New(Warn, fmt.Sprintf("%s is %d%% full", mount, pct)).For(mount).
				Because("Headroom is getting thin.").Fixed("run.sh → 14 (Disk)"))
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

// The check that would have predicted the web1 OOM (2026-07-06): what PHP-FPM
// may allocate under load, not what it uses at rest. Not children × RSS —
// every worker maps the same opcache SHM. sum(PSS) = shared + N×private and
// RSS ≈ shared + private solve for both.
func checkFPMCeiling(ctx context.Context, env *Env) []Result {
	s := env.Sys
	ramMB := meminfo(s)["MemTotal"] / 1024

	children := 0
	pools, _ := s.Glob("/etc/php/*/fpm/pool.d/*.conf")
	for _, p := range pools {
		for _, l := range strings.Split(readString(s, p), "\n") {
			t := strings.TrimSpace(l)
			if strings.HasPrefix(t, "pm.max_children") {
				if i := strings.LastIndex(t, "="); i >= 0 {
					n, _ := strconv.Atoi(strings.TrimSpace(t[i+1:]))
					children += n
				}
			}
		}
	}
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
	if children == 0 || n == 0 {
		return []Result{New(Unknown, "no PHP-FPM pools or workers to measure")}
	}
	avg := rssSum / n
	private, shared := avg, int64(0)
	if n > 1 && pssSum > avg {
		private = (pssSum - avg) / (n - 1)
		shared = max(avg-private, 0)
	}
	if private < 1 {
		private = avg
	}
	worstMB := (shared + int64(children)*private) / 1024
	ev := fmt.Sprintf("%d max_children × %d MB private + %d MB shared (measured over %d workers)",
		children, private/1024, shared/1024, n)
	if worstMB > ramMB {
		return []Result{New(Warn, fmt.Sprintf("PHP-FPM can request %d MB on a %d MB box", worstMB, ramMB)).Ev(ev).
			Because("A traffic spike OOM-kills MariaDB before PHP notices.").
			Fixed("run.sh → 9 (PHP-FPM): pick a profile that fits, or lower pm.max_children")}
	}
	return []Result{New(OK, fmt.Sprintf("worst case %d MB of %d MB RAM", worstMB, ramMB)).Ev(ev)}
}

func checkSwap(ctx context.Context, env *Env) []Result {
	m := meminfo(env.Sys)
	total, free := m["SwapTotal"]/1024, m["SwapFree"]/1024
	used := total - free
	switch {
	case total == 0:
		return []Result{New(Warn, "no swap configured").
			Because("Without swap the kernel OOM-kills immediately instead of degrading.").
			Fixed("run.sh → 7 (Security) → 2")}
	case used > total/2:
		return []Result{New(Warn, fmt.Sprintf("%d of %d MB swap in use", used, total)).
			Because("The box is under real memory pressure, not merely parked.").Fixed("v-server-memory")}
	}
	return []Result{New(OK, fmt.Sprintf("%d MB, %d MB used", total, used))}
}

// Big resident services nothing on this box consumes, and PHP past EOL.
func checkIdleServices(ctx context.Context, env *Env) []Result {
	s := env.Sys
	var rs []Result
	if !pkgInstalled(ctx, s, "exim4", "dovecot-core") && active(ctx, s, "clamav-daemon") {
		rs = append(rs, New(Warn, "clamd is running with no mail stack to serve").For("clamav").
			Because("ClamAV under Hestia only scans inbound mail; with exim4 and dovecot gone it scans nothing and holds ~1 GB.").
			Fixed("run.sh → 13 → 6 → 2 (Remove ClamAV)"))
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
