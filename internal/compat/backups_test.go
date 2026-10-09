package compat

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/sys"
)

// Copied from box.rcloneArgs: the fake keys commands by their full argv.
const rcloneArgs = "rclone.args=serve restic --stdio --b2-hard-delete --retries 1 --low-level-retries 1"

func resticCmd(user string) string {
	return "restic --repo rclone:storagebox:hestia-web1/" + user + " --password-file " + hestia.UsersDir + "/" + user +
		"/restic.conf -o " + rcloneArgs + " --no-lock --json snapshots --latest 1"
}

// 2026-10-09: a fresh tarball must not make a user look backed up to EngineLink.
func TestBackupsCountResticOnly(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.Local)
	snaps, _ := json.Marshal([]map[string]any{
		{"time": now.Add(-9 * time.Hour).Format(time.RFC3339), "paths": []string{"/home/ok"}},
		{"time": now.Add(-40 * time.Minute).Format(time.RFC3339), "tags": []string{"db-hourly"}},
	})
	f := &sys.Fake{
		Clock:    now,
		Commands: map[string]bool{"restic": true},
		Files: map[string]string{
			hestia.ResticSys:                       "REPO='rclone:storagebox:hestia-web1'\n",
			hestia.UsersDir + "/ok/restic.conf":    "k",
			hestia.UsersDir + "/gone/restic.conf":  "k",
			hestia.UsersDir + "/tar/user.conf":     "",
			"/backup/tar.2026-10-09_05-10-01.tar":  "x",
			"/backup/gone.2026-10-09_05-10-01.tar": "x",
		},
		ModTimes: map[string]time.Time{
			"/backup/tar.2026-10-09_05-10-01.tar":  now.Add(-7 * time.Hour),
			"/backup/gone.2026-10-09_05-10-01.tar": now.Add(-7 * time.Hour),
		},
		Cmds: map[string]sys.FakeCmd{
			resticCmd("ok"):   {Out: string(snaps)},
			resticCmd("gone"): {Code: 10, Stderr: "Fatal: repository does not exist: unable to open config file"},
		},
	}
	got := map[string]BackupEntry{}
	for _, e := range resticBackups(context.Background(), &check.Env{Sys: f}, now) {
		got[e.User] = e
	}
	if e := got["ok"]; e.AgeHours == nil || *e.AgeHours != 9 || e.HourlyAgeHours == nil || *e.HourlyAgeHours != 0 {
		t.Errorf("restic user: %+v", e)
	}
	for _, u := range []string{"tar", "gone"} {
		if e, ok := got[u]; !ok || e.AgeHours != nil || e.Error == "" {
			t.Errorf("%s has only a tarball and must read as missing: %+v", u, e)
		}
	}
	if got["tar"].Error != "not enrolled in restic yet (no key; the nightly creates it)" {
		t.Errorf("tar reason: %q", got["tar"].Error)
	}
}
