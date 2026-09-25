package fix

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/sys"
	"github.com/valolink/hestiascripts/internal/wpfiles"
)

func fakeBox() *sys.Fake {
	return &sys.Fake{Clock: time.Now(), Files: map[string]string{
		hestia.UsersDir + "/renea/web.conf":                           "DOMAIN='renea.demolink.fi' PROXY='default'\n",
		hestia.Root + "/data/firewall/rules.conf":                     "RULE='3' PORT='22' IP='0.0.0.0/0'\nRULE='11' ACTION='ACCEPT' PORT='19999' IP='0.0.0.0/0'\n",
		"/home/renea/web/renea.demolink.fi/public_html/wp-config.php": "define('WP_DEBUG', true);",
	}, Cmds: map[string]sys.FakeCmd{}}
}

func steps(t *testing.T, id, subject string, f *sys.Fake) []Step {
	fx, ok := ByID(id)
	if !ok {
		t.Fatalf("no fix %s", id)
	}
	st, err := fx.Plan(context.Background(), &check.Env{Sys: f}, subject)
	if err != nil {
		t.Fatalf("%s: %v", id, err)
	}
	return st
}

func TestDumpsPlanMovesEachFileIntoPrivate(t *testing.T) {
	f := fakeBox()
	root := "/home/renea/web/renea.demolink.fi/public_html"
	f.Cmds["find "+root+" -maxdepth 2 "+strings.Join(wpfiles.DumpPatterns, " ")] =
		sys.FakeCmd{Out: root + "/renea.sql\n" + root + "/old/test.sql\n"}
	st := steps(t, "dumps", "renea.demolink.fi", f)
	if len(st) != 3 {
		t.Fatalf("mkdir + 2 moves, got %d: %+v", len(st), st)
	}
	dest := "/home/renea/web/renea.demolink.fi/private/hs-moved-" + today()
	if got := Quote(st[0].Argv); got != "install -d -o renea -g renea -m 750 "+dest {
		t.Errorf("mkdir: %s", got)
	}
	if got := Quote(st[1].Argv); got != "mv -n -v -- "+root+"/old/test.sql "+dest+"/old__test.sql" {
		t.Errorf("move: %s", got)
	}
}

func TestEmptyPlanWhenResolved(t *testing.T) {
	if st := steps(t, "dumps", "renea.demolink.fi", fakeBox()); len(st) != 0 {
		t.Errorf("no dumps → nothing to do, got %+v", st)
	}
}

func TestNetdataPortDeletesOnlyThatRule(t *testing.T) {
	st := steps(t, "netdata-port", "", fakeBox())
	if len(st) != 1 || Quote(st[0].Argv) != bin+"v-delete-firewall-rule 11" {
		t.Errorf("%+v", st)
	}
}

func TestSubjectsAreValidated(t *testing.T) {
	fx, _ := ByID("hestia-conf-key")
	if _, err := fx.Plan(context.Background(), &check.Env{Sys: fakeBox()}, "FTP_SYSTEM; reboot"); err == nil {
		t.Error("a non-key subject must be refused")
	}
	d, _ := ByID("dumps")
	if _, err := d.Plan(context.Background(), &check.Env{Sys: fakeBox()}, "../../etc"); err == nil {
		t.Error("an unknown domain must be refused")
	}
}

func TestDebugLogPlanTurnsDebugOffFirst(t *testing.T) {
	f := fakeBox()
	f.Files["/home/renea/web/renea.demolink.fi/public_html/wp-content/debug.log"] = "x"
	st := steps(t, "debug-log", "renea.demolink.fi", f)
	if len(st) < 3 || !strings.Contains(Quote(st[0].Argv), "config set WP_DEBUG false --raw") || !strings.Contains(Quote(st[2].Argv), "debug.log") {
		t.Errorf("%+v", st)
	}
}

func TestEveryFixHasACheckAndPlan(t *testing.T) {
	for _, f := range All() {
		if f.Check == "" || f.Plan == nil || f.Title == "" {
			t.Errorf("incomplete fix %+v", f.ID)
		}
	}
}

func TestSpecificFixBeforeDiagnosis(t *testing.T) {
	r := check.Result{Check: "systemd.failed", Subject: "maldet.service", State: check.Fail}
	fs := For(r)
	if len(fs) < 2 || fs[0].ID != "maldet-deps-unit" || fs[len(fs)-1].ID != "unit-diagnose" {
		var ids []string
		for _, f := range fs {
			ids = append(ids, f.ID)
		}
		t.Errorf("order: %v", ids)
	}
}

func redisBox() *sys.Fake {
	dropin := " * Plugin Name: Redis Object Cache Drop-In\n * Version: 2.8.0"
	f := &sys.Fake{Clock: time.Now(), Files: map[string]string{
		hestia.UsersDir + "/w/web.conf": "DOMAIN='valolink.fi'\nDOMAIN='kluuvi.fi'\nDOMAIN='solo.fi'\n",
		"/home/w/web/valolink.fi/public_html/wp-config.php":            "define( 'WP_REDIS_PREFIX', 'valolink_fi_1830bfa8' );",
		"/home/w/web/valolink.fi/public_html/wp-content/object-cache.php": dropin,
		"/home/w/web/kluuvi.fi/public_html/wp-config.php":              "define( 'WP_REDIS_PREFIX', 'kluuvi_fi_794c6b23' );",
		"/home/w/web/kluuvi.fi/public_html/wp-content/object-cache.php":   dropin,
		"/home/w/web/solo.fi/public_html/wp-config.php":                "define('WP_REDIS_PREFIX','solo_1'); define('WP_REDIS_DATABASE', 1);",
		"/home/w/web/solo.fi/public_html/wp-content/object-cache.php":     dropin,
	}, Cmds: map[string]sys.FakeCmd{
		"redis-cli config get databases": {Out: "databases\n16\n"},
		"redis-cli info keyspace":        {Out: "# Keyspace\ndb0:keys=51807,expires=187\ndb1:keys=900,expires=900\ndb2:keys=5,expires=0\n"},
	}}
	return f
}

// hzweb1 shape: valolink.fi and kluuvi.fi share database 0.
func TestRedisOwnDBPicksAFreeDatabaseAndNeverFlushesTheSharedOne(t *testing.T) {
	st := steps(t, "redis-own-db", "valolink.fi", redisBox())
	plan := ""
	for _, s := range st {
		plan += strings.Join(s.Argv, " ") + "\n"
	}
	// 1 is named by solo.fi, 2 holds unknown keys → 3
	if !strings.Contains(plan, "config set WP_REDIS_DATABASE 3 --raw --type=constant") {
		t.Errorf("free database:\n%s", plan)
	}
	if !strings.Contains(plan, "redis-cli -n 0 --scan --pattern 'valolink_fi_1830bfa8*' | xargs") || strings.Contains(plan, "flushdb") {
		t.Errorf("old keys must go by prefix, never FLUSHDB of the shared database:\n%s", plan)
	}
	if !strings.Contains(plan, "WP_REDIS_MAXTTL 86400") {
		t.Errorf("missing MAXTTL should be added on the way:\n%s", plan)
	}
}

func TestRedisMaxTTLOnOwnDatabaseFlushesInBackground(t *testing.T) {
	st := steps(t, "redis-maxttl", "solo.fi", redisBox())
	if len(st) != 2 || Quote(st[1].Argv) != "redis-cli -n 1 flushdb async" {
		t.Errorf("%+v", st)
	}
}
