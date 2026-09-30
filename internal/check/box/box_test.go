package box

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	. "github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/sys"
)

var now = time.Date(2026, 9, 24, 12, 0, 0, 0, time.Local)

func env(f *sys.Fake) *Env {
	if f.Clock.IsZero() {
		f.Clock = now
	}
	if f.Cmds == nil {
		f.Cmds = map[string]sys.FakeCmd{}
	}
	if f.Files == nil {
		f.Files = map[string]string{}
	}
	return &Env{Sys: f}
}

func states(rs []Result) string {
	var s []string
	for _, r := range rs {
		s = append(s, r.State.String())
	}
	return strings.Join(s, ",")
}

func f2bLog(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

func stamp(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") + ",123" }

// kuumalahde 2026-08-28 in miniature: running, jail present, bans failing.
func TestFail2banOnlyCountsSinceStart(t *testing.T) {
	started := now.Add(-2 * time.Hour)
	base := func(log string) *sys.Fake {
		return &sys.Fake{
			Commands: map[string]bool{"fail2ban-client": true},
			Files:    map[string]string{"/var/log/fail2ban.log": log},
			Cmds: map[string]sys.FakeCmd{
				"systemctl is-active --quiet fail2ban":                                     {},
				"fail2ban-client status wordpress":                                         {},
				"systemctl show fail2ban -p ActiveEnterTimestamp --value --timestamp=unix": {Out: fmt.Sprintf("@%d\n", started.Unix())},
			},
		}
	}
	before := stamp(started.Add(-time.Hour))
	after := stamp(started.Add(time.Hour))

	// A failure before the restart is history, not a fault.
	rs := checkFail2ban(context.Background(), env(base(f2bLog(
		before+" fail2ban.actions [1]: ERROR Failed to execute ban jail 'sshd' action",
		after+" fail2ban.actions [1]: NOTICE  [sshd] Ban 1.2.3.4",
	))))
	if states(rs) != "ok" {
		t.Errorf("old failure + fresh ban: %s (%v)", states(rs), rs)
	}

	rs = checkFail2ban(context.Background(), env(base(f2bLog(
		after+" fail2ban.actions [1]: NOTICE  [sshd] Ban 1.2.3.4",
		after+" fail2ban.actions [1]: ERROR Failed to execute ban jail 'sshd' action",
	))))
	if states(rs) != "fail" {
		t.Errorf("failure since start: %s", states(rs))
	}

	// Running with no ban since start is unproven, not green.
	rs = checkFail2ban(context.Background(), env(base(f2bLog(before+" fail2ban.actions [1]: NOTICE  [sshd] Ban 9.9.9.9"))))
	if states(rs) != "configured" {
		t.Errorf("no ban since start: %s", states(rs))
	}
}

func TestResticRepoJoin(t *testing.T) {
	if got := ResticRepoFor("rclone:storagebox:hestia-web1", "alavus"); got != "rclone:storagebox:hestia-web1/alavus" {
		t.Errorf("no trailing slash: %s", got)
	}
	if got := ResticRepoFor("rclone:storagebox:hestia-web1/", "alavus"); got != "rclone:storagebox:hestia-web1/alavus" {
		t.Errorf("trailing slash: %s", got)
	}
	for base, empty := range map[string]bool{"rclone:storagebox:": true, "rclone:storagebox:/": true, "rclone:storagebox:valolink": false} {
		if repoPathEmpty(base) != empty {
			t.Errorf("repoPathEmpty(%q) != %v", base, empty)
		}
	}
}

func TestNightlyAndHourlyFromOneResticCall(t *testing.T) {
	snaps, _ := json.Marshal([]map[string]any{
		{"time": now.Add(-5 * time.Hour).Format(time.RFC3339), "paths": []string{"/home/alavus"}},
		{"time": now.Add(-30 * time.Minute).Format(time.RFC3339), "tags": []string{"db-hourly"}, "paths": []string{"/var/lib/hestia-hourly-db/alavus"}},
	})
	f := &sys.Fake{
		Commands: map[string]bool{"restic": true},
		Files: map[string]string{
			hestia.ResticSys:                        "REPO='rclone:storagebox:hestia-web1'\n",
			hestia.UsersDir + "/alavus/restic.conf": "secret",
			hestia.UsersDir + "/alavus/db.conf":     "DB='alavus_wp'\n",
			hestia.UsersDir + "/old/user.conf":      "",
			"/backup/old.2026-09-20_05-10-01.tar":   "x",
			resticCron:                              "0 3 * * * root v-backup-users-restic\n30 * * * * root v-server-backup-db-hourly\n45 2 * * * root v-server-backup-guard\n",
		},
		ModTimes: map[string]time.Time{"/backup/old.2026-09-20_05-10-01.tar": now.Add(-100 * time.Hour)},
		Cmds: map[string]sys.FakeCmd{
			"restic --repo rclone:storagebox:hestia-web1/alavus --password-file /usr/local/hestia/data/users/alavus/restic.conf -o " + rcloneArgs + " --no-lock --json snapshots --latest 1": {Out: string(snaps)},
		},
	}
	rs := checkNightly(context.Background(), env(f))
	got := map[string]State{}
	for _, r := range rs {
		got[r.Subject] = r.State
	}
	if got["alavus"] != OK || got["alavus db-hourly"] != OK {
		t.Errorf("alavus: %v", got)
	}
	if got["old"] != Fail {
		t.Errorf("100h-old tarball should fail: %v", got)
	}
}

func TestRedisSitesLayout(t *testing.T) {
	dropin := "<?php\n/*\n * Plugin Name: Redis Object Cache Drop-In\n * Version: 2.8.0\n */"
	mk := func(confA, confB string) *sys.Fake {
		f := &sys.Fake{Files: map[string]string{
			hestia.UsersDir + "/u/web.conf": "DOMAIN='a.fi' PROXY='wp-secure'\nDOMAIN='b.fi' PROXY='wp-secure'\n",
		}, Cmds: map[string]sys.FakeCmd{"redis-cli config get databases": {Out: "databases\n16\n"}}}
		for _, x := range [][2]string{{"a.fi", confA}, {"b.fi", confB}} {
			root := "/home/u/web/" + x[0] + "/public_html"
			f.Files[root+"/wp-config.php"] = x[1]
			f.Files[root+"/wp-content/object-cache.php"] = dropin
			f.Files[root+"/wp-content/plugins/redis-cache/redis-cache.php"] = " * Version: 2.8.0"
		}
		return f
	}
	byDomain := func(rs []Result) map[string]Result {
		m := map[string]Result{}
		for _, r := range rs {
			m[r.Subject] = r
		}
		return m
	}
	// hzweb1 shape: same database, different prefixes, no MAXTTL
	rs := byDomain(checkRedisSites(context.Background(), env(mk(
		"define('WP_REDIS_PREFIX', 'a_1');", "define( 'WP_REDIS_PREFIX', 'b_2' );"))))
	if rs["a.fi"].State != Warn || !strings.Contains(rs["a.fi"].Summary, "shares Redis database 0 with b.fi") || rs["a.fi"].Data["shared"] != "true" {
		t.Errorf("shared db: %+v", rs["a.fi"])
	}
	// same database AND same prefix: real collision
	rs = byDomain(checkRedisSites(context.Background(), env(mk(
		"define('WP_REDIS_PREFIX', 'shop_');", "define('WP_REDIS_PREFIX', 'shop_');"))))
	if rs["a.fi"].State != Fail || !strings.Contains(rs["a.fi"].Summary, "read each other's cache") {
		t.Errorf("same db + prefix: %+v", rs["a.fi"])
	}
	// the documented layout, two defines on one line, one commented out
	rs = byDomain(checkRedisSites(context.Background(), env(mk(
		"define('WP_REDIS_PREFIX', 'a_1'); define('WP_REDIS_DATABASE', 1); define('WP_REDIS_MAXTTL', 86400);\n// define('WP_REDIS_SELECTIVE_FLUSH', true);",
		"define('WP_REDIS_PREFIX', 'b_2'); define('WP_REDIS_DATABASE', 2); define('WP_REDIS_MAXTTL', 86400);"))))
	if rs["a.fi"].State != OK || rs["b.fi"].State != OK {
		t.Errorf("documented layout should be OK: %+v / %+v", rs["a.fi"], rs["b.fi"])
	}
	// alavus staging shape: own database, selective flush
	rs = byDomain(checkRedisSites(context.Background(), env(mk(
		"define('WP_REDIS_PREFIX', 'a_1'); define('WP_REDIS_DATABASE', 1); define('WP_REDIS_SELECTIVE_FLUSH', true);",
		"define('WP_REDIS_PREFIX', 'b_2'); define('WP_REDIS_DATABASE', 2); define('WP_REDIS_MAXTTL', 86400);"))))
	if !strings.Contains(rs["a.fi"].Summary, "SELECTIVE_FLUSH") || !strings.Contains(rs["a.fi"].Summary, "never expire") {
		t.Errorf("selective + no ttl: %+v", rs["a.fi"])
	}
}

func TestTemplateUsage(t *testing.T) {
	f := &sys.Fake{Files: map[string]string{
		hestia.UsersDir + "/u/web.conf":    "DOMAIN='a.fi' PROXY='wp-rocket'\nDOMAIN='b.fi' PROXY='default'\n",
		hestia.NginxTpl + "/wp-rocket.tpl": "# Valolink security rules\n",
		hestia.NginxTpl + "/default.tpl":   "server {}\n",
	}}
	rs := checkTemplateUsage(context.Background(), env(f))
	if states(rs) != "warn" || rs[0].Subject != "b.fi" {
		t.Errorf("one bare domain, as its own result: %v", rs)
	}
}

func TestDiskParsesEveryFilesystem(t *testing.T) {
	f := &sys.Fake{Cmds: map[string]sys.FakeCmd{
		"df --output=target,pcent,ipcent,used,size -x tmpfs -x devtmpfs -x overlay -x squashfs -x efivarfs": {Out: "Mounted on Use% IUse%  Used  1K-blocks\n/           42%    10% 100 200\n/backup     93%     2% 900 1000\n/home       50%    88% 1 2\n"},
	}}
	rs := checkDisk(context.Background(), env(f))
	got := map[string]State{}
	for _, r := range rs {
		got[r.Subject] = r.State
	}
	if got["/backup"] != Fail || got["/home inodes"] != Warn || len(rs) != 2 {
		t.Errorf("disk: %v", got)
	}
}

// hzdemolink 2026-09-24: Storage Box refusing the box's IP, fresh tarballs
// still landing. The tarball must not make the user read as backed up.
func TestEnrolledUserWithUnreachableResticWarns(t *testing.T) {
	f := &sys.Fake{
		Commands: map[string]bool{"restic": true},
		Files: map[string]string{
			hestia.ResticSys: "REPO='rclone:storagebox:hestia-hzhestia'\n",
			hestia.UsersDir + "/valolink/restic.conf":  "secret",
			"/backup/valolink.2026-09-24_05-10-01.tar": "x",
		},
		ModTimes: map[string]time.Time{"/backup/valolink.2026-09-24_05-10-01.tar": now.Add(-6 * time.Hour)},
		Cmds: map[string]sys.FakeCmd{
			"restic --repo rclone:storagebox:hestia-hzhestia/valolink --password-file /usr/local/hestia/data/users/valolink/restic.conf -o " + rcloneArgs + " --no-lock --json snapshots --latest 1": {
				Code:   1,
				Stderr: `rclone: 2026/09/24 11:21:11 CRITICAL: couldn't connect SSH: dial tcp 62.238.66.142:23: connect: connection refused` + "\n" + `{"message_type":"exit_error","code":1,"message":"Fatal: unable to open repository at rclone:storagebox:hestia-hzhestia/valolink"}`,
			},
		},
	}
	rs := checkNightly(context.Background(), env(f))
	if len(rs) != 1 || rs[0].State != Warn || !strings.Contains(strings.Join(rs[0].Evidence, " "), "restic: Fatal: unable to open repository at rclone:storagebox:hestia-hzhestia/valolink — couldn't connect SSH: dial tcp 62.238.66.142:23: connect: connection refused") {
		t.Fatalf("got %+v", rs)
	}
}

// alavus 2026-09-25: maldet missing `ed`; quotacheck failing by Hestia's design.
func TestFailedUnitsCarryCauseAndKnownPatterns(t *testing.T) {
	f := &sys.Fake{Files: map[string]string{"/etc/cron.daily/quotacheck": "touch /forcequotacheck"}, Cmds: map[string]sys.FakeCmd{
		"systemctl --failed --no-legend --plain":               {Out: "maldet.service loaded failed failed Linux Malware Detect\nsystemd-quotacheck.service loaded failed failed File System Quota Check\n"},
		"journalctl -u maldet.service -n 30 --no-pager -o cat": {Out: "Linux Malware Detect v1.6.6\nmaldet(916): {mon} could not find monitor mode dependency 'ed' in PATH, please apt/yum/dnf install ed and try again.\nmaldet.service: Failed with result 'protocol'.\n"},
		"quotaon -pa": {Out: "group quota on / (/dev/sda1) is on\nuser quota on / (/dev/sda1) is on\n"},
	}}
	rs := checkFailedUnits(context.Background(), env(f))
	got := map[string]Result{}
	for _, r := range rs {
		got[r.Subject] = r
	}
	m := got["maldet.service"]
	if m.State != Fail || !strings.Contains(strings.Join(m.Evidence, " "), "dependency 'ed'") {
		t.Errorf("maldet: %+v", m)
	}
	q := got["systemd-quotacheck.service"]
	if q.State != Configured || !strings.Contains(q.Summary, "Hestia's design") {
		t.Errorf("quotacheck: %+v", q)
	}
}

// kuumalahde 2026-09-30: swap 94 % full, nothing moving, 3 GB available —
// parked, not pressure. The same numbers with PSI showing stalls are.
func TestSwapFullButParkedIsNotPressure(t *testing.T) {
	mk := func(pressure string) *sys.Fake {
		return &sys.Fake{Files: map[string]string{
			"/proc/meminfo":           "MemTotal:        8155000 kB\nMemAvailable:    3300000 kB\nSwapTotal:       1048572 kB\nSwapFree:          60000 kB\n",
			"/proc/swaps":             "Filename\tType\tSize\tUsed\tPriority\n/swapfile file 1048572 988572 -2\n",
			"/proc/sys/vm/swappiness": "60\n",
			"/proc/pressure/memory":   pressure,
			"/proc/pressure/cpu":      "some avg10=0.00 avg60=0.00 avg300=0.00 total=1\n",
		}}
	}
	rs := checkSwap(context.Background(), env(mk("some avg10=0.00 avg60=0.00 avg300=0.00 total=100\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n")))
	if states(rs) != "ok" || !strings.Contains(rs[0].Summary, "parked") || rs[0].Data["swappiness"] != "60" {
		t.Errorf("parked swap: %+v", rs)
	}
	rs = checkSwap(context.Background(), env(mk("some avg10=40.00 avg60=35.00 avg300=20.00 total=100\nfull avg10=2.00 avg60=1.00 avg300=0.50 total=0\n")))
	if states(rs) != "warn" || !strings.Contains(rs[0].Summary, "under pressure") {
		t.Errorf("full swap under pressure: %+v", rs)
	}
	// No PSI (older kernel): the swap's own numbers, nothing claimed about pressure.
	f := mk("")
	delete(f.Files, "/proc/pressure/memory")
	if rs := checkSwap(context.Background(), env(f)); states(rs) != "ok" || strings.Contains(rs[0].Summary, "parked") {
		t.Errorf("without PSI: %+v", rs)
	}
}

func TestFPMCeilingNamesTheTemplateAndReadsPSI(t *testing.T) {
	f := &sys.Fake{Files: map[string]string{
		"/proc/meminfo":                           "MemTotal:        8155000 kB\n",
		hestia.UsersDir + "/u/web.conf":           "DOMAIN='a.fi' BACKEND='production-PHP-8_2'\nDOMAIN='b.fi' BACKEND='production-PHP-8_2'\nDOMAIN='c.fi' BACKEND='PHP-8_2'\n",
		hestia.FpmTpl + "/production-PHP-8_2.tpl": "pm = dynamic\npm.max_children = 50\npm.min_spare_servers = 10\npm.max_spare_servers = 20\n",
		hestia.FpmTpl + "/PHP-8_2.tpl":            "pm = ondemand\npm.max_children = 8\n",
		"/etc/php/8.2/fpm/pool.d/a.fi.conf":       "pm.max_children = 50\n",
		"/etc/php/8.2/fpm/pool.d/b.fi.conf":       "pm.max_children = 50\n",
		"/etc/php/8.2/fpm/pool.d/c.fi.conf":       "pm.max_children = 8\n",
		"/proc/200/comm":                          "php-fpm8.2\n",
		"/proc/200/smaps_rollup":                  "Rss:              256000 kB\nPss:              205000 kB\n",
		"/proc/201/comm":                          "php-fpm8.2\n",
		"/proc/201/smaps_rollup":                  "Rss:              256000 kB\nPss:              205000 kB\n",
		"/proc/pressure/memory":                   "some avg10=0.00 avg60=0.00 avg300=0.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n",
	}}
	rs := checkFPMCeiling(context.Background(), env(f))
	if len(rs) != 1 || rs[0].State != Warn || !strings.Contains(rs[0].Fix, "fpm-pool-size") {
		t.Fatalf("108 workers × ~150 MB on 8 GB should warn: %+v", rs)
	}
	ev := strings.Join(rs[0].Evidence, "\n")
	if !strings.Contains(ev, "production-PHP-8_2: 2 domains × max_children 50 = 100 workers (a.fi, b.fi)") || !strings.Contains(ev, "PHP-8_2: 1 domain × max_children 8 = 8 workers") || !strings.Contains(ev, "none") {
		t.Errorf("evidence:\n%s", ev)
	}
	ts := FPMTemplates(f)
	if len(ts) != 2 || ts[0].Name != "production-PHP-8_2" || ts[0].Version != "8.2" || ts[0].Mode != "dynamic" || ts[0].MinSpare != 10 || ts[0].Ceiling() != 100 {
		t.Errorf("templates: %+v", ts)
	}
	// Every task stalled 12 % of the last minute: a second, failing result for "now".
	f.Files["/proc/pressure/memory"] = "some avg10=60.00 avg60=45.00 avg300=10.00 total=1\nfull avg10=15.00 avg60=12.00 avg300=3.00 total=0\n"
	rs = checkFPMCeiling(context.Background(), env(f))
	got := map[string]State{}
	for _, r := range rs {
		got[r.Subject] = r.State
	}
	if got["now"] != Fail || got[""] != Warn || len(rs) != 2 {
		t.Errorf("under full pressure: %v", got)
	}
}

func TestNetdataAlarmsGreenOnlyWhenLoaded(t *testing.T) {
	tpl := "alarm: hs_swap_io\n"
	mk := func(installed bool, alarms string) *sys.Fake {
		f := &sys.Fake{
			Commands: map[string]bool{"netdata": true},
			Files: map[string]string{
				"/repo/templates/netdata/health.d/swap.conf":        tpl,
				"/repo/templates/netdata/health.d/hs-pressure.conf": tpl,
			},
			Cmds: map[string]sys.FakeCmd{"systemctl is-active --quiet netdata": {}},
			HTTP: map[string]sys.FakeHTTP{"http://127.0.0.1:19999/api/v1/alarms?all": {Status: 200, Body: alarms}},
		}
		if installed {
			f.Files["/etc/netdata/health.d/swap.conf"], f.Files["/etc/netdata/health.d/hs-pressure.conf"] = tpl, tpl
		}
		return f
	}
	withRepo := func(f *sys.Fake) *Env {
		e := env(f)
		e.RepoDir = "/repo"
		return e
	}
	all := `{"alarms":{"system.memory_some_pressure.hs_ram_pressure":{"name":"hs_ram_pressure"},"system.memory_full_pressure.hs_ram_stall":{"name":"hs_ram_stall"},"mem.swapio.hs_swap_io":{"name":"hs_swap_io"},"system.cpu_some_pressure.hs_cpu_pressure":{"name":"hs_cpu_pressure"},"mem.available.ram_available":{"name":"ram_available"}}}`
	if rs := checkNetdataAlarms(context.Background(), withRepo(mk(false, all))); states(rs) != "warn" || !strings.Contains(rs[0].Fix, "netdata-alarms") {
		t.Errorf("not installed: %+v", rs)
	}
	if rs := checkNetdataAlarms(context.Background(), withRepo(mk(true, all))); states(rs) != "ok" {
		t.Errorf("installed and loaded: %+v", rs)
	}
	stale := strings.Replace(all, `"mem.available.ram_available"`, `"mem.swap.used_swap":{"name":"used_swap"},"mem.available.ram_available"`, 1)
	if rs := checkNetdataAlarms(context.Background(), withRepo(mk(true, stale))); states(rs) != "warn" || !strings.Contains(rs[0].Summary, "used_swap") {
		t.Errorf("files written, not reloaded: %+v", rs)
	}
	noPSI := `{"alarms":{"mem.swapio.hs_swap_io":{"name":"hs_swap_io"}}}`
	if rs := checkNetdataAlarms(context.Background(), withRepo(mk(true, noPSI))); states(rs) != "configured" {
		t.Errorf("a kernel without PSI caps at configured: %+v", rs)
	}
}

// Fleet sweep 2026-09-30: nginx → Apache on the box's own public IP read as
// "outbound to port 8443" on every box.
func TestOutboundSkipsTheBoxToItself(t *testing.T) {
	f := &sys.Fake{Cmds: map[string]sys.FakeCmd{
		"ss -Htanl": {Out: "LISTEN 0 511 94.237.39.7:8443 0.0.0.0:*\nLISTEN 0 511 94.237.39.7:443 0.0.0.0:*\n"},
		"ss -Htanp": {Out: "ESTAB 0 0 94.237.39.7:59318 94.237.39.7:8443 users:((\"nginx\",pid=1,fd=52))\n" +
			"ESTAB 0 0 94.237.39.7:40000 195.72.61.165:8000 users:((\"ulibd\",pid=2,fd=3))\n"},
	}}
	rs := checkOutbound(context.Background(), env(f))
	if len(rs) != 1 || rs[0].Subject != "195.72.61.165:8000" {
		t.Errorf("want only the real outbound: %+v", rs)
	}
}

func TestPkgIntegrityResticSelfUpdate(t *testing.T) {
	f := &sys.Fake{Cmds: map[string]sys.FakeCmd{
		"dpkg --verify":                      {Out: "??5??????   /usr/bin/restic\n"},
		"/usr/bin/restic version":            {Out: "restic 0.19.1 compiled with go1.26.4 on linux/amd64\n"},
		"dpkg-query -W -f=${Version} restic": {Out: "0.14.0-1+b5"},
	}}
	if rs := checkPkgIntegrity(context.Background(), env(f)); rs[0].State != OK {
		t.Errorf("self-updated restic: %+v", rs)
	}
	f.Cmds["/usr/bin/restic version"] = sys.FakeCmd{Out: "restic 0.14.0 compiled with go1.19 on linux/amd64\n"}
	if rs := checkPkgIntegrity(context.Background(), env(f)); rs[0].State != Warn {
		t.Errorf("changed restic that is not newer: %+v", rs)
	}
}

func TestVersionNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"0.19.1", "0.14.0-1+b5", true}, {"0.14.0", "0.14.0-1+b5", false}, {"0.9.9", "0.14.0", false}, {"1.0", "1:0.9", true}, {"x", "0.1", false}} {
		if got := versionNewer(c.a, c.b); got != c.want {
			t.Errorf("versionNewer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

// viona 2026-09-30: the Hestia keyring was an empty file since install.
func TestReposEmptyKeyring(t *testing.T) {
	t.Setenv("HS_STATE_DIR", t.TempDir())
	f := &sys.Fake{Files: map[string]string{
		"/etc/apt/sources.list.d/hestia.list":    "deb [arch=amd64 signed-by=/usr/share/keyrings/hestia-keyring.gpg] https://apt.hestiacp.com/ bookworm main\n",
		"/usr/share/keyrings/hestia-keyring.gpg": "",
	}}
	rs := checkRepos(context.Background(), env(f))
	if len(rs) != 1 || rs[0].State != Fail || !strings.Contains(strings.Join(rs[0].Evidence, " "), "is empty") {
		t.Errorf("empty keyring: %+v", rs)
	}
	a := GetAptStatus(context.Background(), env(f))
	if a.Repos.Total != 1 || len(a.Repos.Failing) != 1 || a.Repos.Failing[0].Host != "apt.hestiacp.com" {
		t.Errorf("apt status: %+v", a)
	}
}

// soutuveneet 2026-09-30: timers enabled, 20auto-upgrades missing → the fix
// is hs op unattended, not "enable the apt timers".
func TestUnattendedCause(t *testing.T) {
	f := &sys.Fake{Files: map[string]string{
		"/etc/apt/apt.conf.d/50unattended-upgrades": "Unattended-Upgrade::Origins-Pattern {\n        \"origin=Debian,codename=${distro_codename},label=Debian-Security\";\n};\n",
	}, Cmds: map[string]sys.FakeCmd{
		"dpkg-query -W -f=${Status} unattended-upgrades": {Out: "install ok installed"},
		"systemctl is-enabled apt-daily.timer":           {Out: "enabled\n"},
		"systemctl is-enabled apt-daily-upgrade.timer":   {Out: "enabled\n"},
	}}
	rs := checkUnattended(context.Background(), env(f))
	if rs[0].Data["cause"] != "periodic" || !strings.Contains(rs[0].Fix, "hs op unattended") {
		t.Errorf("missing 20auto-upgrades: %+v", rs)
	}
	f.Files["/etc/apt/apt.conf.d/20auto-upgrades"] = "APT::Periodic::Update-Package-Lists \"1\";\nAPT::Periodic::Unattended-Upgrade \"1\";\n"
	f.Cmds["systemctl is-enabled apt-daily-upgrade.timer"] = sys.FakeCmd{Out: "disabled\n", Code: 1}
	if rs := checkUnattended(context.Background(), env(f)); rs[0].Data["cause"] != "timers" {
		t.Errorf("timers off: %+v", rs)
	}
	f.Files["/etc/apt/apt.conf.d/99off"] = "APT::Periodic::Unattended-Upgrade \"0\";\n"
	if rs := checkUnattended(context.Background(), env(f)); rs[0].Data["cause"] != "periodic" {
		t.Errorf("a later file setting 0 wins: %+v", rs)
	}
}
