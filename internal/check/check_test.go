package check

import (
	"context"
	"sync"
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

func TestRunCachedReusesWithinMinInterval(t *testing.T) {
	calls := 0
	c := Check{ID: "restic", Section: "backups", Title: "R", MinInterval: 6 * time.Hour,
		Run: func(context.Context, *Env) []Result { calls++; return []Result{New(OK, "fresh").For("alavus")} }}
	f := &sys.Fake{Clock: t0}
	env := &Env{Sys: f}
	first := RunCached(context.Background(), env, []Check{c}, nil, false, nil)
	f.Clock = t0.Add(time.Hour)
	second := RunCached(context.Background(), env, []Check{c}, first, false, nil)
	if calls != 1 || second[0].CheckedAt != t0 {
		t.Fatalf("within interval: calls=%d at=%v", calls, second[0].CheckedAt)
	}
	RunCached(context.Background(), env, []Check{c}, first, true, nil)
	if calls != 2 {
		t.Errorf("force should probe: calls=%d", calls)
	}
	f.Clock = t0.Add(7 * time.Hour)
	RunCached(context.Background(), env, []Check{c}, first, false, nil)
	if calls != 3 {
		t.Errorf("after interval should probe: calls=%d", calls)
	}
}

func TestHeavyChecksAreLimited(t *testing.T) {
	var cur, peak int32
	var mu sync.Mutex
	var cs []Check
	for i := 0; i < 10; i++ {
		cs = append(cs, Check{ID: "wp", Subject: string(rune('a' + i)), Heavy: true, Run: func(context.Context, *Env) []Result {
			mu.Lock()
			cur++
			if cur > peak {
				peak = cur
			}
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			cur--
			mu.Unlock()
			return []Result{New(OK, "x")}
		}})
	}
	rs := RunAll(context.Background(), &Env{Sys: &sys.Fake{Clock: t0}}, cs)
	if len(rs) != 10 || peak > HeavyLimit {
		t.Errorf("results %d, peak concurrency %d", len(rs), peak)
	}
}
