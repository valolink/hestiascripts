// Package plan is what fixes and operations produce: plain commands, each
// with the reason it is there. One executor prints and runs them for both, so
// `hs fix`, `hs op` and every preview behave the same.
package plan

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Step is one command and why it is in the plan.
type Step struct {
	Why  string
	Argv []string
}

// Quote renders argv as a copy-pasteable shell line.
func Quote(argv []string) string {
	var parts []string
	for _, a := range argv {
		if a != "" && !strings.ContainsAny(a, " \t\n'\"$`\\;&|<>*?()[]{}!#~") {
			parts = append(parts, a)
		} else {
			parts = append(parts, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
		}
	}
	return strings.Join(parts, " ")
}

// Preview renders the steps as the plan screen and --dry-run show them.
func Preview(steps []Step) []string {
	var out []string
	for _, s := range steps {
		for _, w := range strings.Split(s.Why, "\n") {
			if w != "" {
				out = append(out, "# "+w)
			}
		}
		out = append(out, "$ "+Quote(s.Argv))
	}
	return out
}

// Execute prints each step before running it. A read-only plan (a
// diagnosis) runs every step and reports exit codes as information —
// `systemctl status` exits 3 for any stopped unit. Anything else stops at the
// first failing step. Returns the exit code for the whole plan.
func Execute(ctx context.Context, w io.Writer, title string, steps []Step, readOnly, dry bool) int {
	fmt.Fprintf(w, "# %s\n", title)
	if len(steps) == 0 {
		fmt.Fprintln(w, "# nothing to do: the box is already in the state this would produce")
		return 0
	}
	for i, st := range steps {
		for j, l := range strings.Split(st.Why, "\n") {
			switch {
			case l == "" && j == 0:
				fmt.Fprintln(w)
			case j == 0:
				fmt.Fprintf(w, "\n# %d/%d %s\n", i+1, len(steps), l)
			default:
				fmt.Fprintf(w, "#     %s\n", l)
			}
		}
		fmt.Fprintln(w, "$ "+Quote(st.Argv))
		if dry {
			continue
		}
		cmd := exec.CommandContext(ctx, st.Argv[0], st.Argv[1:]...)
		cmd.Stdout, cmd.Stderr = w, w
		if err := cmd.Run(); err != nil {
			if readOnly {
				fmt.Fprintf(w, "# (%v)\n", err)
				continue
			}
			fmt.Fprintf(w, "! %v\n\n# stopped after step %d of %d; nothing after it ran\n", err, i+1, len(steps))
			return 1
		}
	}
	if dry {
		fmt.Fprintf(w, "\n# dry run: %d step(s), nothing ran\n", len(steps))
		return 0
	}
	fmt.Fprintf(w, "\n# done: %d step(s)\n", len(steps))
	return 0
}
