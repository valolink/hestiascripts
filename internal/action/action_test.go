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
		desc := Describe(a, tg, repo)
		stock := strings.Contains(a.Plan(tg, repo), "v-add-letsencrypt") || strings.Contains(a.Plan(tg, repo), "v-purge-nginx") ||
			strings.Contains(a.Plan(tg, repo), "v-update-sys-hestia") || strings.HasPrefix(a.Plan(tg, repo), "mailq")
		if len(desc) == 0 && !stock {
			t.Errorf("%s: no description from source", a.ID)
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
