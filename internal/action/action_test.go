package action

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valolink/hestiascripts/internal/hestia"
)

func repoRoot(t *testing.T) string {
	wd, _ := os.Getwd()
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

func TestEveryActionBuildsAndDescribes(t *testing.T) {
	repo := repoRoot(t)
	d := hestia.Domain{User: "alavus", Name: "alavusikkunat.fi"}
	seen := map[string]bool{}
	for _, a := range All() {
		if seen[a.ID] {
			t.Errorf("duplicate id %s", a.ID)
		}
		seen[a.ID] = true
		tg := Target{Host: "box"}
		if a.Site {
			tg.Domain = &d
		}
		if plan := a.Plan(tg, repo); plan == "" {
			t.Errorf("%s: empty plan", a.ID)
		}
		// Commands whose source is in the repo must describe themselves from
		// it; stock Hestia binaries and inline commands carry a Note instead.
		desc := Describe(a, tg, repo)
		if len(desc) == 0 && a.Note == "" {
			t.Errorf("%s: neither a source description nor a note", a.ID)
		}
		argv := a.Command(tg, repo)
		if name := strings.TrimPrefix(argv[0], bin); name != argv[0] {
			if _, err := os.Stat(filepath.Join(repo, name+".sh")); err == nil && len(desc) == 0 {
				t.Errorf("%s: repo script %s has no header or usage text", a.ID, name)
			}
		}
	}
	for chk, id := range ForCheck {
		if _, ok := ByID(id); !ok {
			t.Errorf("ForCheck[%s] → unknown action %s", chk, id)
		}
	}
}

func TestSiteArgsAreNotShellParsed(t *testing.T) {
	d := hestia.Domain{User: "u", Name: "x.fi; rm -rf /"}
	a, _ := ByID("site.info")
	argv := a.Command(Target{Domain: &d}, "")
	if argv[2] != "--domain=x.fi; rm -rf /" {
		t.Errorf("domain must stay one argv element: %q", argv)
	}
	if !strings.Contains(a.Plan(Target{Domain: &d}, ""), "'--domain=x.fi; rm -rf /'") {
		t.Errorf("plan must show it quoted: %s", a.Plan(Target{Domain: &d}, ""))
	}
}
