package check

import (
	"context"
	"testing"
	"time"

	"github.com/valolink/hestiascripts/internal/sys"
)

var t0 = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func TestRunnerTimeoutAndPanicAreUnknown(t *testing.T) {
	env := &Env{Sys: &sys.Fake{Clock: t0}}
	checks := []Check{
		{ID: "slow", Section: "system", Title: "Slow", Timeout: 20 * time.Millisecond, Run: func(ctx context.Context, _ *Env) []Result {
			<-time.After(time.Second)
			return []Result{New(OK, "never")}
		}},
		{ID: "boom", Section: "system", Title: "Boom", Run: func(context.Context, *Env) []Result { panic("kaput") }},
		{ID: "fine", Section: "web", Title: "Fine", Run: func(context.Context, *Env) []Result {
			return []Result{New(OK, "yes").For("a.fi")}
		}},
	}
	rs := RunAll(context.Background(), env, checks)
	got := map[string]State{}
	for _, r := range rs {
		got[r.ID] = r.State
		if r.CheckedAt != t0 || r.Section == "" || r.Title == "" {
			t.Errorf("%s not stamped: %+v", r.ID, r)
		}
	}
	if got["slow"] != Unknown || got["boom"] != Unknown || got["fine:a.fi"] != OK {
		t.Fatalf("states = %v", got)
	}
	if rs[0].State != Unknown || rs[len(rs)-1].State != OK {
		t.Errorf("not sorted worst first: %v", rs)
	}
}

func TestStaleOKDegradesToConfigured(t *testing.T) {
	r := New(OK, "x").Valid(time.Hour)
	r.CheckedAt = t0
	if r.Effective(t0.Add(30*time.Minute)) != OK {
		t.Error("fresh OK should stay OK")
	}
	if r.Effective(t0.Add(2*time.Hour)) != Configured {
		t.Error("stale OK should read Configured")
	}
}

func TestExitCode(t *testing.T) {
	mk := func(ss ...State) []Result {
		var rs []Result
		for _, s := range ss {
			rs = append(rs, Result{State: s, CheckedAt: t0})
		}
		return rs
	}
	cases := []struct {
		rs   []Result
		want int
	}{
		{mk(OK, Configured), 0},
		{mk(OK, Unknown), 1}, // unknown is never green
		{mk(Warn, OK), 1},
		{mk(Warn, Fail), 2},
	}
	for i, c := range cases {
		if got := ExitCode(c.rs, t0); got != c.want {
			t.Errorf("case %d: exit %d, want %d", i, got, c.want)
		}
	}
}
