// Package check is hs's single check engine. The TUI, the CLI, EngineLink
// (through internal/compat) and cron all read the results it produces.
//
// The rule that shapes everything here: green (OK) means a probe saw the
// thing *working*. A check that can only prove something is installed or
// configured tops out at Configured, and a check that could not run is
// Unknown — never OK.
package check

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/valolink/hestiascripts/internal/sys"
)

type State int

// Ordered best → worst so a roll-up is max().
const (
	NA State = iota
	OK
	Configured
	Unknown
	Warn
	Fail
)

var stateNames = map[State]string{NA: "na", OK: "ok", Configured: "configured", Unknown: "unknown", Warn: "warn", Fail: "fail"}

func (s State) String() string { return stateNames[s] }

func (s State) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

func (s *State) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	for k, n := range stateNames {
		if n == v {
			*s = k
			return nil
		}
	}
	return fmt.Errorf("unknown state %q", v)
}

// Result is one verdict. A check may return several — one per user, domain
// or sub-question — so a box with one stale backup names that user instead of
// hiding it inside a roll-up.
type Result struct {
	ID        string        `json:"id"`
	Section   string        `json:"section"`
	Title     string        `json:"title"`
	Subject   string        `json:"subject,omitempty"`
	State     State         `json:"state"`
	Summary   string        `json:"summary"`
	Evidence  []string      `json:"evidence,omitempty"`
	Why       string        `json:"why,omitempty"`
	Fix       string        `json:"fix,omitempty"`
	CheckedAt time.Time     `json:"checkedAt"`
	TTL       time.Duration `json:"ttl,omitempty"`
}

// Effective degrades an OK whose evidence has gone stale to Configured: it was
// proven working once, and nothing has proven it since.
func (r Result) Effective(now time.Time) State {
	if r.State == OK && r.TTL > 0 && now.Sub(r.CheckedAt) > r.TTL {
		return Configured
	}
	return r.State
}

// Builder helpers keep check bodies readable.
func New(state State, summary string) Result { return Result{State: state, Summary: summary} }

func (r Result) For(subject string) Result      { r.Subject = subject; return r }
func (r Result) Because(why string) Result      { r.Why = why; return r }
func (r Result) Fixed(fix string) Result        { r.Fix = fix; return r }
func (r Result) Valid(ttl time.Duration) Result { r.TTL = ttl; return r }
func (r Result) Ev(lines ...string) Result {
	r.Evidence = append(append([]string{}, r.Evidence...), lines...)
	return r
}

// Env is what a check can reach.
type Env struct {
	Sys     sys.Sys
	RepoDir string // hestiascripts checkout, for comparing deployed files with the repo
}

type Check struct {
	ID      string
	Section string
	Title   string
	Timeout time.Duration // default 10s
	Run     func(ctx context.Context, env *Env) []Result
}

const DefaultTimeout = 10 * time.Second

// Sections in display order.
var Sections = []string{"security", "backups", "system", "performance", "web", "mail", "monitoring"}

// RunAll runs checks in parallel, each under its own deadline. A check that
// times out or panics yields one Unknown result — a wedged daemon costs one
// line, never the whole run.
func RunAll(ctx context.Context, env *Env, checks []Check) []Result {
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out []Result
	)
	for _, c := range checks {
		wg.Add(1)
		go func(c Check) {
			defer wg.Done()
			res := runOne(ctx, env, c)
			mu.Lock()
			out = append(out, res...)
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	Sort(out)
	return out
}

func runOne(parent context.Context, env *Env, c Check) (res []Result) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	now := env.Sys.Now()
	stamp := func(rs []Result) []Result {
		for i := range rs {
			r := &rs[i]
			r.Section, r.Title, r.CheckedAt = c.Section, c.Title, now
			r.ID = c.ID
			if r.Subject != "" {
				r.ID = c.ID + ":" + r.Subject
			}
		}
		return rs
	}

	done := make(chan []Result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- []Result{New(Unknown, fmt.Sprintf("check crashed: %v", p))}
			}
		}()
		done <- c.Run(ctx, env)
	}()

	select {
	case rs := <-done:
		if ctx.Err() == context.DeadlineExceeded {
			// Finished, but only after something inside it hit the deadline —
			// its verdict may rest on missing data.
			rs = append(rs, New(Unknown, fmt.Sprintf("part of this check timed out after %s", timeout)))
		}
		return stamp(rs)
	case <-ctx.Done():
		return stamp([]Result{New(Unknown, fmt.Sprintf("timed out after %s", timeout)).
			Because("A probe did not answer in time, so nothing can be said about this area.")})
	}
}

// Sort orders worst first, then by section order, then ID.
func Sort(rs []Result) {
	secIdx := map[string]int{}
	for i, s := range Sections {
		secIdx[s] = i
	}
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].State != rs[j].State {
			return rs[i].State > rs[j].State
		}
		if rs[i].Section != rs[j].Section {
			return secIdx[rs[i].Section] < secIdx[rs[j].Section]
		}
		return rs[i].ID < rs[j].ID
	})
}

type Counts map[State]int

func Count(rs []Result, now time.Time) Counts {
	c := Counts{}
	for _, r := range rs {
		c[r.Effective(now)]++
	}
	return c
}

// ExitCode follows v-server-audit: 2 any Fail, 1 any Warn or Unknown, 0 clean.
func ExitCode(rs []Result, now time.Time) int {
	c := Count(rs, now)
	switch {
	case c[Fail] > 0:
		return 2
	case c[Warn] > 0 || c[Unknown] > 0:
		return 1
	}
	return 0
}
