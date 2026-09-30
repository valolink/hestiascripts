package fix

import (
	"context"
	"strings"
	"testing"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/sys"
)

func stagingFake() *sys.Fake {
	return &sys.Fake{Files: map[string]string{
		"/usr/local/hestia/data/users/sv/user.conf":             "",
		"/usr/local/hestia/data/users/sv/web.conf":              "DOMAIN='soutuveneet.fi' IP='1.1.1.1'\nDOMAIN='staging.soutuveneet.fi' IP='1.1.1.1'\n",
		"/root/.hestia-site-copies/staging.soutuveneet.fi.conf": "KIND=staging\nDOMAIN=staging.soutuveneet.fi\nUSER=sv\nSOURCE_DOMAIN=soutuveneet.fi\nSOURCE_USER=sv\n",
		"/proc/self/mountinfo":                                  "25 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n",
		"/etc/fstab":                                            "UUID=x / ext4 defaults 0 1\n",
	}, Dirs: []string{
		"/home/sv/web/soutuveneet.fi/public_html/wp-content/uploads",
		"/home/sv/web/staging.soutuveneet.fi/public_html/wp-content/uploads",
	}}
}

func TestStagingUploadsPlan(t *testing.T) {
	f := stagingFake()
	fx, _ := ByID("staging-uploads")
	steps, err := fx.Plan(context.Background(), &check.Env{Sys: f}, "staging.soutuveneet.fi")
	if err != nil {
		t.Fatal(err)
	}
	all := ""
	for _, s := range steps {
		all += strings.Join(s.Argv, " ") + "\n"
	}
	for _, want := range []string{"mount --bind /home/sv/web/soutuveneet.fi/public_html/wp-content/uploads /home/sv/web/staging.soutuveneet.fi/public_html/wp-content/uploads", "remount,ro,bind", ".hestia-ro-probe", "findmnt --verify"} {
		if !strings.Contains(all, want) {
			t.Errorf("plan lacks %q:\n%s", want, all)
		}
	}
	// Mounted read-only but not persisted → only the fstab steps.
	f.Files["/proc/self/mountinfo"] += "90 25 8:1 /home/sv/web/soutuveneet.fi/public_html/wp-content/uploads /home/sv/web/staging.soutuveneet.fi/public_html/wp-content/uploads ro,relatime shared:1 - ext4 /dev/sda1 rw\n"
	steps, err = fx.Plan(context.Background(), &check.Env{Sys: f}, "staging.soutuveneet.fi")
	if err != nil || strings.Contains(steps[0].Argv[0]+steps[1].Argv[0], "mount") {
		t.Errorf("persist only: %v %+v", err, steps)
	}
	// Mounted and persisted → nothing to do.
	f.Files["/etc/fstab"] += "/home/sv/web/soutuveneet.fi/public_html/wp-content/uploads /home/sv/web/staging.soutuveneet.fi/public_html/wp-content/uploads none bind,ro,nofail 0 0 # hestia-staging-uploads /home/sv/web/staging.soutuveneet.fi/public_html/wp-content/uploads\n"
	if _, err := fx.Plan(context.Background(), &check.Env{Sys: f}, "staging.soutuveneet.fi"); err == nil {
		t.Error("nothing to do should refuse")
	}
}
