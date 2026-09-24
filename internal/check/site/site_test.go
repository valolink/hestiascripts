package site

import (
	"context"
	"strings"
	"testing"
	"time"

	. "github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/logcap"
	"github.com/valolink/hestiascripts/internal/sys"
)

var now = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

var dom = hestia.Domain{User: "alavus", Name: "alavusikkunat.fi", SSL: true}

func wpCmd(args ...string) string {
	return "runuser -u alavus -- env HOME=/home/alavus wp --path=/home/alavus/web/alavusikkunat.fi/public_html " + strings.Join(args, " ")
}

func env(cmds map[string]sys.FakeCmd) *Env {
	return &Env{Sys: &sys.Fake{Clock: now, Cmds: cmds}}
}

// alavus 2026-09-24: the webshell sat in wp-includes/sodium_compat.
func TestCoreChecksumsCatchAddedFile(t *testing.T) {
	e := env(map[string]sys.FakeCmd{
		wpCmd("core", "version"): {Out: "7.1.2\n"},
		wpCmd("core", "verify-checksums"): {Code: 1, Stderr: "Warning: File should not exist: wp-includes/sodium_compat/namespaced/Core/Curve25519/Ge/zwfile.php\n" +
			"Warning: File should not exist: wp-admin/.rnd\nError: WordPress installation doesn't verify against checksums.\n"},
	})
	rs := checkCore(context.Background(), e, dom)
	if len(rs) != 1 || rs[0].State != Fail || rs[0].Data["wp"] != "7.1.2" || !strings.Contains(strings.Join(rs[0].Evidence, " "), "zwfile.php") {
		t.Fatalf("got %+v", rs)
	}

	clean := env(map[string]sys.FakeCmd{
		wpCmd("core", "version"):          {Out: "7.1.2\n"},
		wpCmd("core", "verify-checksums"): {Out: "Success: WordPress installation verifies against checksums.\n"},
	})
	if rs := checkCore(context.Background(), clean, dom); rs[0].State != OK {
		t.Errorf("clean: %+v", rs)
	}

	// chat.demolink.fi 2026-09-24: a missing file is not a backdoor.
	missing := env(map[string]sys.FakeCmd{
		wpCmd("core", "version"):          {Out: "6.2.12\n"},
		wpCmd("core", "verify-checksums"): {Code: 1, Stderr: "Warning: File doesn't exist: index.php\nError: WordPress installation doesn't verify against checksums.\n"},
	})
	if rs := checkCore(context.Background(), missing, dom); rs[0].State != Warn || !strings.Contains(rs[0].Summary, "1 core files missing") {
		t.Errorf("missing file: %+v", rs)
	}

	offline := env(map[string]sys.FakeCmd{
		wpCmd("core", "version"):          {Out: "7.1.2\n"},
		wpCmd("core", "verify-checksums"): {Code: 1, Stderr: "Error: Couldn't get checksums from WordPress.org.\n"},
	})
	if rs := checkCore(context.Background(), offline, dom); rs[0].State != Unknown {
		t.Errorf("offline must be unknown, not ok/fail: %+v", rs)
	}
}

func TestUpdatesParsesPluginsAndCore(t *testing.T) {
	e := env(map[string]sys.FakeCmd{
		wpCmd("plugin", "list", "--update=available", "--skip-update-check", "--fields=name,version,update_version", "--format=csv", "--skip-plugins", "--skip-themes"): {
			Out: "name,version,update_version\nforminator,1.57.2,1.57.3\nwoocommerce,11.0.1,11.1.2\n"},
		wpCmd("transient", "get", "update_core", "--network", "--format=json", "--skip-plugins", "--skip-themes"): {
			Out: `{"updates":[{"response":"upgrade","current":"7.1.3"},{"response":"autoupdate","current":"7.1.3"}]}`},
	})
	rs := checkUpdates(context.Background(), e, dom)
	ev := strings.Join(rs[0].Evidence, " ")
	if rs[0].State != Warn || rs[0].Data["updates"] != "3" || !strings.Contains(ev, "forminator 1.57.2 → 1.57.3") || !strings.Contains(ev, "core → 7.1.3") {
		t.Fatalf("got %+v", rs)
	}
}

func TestHTTP(t *testing.T) {
	f := &sys.Fake{Clock: now, HTTP: map[string]sys.FakeHTTP{
		"local https://alavusikkunat.fi/": {Status: 301, Body: "https://www.alavusikkunat.fi/"},
	}}
	rs := checkHTTP(context.Background(), &Env{Sys: f}, dom)
	if rs[0].State != OK || rs[0].Data["http"] != "301" {
		t.Errorf("redirect: %+v", rs)
	}
	f.HTTP["local https://alavusikkunat.fi/"] = sys.FakeHTTP{Status: 502}
	if rs := checkHTTP(context.Background(), &Env{Sys: f}, dom); rs[0].State != Fail {
		t.Errorf("502: %+v", rs)
	}
}

func TestDebug(t *testing.T) {
	t.Setenv("HS_ETC_DIR", t.TempDir())
	t.Setenv("HS_CRON_DIR", t.TempDir())
	f := &sys.Fake{Files: map[string]string{dom.DocRoot() + "/wp-config.php": "<?php\ndefine( 'WP_DEBUG', true );\n"}}
	if rs := checkDebug(&Env{Sys: f}, dom); rs[0].State != Warn || !strings.Contains(rs[0].Summary, "shown to visitors") {
		t.Errorf("debug on, display default: %+v", rs)
	}
	priv := "/home/alavus/web/alavusikkunat.fi/private/wp-debug.log"
	f.Files[dom.DocRoot()+"/wp-config.php"] = "<?php\ndefine('WP_DEBUG', true);\ndefine('WP_DEBUG_DISPLAY', false);\ndefine( 'WP_DEBUG_LOG', '" + priv + "' );\n"
	if rs := checkDebug(&Env{Sys: f}, dom); rs[0].State != Warn || !strings.Contains(rs[0].Summary, "not size-capped") {
		t.Errorf("private but uncapped: %+v", rs)
	}
	logcap.Add(dom.Name, priv, "alavus", "50M")
	if rs := checkDebug(&Env{Sys: f}, dom); rs[0].State != OK || !strings.Contains(rs[0].Summary, "by choice") {
		t.Errorf("kept on deliberately: %+v", rs)
	}
	f.Files[dom.DocRoot()+"/wp-config.php"] = "<?php\n// define('WP_DEBUG', true);\ndefine('WP_DEBUG', false);\n"
	if rs := checkDebug(&Env{Sys: f}, dom); rs[0].State != OK {
		t.Errorf("debug off (commented line must not count): %+v", rs)
	}
}

func TestUpdatesReportsDatabaseFailure(t *testing.T) {
	e := env(map[string]sys.FakeCmd{
		wpCmd("plugin", "list", "--update=available", "--skip-update-check", "--fields=name,version,update_version", "--format=csv", "--skip-plugins", "--skip-themes"): {
			Code: 1, Stderr: "Error: Error establishing a database connection."},
	})
	if rs := checkUpdates(context.Background(), e, dom); rs[0].State != Fail || !strings.Contains(rs[0].Summary, "database") {
		t.Errorf("got %+v", rs)
	}
}

// boostwith.ai 2026-09-24: 301 to www in front of a dead database.
func TestHTTPFollowsOwnRedirects(t *testing.T) {
	d := dom
	d.Aliases = []string{"www.alavusikkunat.fi"}
	f := &sys.Fake{Clock: now, HTTP: map[string]sys.FakeHTTP{
		"local https://alavusikkunat.fi/":     {Status: 301, Body: "https://www.alavusikkunat.fi/"},
		"local https://www.alavusikkunat.fi/": {Status: 500},
	}}
	if rs := checkHTTP(context.Background(), &Env{Sys: f}, d); rs[0].State != Fail || rs[0].Data["http"] != "500" {
		t.Errorf("own redirect to a 500: %+v", rs)
	}
	f.HTTP["local https://alavusikkunat.fi/"] = sys.FakeHTTP{Status: 302, Body: "https://login.example.com/"}
	if rs := checkHTTP(context.Background(), &Env{Sys: f}, d); rs[0].State != OK {
		t.Errorf("foreign redirect is not followed: %+v", rs)
	}
}

// riverfinland.fi 2026-09-24: https→http→https on a stale copy; DNS elsewhere.
func TestRedirectLoopAndDNS(t *testing.T) {
	d := dom
	d.IP = "37.27.188.192"
	f := &sys.Fake{Clock: now, HTTP: map[string]sys.FakeHTTP{
		"local https://alavusikkunat.fi/": {Status: 301, Body: "http://alavusikkunat.fi/"},
		"local http://alavusikkunat.fi/":  {Status: 301, Body: "https://alavusikkunat.fi/"},
	}, DNS: map[string][]string{"alavusikkunat.fi": {"95.217.1.1"}}}
	rs := checkHTTP(context.Background(), &Env{Sys: f}, d)
	if rs[0].State != Warn || rs[0].Data["dns"] != "elsewhere" {
		t.Errorf("loop on a stale copy: %+v", rs)
	}
	f.DNS["alavusikkunat.fi"] = []string{"37.27.188.192"}
	if rs := checkHTTP(context.Background(), &Env{Sys: f}, d); rs[0].State != Fail || rs[0].Data["dns"] != "here" {
		t.Errorf("loop on the live copy: %+v", rs)
	}
}
