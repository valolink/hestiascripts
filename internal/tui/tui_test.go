package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/sys"
)

func testModel(t *testing.T) *model {
	t.Setenv("HS_STATE_DIR", t.TempDir())
	now := time.Now()
	f := &sys.Fake{Clock: now, Files: map[string]string{
		hestia.UsersDir + "/alavus/web.conf": "DOMAIN='alavusikkunat.fi' PROXY='wp-rocket' BACKEND='default' SSL='yes'\nDOMAIN='kehitys.alavusikkunat.fi' PROXY='default' SSL='no'\n",
	}}
	m := newModel(&check.Env{Sys: f}, "hzdemo", "test")
	mk := func(chk, sec, title, subj string, st check.State, sum string, data map[string]string) check.Result {
		return check.Result{ID: chk + ":" + subj, Check: chk, Key: chk, Section: sec, Title: title, Subject: subj,
			State: st, Summary: sum, CheckedAt: now, Data: data, Why: "because", Fix: "do the thing", Evidence: []string{"ev 1", "ev 2"}}
	}
	m.results = []check.Result{
		mk("site.core", "sites", "WordPress core", "alavusikkunat.fi", check.Fail, "2 core files added", map[string]string{"wp": "7.1.2"}),
		mk("site.http", "sites", "Site answers", "alavusikkunat.fi", check.OK, "HTTP 301", map[string]string{"http": "301", "sslDays": "60"}),
		mk("backups.nightly", "backups", "Nightly backup", "alavus", check.OK, "restic 6h ago", map[string]string{"ageHours": "6", "source": "restic"}),
		mk("mail.delivery", "mail", "Outbound mail", "", check.Fail, "postfix is not running", nil),
		mk("firewall", "security", "Firewall", "", check.OK, "198 rules", nil),
	}
	m.width, m.height = 120, 40
	return m
}

func press(m *model, keys ...string) {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		m.Update(msg)
	}
}

func TestScreensRender(t *testing.T) {
	m := testModel(t)
	for _, size := range [][2]int{{120, 40}, {60, 14}} {
		m.width, m.height = size[0], size[1]
		for _, keys := range [][]string{{"o"}, {"a"}, {"s"}, {"s", "enter"}, {"b"}, {"x"}, {"p"}, {"w"}, {"m"}, {"n"}, {"y"}, {"?"}} {
			press(m, "esc", "esc")
			press(m, keys...)
			out := m.View()
			if out == "" {
				t.Fatalf("%v at %v: empty view", keys, size)
			}
		}
	}
}

func TestOverviewShowsProblemsOnly(t *testing.T) {
	m := testModel(t)
	out := m.View()
	if !strings.Contains(out, "postfix is not running") || strings.Contains(out, "198 rules") {
		t.Errorf("overview should list problems only:\n%s", out)
	}
	press(m, "a")
	if !strings.Contains(m.View(), "198 rules") {
		t.Error("a should show all results")
	}
}

func TestSitesAndSiteScreen(t *testing.T) {
	m := testModel(t)
	press(m, "s")
	out := m.View()
	for _, want := range []string{"alavusikkunat.fi", "kehitys.alavusikkunat.fi", "7.1.2", "60d", "6h restic"} {
		if !strings.Contains(out, want) {
			t.Errorf("sites view missing %q:\n%s", want, out)
		}
	}
	// worst site first: alavusikkunat.fi has a Fail
	press(m, "enter")
	if m.scr != scrSite || m.siteName != "alavusikkunat.fi" {
		t.Fatalf("enter opened %q (scr %d)", m.siteName, m.scr)
	}
	out = m.View()
	if !strings.Contains(out, "2 core files added") || !strings.Contains(out, "restic 6h ago") || strings.Contains(out, "postfix") {
		t.Errorf("site screen should show its own results and its user's backup only:\n%s", out)
	}
	press(m, "esc")
	if m.scr != scrSites {
		t.Error("esc from a site should return to sites")
	}
}

func TestFilter(t *testing.T) {
	m := testModel(t)
	press(m, "s", "/", "k", "e", "h", "enter")
	if rows := m.siteRows(); len(rows) != 1 || rows[0].Name != "kehitys.alavusikkunat.fi" {
		t.Errorf("filter: %+v", rows)
	}
	press(m, "esc")
	if rows := m.siteRows(); len(rows) != 2 {
		t.Errorf("esc should clear the filter: %d rows", len(rows))
	}
}

func TestSitesCursorOpensTheSelectedRow(t *testing.T) {
	m := testModel(t)
	press(m, "s")
	m.View()
	press(m, "j")
	m.View()
	rows := m.siteRows()
	press(m, "enter")
	if m.siteName != rows[1].Name {
		t.Errorf("opened %q, want %q", m.siteName, rows[1].Name)
	}
}
