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

func TestRedisSitesCollideOnlyOnSameDatabaseAndPrefix(t *testing.T) {
	site := func(name, conf string) (string, string, string, string) {
		root := "/home/u/web/" + name + "/public_html"
		return root + "/wp-config.php", conf + "\ndefine('WP_REDIS_DISABLE_GROUP_FLUSH', true);", root + "/wp-content/object-cache.php", "<?php"
	}
	mk := func(a, b string) *sys.Fake {
		f := &sys.Fake{Files: map[string]string{
			hestia.UsersDir + "/u/web.conf": "DOMAIN='a.fi' PROXY='wp-secure'\nDOMAIN='b.fi' PROXY='wp-secure'\n",
		}}
		for _, x := range [][2]string{{"a.fi", a}, {"b.fi", b}} {
			k1, v1, k2, v2 := site(x[0], x[1])
			f.Files[k1], f.Files[k2] = v1, v2
		}
		return f
	}
	same := checkRedisSites(context.Background(), env(mk(
		"define('WP_REDIS_PREFIX', 'shop_');", "define( 'WP_REDIS_PREFIX', 'shop_' );")))
	if states(same) != "fail" {
		t.Errorf("same db+prefix: %s", states(same))
	}
	split := checkRedisSites(context.Background(), env(mk(
		"define('WP_REDIS_PREFIX', 'shop_'); define('WP_REDIS_DATABASE', 1);",
		"define('WP_REDIS_PREFIX', 'shop_'); define('WP_REDIS_DATABASE', 2);")))
	if states(split) != "ok" {
		t.Errorf("separate databases: %s %v", states(split), split)
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
