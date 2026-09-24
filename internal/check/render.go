package check

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

type Style struct {
	Color bool
	All   bool // also list OK and Configured results
}

const (
	cBold   = "\033[1m"
	cDim    = "\033[2m"
	cRed    = "\033[0;31m"
	cYellow = "\033[1;33m"
	cGreen  = "\033[0;32m"
	cCyan   = "\033[0;36m"
	cGrey   = "\033[0;90m"
	cReset  = "\033[0m"
)

func (st Style) c(code, s string) string {
	if !st.Color {
		return s
	}
	return code + s + cReset
}

var groupHeadings = []struct {
	state   State
	heading string
	color   string
}{
	{Fail, "FIX NOW", cRed},
	{Warn, "SHOULD FIX", cYellow},
	{Unknown, "COULD NOT CHECK", cGrey},
	{Configured, "CONFIGURED, NOT PROVEN WORKING", cDim},
	{OK, "WORKING", cGreen},
}

// Text renders the full report: everything that is not OK, grouped worst
// first, each with its evidence, why it matters and the fix.
func Text(w io.Writer, host string, rs []Result, now time.Time, st Style) {
	line := strings.Repeat("─", 64)
	fmt.Fprintf(w, "\n  %s\n", st.c(cBold, "hs check — "+host))
	fmt.Fprintf(w, "  %s\n", st.c(cDim, now.Format("2006-01-02 15:04 MST")+" · read-only, nothing was changed"))
	fmt.Fprintf(w, "  %s\n", line)

	for _, g := range groupHeadings {
		if !st.All && (g.state == OK || g.state == Configured) {
			continue
		}
		var group []Result
		for _, r := range rs {
			if r.Effective(now) == g.state {
				group = append(group, r)
			}
		}
		if len(group) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n  %s\n", st.c(g.color+cBold, g.heading))
		for _, r := range group {
			fmt.Fprintf(w, "\n  %s %s\n", st.c(g.color, "●"), st.c(cBold, headline(r)))
			for _, e := range r.Evidence {
				fmt.Fprintf(w, "    %s\n", st.c(cDim, "· "+e))
			}
			if r.State == OK && g.state == Configured {
				fmt.Fprintf(w, "    %s\n", st.c(cDim, "last proven working "+ago(now.Sub(r.CheckedAt))+" ago"))
			}
			if r.Why != "" && g.state != OK {
				fmt.Fprintf(w, "    %s\n", st.c(cDim, r.Why))
			}
			if r.Fix != "" && g.state != OK {
				fmt.Fprintf(w, "    %s %s\n", st.c(cCyan, "→"), r.Fix)
			}
		}
	}

	c := Count(rs, now)
	fmt.Fprintf(w, "\n  %s\n  %s\n\n", line, CountsLine(c, st))
	if !st.All && c[OK]+c[Configured] > 0 {
		fmt.Fprintf(w, "  %s\n\n", st.c(cDim, "hs check --all also lists what is working and what is only configured."))
	}
}

func CountsLine(c Counts, st Style) string {
	return strings.Join([]string{
		st.c(cRed, fmt.Sprintf("%d fail", c[Fail])),
		st.c(cYellow, fmt.Sprintf("%d warn", c[Warn])),
		st.c(cGrey, fmt.Sprintf("%d unknown", c[Unknown])),
		st.c(cDim, fmt.Sprintf("%d configured", c[Configured])),
		st.c(cGreen, fmt.Sprintf("%d ok", c[OK])),
	}, " · ")
}

// Brief is one summary line per box plus its Fail headlines — the fleet
// sweep format of v-server-audit --brief.
func Brief(w io.Writer, host string, rs []Result, now time.Time) {
	c := Count(rs, now)
	fmt.Fprintf(w, "%-24s %d fail, %d warn, %d unknown, %d configured, %d ok\n", host, c[Fail], c[Warn], c[Unknown], c[Configured], c[OK])
	for _, r := range rs {
		if r.Effective(now) == Fail {
			fmt.Fprintf(w, "  ! %s\n", headline(r))
		}
	}
}

type Report struct {
	Host    string         `json:"host"`
	At      time.Time      `json:"at"`
	Counts  map[string]int `json:"counts"`
	Results []Result       `json:"results"`
}

func JSON(w io.Writer, host string, rs []Result, now time.Time) error {
	counts := map[string]int{}
	for s, n := range Count(rs, now) {
		counts[s.String()] = n
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(Report{Host: host, At: now, Counts: counts, Results: rs})
}

func headline(r Result) string {
	if r.Subject != "" && !strings.Contains(r.Summary, r.Subject) {
		return r.Title + " — " + r.Subject + ": " + r.Summary
	}
	return r.Title + ": " + r.Summary
}

func ago(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
