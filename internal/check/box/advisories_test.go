package box

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/sys"
)

func TestAdvisoryAffects(t *testing.T) {
	a := Advisory{Patched: []string{"1.10.4", "1.9.10"}}
	for v, want := range map[string]bool{"1.9.9": true, "1.9.10": false, "1.10.3": true, "1.10.4": false, "1.8.12": true, "1.11.0": false} {
		if a.Affects(v) != want {
			t.Errorf("%s: %v", v, !want)
		}
	}
	b := Advisory{Patched: []string{"1.10.5"}}
	if !b.Affects("1.9.10") || b.Affects("1.10.5") {
		t.Error("an older branch without its own fix is affected; the fix itself is not")
	}
}

// alavus 2026-09-28: turned off, 1.0.2 still installed → it read "on".
func TestWebTerminalOffButVulnerableIsNotOn(t *testing.T) {
	f := &sys.Fake{Clock: time.Now(), Files: map[string]string{hestia.ConfPath: "WEB_TERMINAL='false'\n"}, Cmds: map[string]sys.FakeCmd{
		"dpkg-query -W -f=${Status} ${Version} hestia-web-terminal": {Out: "install ok installed 1.0.2"},
	}}
	rs := checkWebTerminal(context.Background(), &check.Env{Sys: f})
	if rs[0].State != check.Warn || !strings.Contains(rs[0].Summary, "vulnerable package") {
		t.Errorf("%s: %s", rs[0].State, rs[0].Summary)
	}
	f.Files[hestia.ConfPath] = "WEB_TERMINAL='true'\n"
	if rs := checkWebTerminal(context.Background(), &check.Env{Sys: f}); rs[0].State != check.Fail {
		t.Errorf("on: %s", rs[0].State)
	}
}
