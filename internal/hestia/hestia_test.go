package hestia

import (
	"testing"

	"github.com/valolink/hestiascripts/internal/sys"
)

func TestParseKV(t *testing.T) {
	kv := ParseKV(`DOMAIN='alavusikkunat.fi' IP='1.2.3.4' ALIAS='www.alavusikkunat.fi,x.fi' PROXY='wp-rocket' SSL='yes' EMPTY=''`)
	want := map[string]string{"DOMAIN": "alavusikkunat.fi", "IP": "1.2.3.4", "ALIAS": "www.alavusikkunat.fi,x.fi", "PROXY": "wp-rocket", "SSL": "yes", "EMPTY": ""}
	for k, v := range want {
		if kv[k] != v {
			t.Errorf("%s = %q, want %q", k, kv[k], v)
		}
	}
	if got := ParseKV(`VERSION='1.9.4'`)["VERSION"]; got != "1.9.4" {
		t.Errorf("VERSION = %q", got)
	}
	if got := ParseKV(`BACKUP_INCREMENTAL=yes`)["BACKUP_INCREMENTAL"]; got != "yes" {
		t.Errorf("unquoted = %q", got)
	}
}

func TestWebDomainsAndRestic(t *testing.T) {
	f := &sys.Fake{Files: map[string]string{
		UsersDir + "/alavus/web.conf":   "DOMAIN='alavusikkunat.fi' PROXY='wp-rocket' SSL='yes'\nDOMAIN='kehitys.alavusikkunat.fi' PROXY='default' SSL='no' SUSPENDED='no'\n",
		UsersDir + "/alavus/db.conf":    "DB='alavus_wp' DBUSER='x'\n",
		UsersDir + "/staging/user.conf": "NAME='s'\n",
		ResticSys:                       "REPO='rclone:storagebox:hestia-alavus'\nSNAPSHOTS='48'\n",
	}}
	doms := WebDomains(f)
	if len(doms) != 2 || doms[0].Name != "alavusikkunat.fi" || doms[0].Proxy != "wp-rocket" || !doms[0].SSL {
		t.Fatalf("domains = %+v", doms)
	}
	if doms[1].DocRoot() != "/home/alavus/web/kehitys.alavusikkunat.fi/public_html" {
		t.Errorf("docroot = %s", doms[1].DocRoot())
	}
	if u := Users(f); len(u) != 2 || u[0] != "alavus" || u[1] != "staging" {
		t.Errorf("users = %v", u)
	}
	if u := UsersWithDatabases(f); len(u) != 1 || u[0] != "alavus" {
		t.Errorf("db users = %v", u)
	}
	if r := ResticRepo(f); r != "rclone:storagebox:hestia-alavus" {
		t.Errorf("repo = %q", r)
	}
}
