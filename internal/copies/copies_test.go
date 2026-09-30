package copies

import (
	"testing"

	"github.com/valolink/hestiascripts/internal/sys"
)

func TestList(t *testing.T) {
	f := &sys.Fake{Files: map[string]string{
		"/usr/local/hestia/data/users/del_user/user.conf":                  "",
		"/usr/local/hestia/data/users/del_user/web.conf":                   "DOMAIN='delicatessen.fi'\nDOMAIN='staging.delicatessen.fi'\nDOMAIN='dev.delicatessen.fi'\nDOMAIN='clone.delicatessen.fi'\n",
		URLStoreDir + "/del_user_delicatessen.fi":                          "https://staging.delicatessen.fi\n",
		RegistryDir + "/clone.delicatessen.fi.conf":                        "KIND=clone\nDOMAIN=clone.delicatessen.fi\nUSER=del_user\nSOURCE_DOMAIN=delicatessen.fi\nSOURCE_USER=del_user\n",
		"/home/del_user/web/dev.delicatessen.fi/public_html/wp-config.php": "<?php\ndefine( 'WP_ENVIRONMENT_TYPE', 'staging' );\n",
	}}
	got := map[string]Copy{}
	for _, c := range List(f) {
		got[c.Domain] = c
	}
	if c := got["staging.delicatessen.fi"]; c.Origin != "url-store" || c.SourceUser != "del_user" || c.SourceDomain != "delicatessen.fi" {
		t.Errorf("url store (user with underscore): %+v", c)
	}
	if c := got["clone.delicatessen.fi"]; c.Kind != "clone" {
		t.Errorf("registry clone: %+v", c)
	}
	if c := got["dev.delicatessen.fi"]; c.Origin != "wp-config" || c.SourceDomain != "" {
		t.Errorf("wp-config staging: %+v", c)
	}
	if _, ok := got["delicatessen.fi"]; ok {
		t.Error("live site listed as a copy")
	}
}

func TestMountAt(t *testing.T) {
	f := &sys.Fake{Files: map[string]string{"/proc/self/mountinfo": "90 25 8:1 /home/a\\040b/uploads /home/s/uploads ro,relatime shared:1 - ext4 /dev/sda1 rw\n"}}
	m := MountAt(f, "/home/s/uploads")
	if !m.Mounted || !m.ReadOnly || m.Root != "/home/a b/uploads" {
		t.Errorf("%+v", m)
	}
	if MountAt(f, "/home/other").Mounted {
		t.Error("unmounted point read as mounted")
	}
}
