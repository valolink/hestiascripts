package box

import "testing"

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
