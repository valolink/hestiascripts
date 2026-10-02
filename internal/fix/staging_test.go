package fix

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/sys"
)

const (
	liveUp = "/home/sv/web/soutuveneet.fi/public_html/wp-content/uploads"
	stgUp  = "/home/sv/web/staging.soutuveneet.fi/public_html/wp-content/uploads"
)

// Live has 2025, 2026/09 and 2026/10; on 2026-10-02 the copy binds 2025 and
// 2026/09 (the current month is the copy's own).
func stagingFake() *sys.Fake {
	return &sys.Fake{Clock: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC), Files: map[string]string{
		"/usr/local/hestia/data/users/sv/user.conf":             "",
		"/usr/local/hestia/data/users/sv/web.conf":              "DOMAIN='soutuveneet.fi' IP='1.1.1.1'\nDOMAIN='staging.soutuveneet.fi' IP='1.1.1.1'\n",
		"/root/.hestia-site-copies/staging.soutuveneet.fi.conf": "KIND=staging\nDOMAIN=staging.soutuveneet.fi\nUSER=sv\nSOURCE_DOMAIN=soutuveneet.fi\nSOURCE_USER=sv\n",
		"/proc/self/mountinfo":                                  "25 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n",
		"/etc/fstab":                                            "UUID=x / ext4 defaults 0 1\n",
		liveUp + "/2026/10/a.jpg":                               "x",
	}, Dirs: []string{
		liveUp, liveUp + "/2025", liveUp + "/2026", liveUp + "/2026/09", liveUp + "/2026/10",
		stgUp, stgUp + "/2026",
	}}
}

func TestStagingUploadsPlan(t *testing.T) {
	f := stagingFake()
	fx, _ := ByID("staging-uploads")
	plan := func() (string, error) {
		steps, err := fx.Plan(context.Background(), &check.Env{Sys: f}, "staging.soutuveneet.fi")
		all := ""
		for _, s := range steps {
			all += strings.Join(s.Argv, " ") + "\n"
		}
		return all, err
	}
	all, err := plan()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"mount --bind " + liveUp + "/2025 " + stgUp + "/2025", "mount --bind " + liveUp + "/2026/09 " + stgUp + "/2026/09", "remount,ro,bind", "--make-private", ".hestia-ro-probe", "findmnt --verify"} {
		if !strings.Contains(all, want) {
			t.Errorf("plan lacks %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "2026/10") {
		t.Errorf("the current month is the copy's own, not a bind:\n%s", all)
	}
	// Mounted read-only and private, not persisted → only the fstab steps.
	f.Files["/proc/self/mountinfo"] += "90 25 8:1 " + liveUp + "/2025 " + stgUp + "/2025 ro,relatime - ext4 /dev/sda1 rw\n" +
		"91 25 8:1 " + liveUp + "/2026/09 " + stgUp + "/2026/09 ro,relatime - ext4 /dev/sda1 rw\n"
	if all, err = plan(); err != nil || strings.Contains(all, "mount --bind") || !strings.Contains(all, "bind,ro,private,nofail") {
		t.Errorf("persist only: %v\n%s", err, all)
	}
	// Persisted → nothing to do.
	for _, rel := range []string{"2025", "2026/09"} {
		f.Files["/etc/fstab"] += liveUp + "/" + rel + " " + stgUp + "/" + rel + " none bind,ro,private,nofail 0 0 # hestia-staging-uploads " + stgUp + "/" + rel + "\n"
	}
	if _, err := plan(); err == nil {
		t.Error("nothing to do should refuse")
	}
	// The old layout (all of uploads one bind) is rebuilt by v-wp-staging-create, not patched.
	f.Files["/proc/self/mountinfo"] += "92 25 8:1 " + liveUp + " " + stgUp + " ro,relatime shared:1 - ext4 /dev/sda1 rw\n"
	if _, err := plan(); err == nil || !strings.Contains(err.Error(), "--uploads-only") {
		t.Errorf("old layout: %v", err)
	}
}
