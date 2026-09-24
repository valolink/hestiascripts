package logcap

import (
	"os"
	"strings"
	"testing"
)

func TestAddReplaceRemove(t *testing.T) {
	t.Setenv("HS_ETC_DIR", t.TempDir())
	t.Setenv("HS_CRON_DIR", t.TempDir())
	p := "/home/alavus/web/alavusikkunat.fi/private/wp-debug.log"
	if err := Add("alavusikkunat.fi", p, "alavus", "50M"); err != nil {
		t.Fatal(err)
	}
	if err := Add("x.fi", "/home/x/web/x.fi/private/wp-debug.log", "x", "20M"); err != nil {
		t.Fatal(err)
	}
	if err := Add("alavusikkunat.fi", p, "alavus", "100M"); err != nil { // replace, not duplicate
		t.Fatal(err)
	}
	conf, _ := os.ReadFile(ConfPath())
	if strings.Count(string(conf), "# hs:logcap alavusikkunat.fi") != 1 {
		t.Fatalf("duplicate block:\n%s", conf)
	}
	if s, ok := Managed(p); !ok || s != "100M" {
		t.Errorf("managed %v %q", ok, s)
	}
	if !strings.Contains(string(conf), "su alavus alavus") || !strings.Contains(string(conf), "copytruncate") {
		t.Errorf("block:\n%s", conf)
	}
	if b, _ := os.ReadFile(CronPath()); !strings.Contains(string(b), "logrotate -s /var/lib/hs/logcap.state "+ConfPath()) {
		t.Errorf("cron: %s", b)
	}
	Remove("alavusikkunat.fi")
	Remove("x.fi")
	if len(Entries()) != 0 {
		t.Errorf("entries left: %v", Entries())
	}
	if _, err := os.Stat(CronPath()); !os.IsNotExist(err) {
		t.Error("cron entry should go with the last block")
	}
}

func TestRefusesUnsafe(t *testing.T) {
	t.Setenv("HS_ETC_DIR", t.TempDir())
	t.Setenv("HS_CRON_DIR", t.TempDir())
	for _, c := range [][4]string{
		{"k", "relative.log", "u", "50M"},
		{"k", "/a/../etc/passwd", "u", "50M"},
		{"k", "/a.log {\n}", "u", "50M"},
		{"k", "/a.log", "u u", "50M"},
	} {
		if err := Add(c[0], c[1], c[2], c[3]); err == nil {
			t.Errorf("accepted %q", c)
		}
	}
}
