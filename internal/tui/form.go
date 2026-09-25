package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/valolink/hestiascripts/internal/action"
	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/op"
	"github.com/valolink/hestiascripts/internal/plan"
)

// formState is an operation's input form: one field at a time focused,
// today's value shown beside each, the suggestion pre-filled.
type formState struct {
	op      op.Op
	target  op.Target
	values  op.Values
	current map[string]string
	choices map[string][][2]string
	cursor  int
	err     string
}

func (m *model) openForm(o op.Op, t op.Target) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	f := &formState{op: o, target: t, values: o.Defaults(ctx, m.env, t), current: map[string]string{}, choices: map[string][][2]string{}}
	for _, fl := range o.Fields {
		if fl.Current != nil {
			f.current[fl.Key] = fl.Current(ctx, m.env, t)
		}
		if fl.Kind == op.Choice && fl.Choices != nil {
			f.choices[fl.Key] = fl.Choices(ctx, m.env, t)
			if f.values[fl.Key] == "" && len(f.choices[fl.Key]) > 0 {
				f.values[fl.Key] = f.choices[fl.Key][0][0]
			}
		}
		if fl.Kind == op.Bool && f.values[fl.Key] == "" {
			f.values[fl.Key] = "no"
		}
	}
	if len(o.Fields) == 0 {
		m.submitForm(f)
		return
	}
	m.form = f
}

// opAction wraps a filled-in operation as an action: `hs op <id> k=v …`,
// streamed and logged, with the plan computed from these values for the
// plan screen.
func (m *model) opAction(o op.Op, t op.Target, v op.Values) (action.Action, action.Target) {
	exe, _ := os.Executable()
	argv := []string{exe, "op", o.ID}
	at := action.Target{Host: m.host}
	if t.Domain != nil {
		argv = append(argv, "--site", t.Domain.Name)
		at.Domain = t.Domain
	}
	argv = append(argv, v.Args(o)...)
	env := m.env
	mode := action.Stream
	if o.Interactive {
		mode = action.Interactive
	}
	a := action.Action{
		ID: "op." + o.ID, Title: o.Title, Site: o.Site, Mode: mode,
		Confirm: confirmFor(o.RiskFor(v)), Env: v.Env(o),
		Note:    o.Note, How: o.How, Undo: o.Undo, Recheck: o.Recheck,
		Command: func(action.Target, string) []string { return argv },
		Preview: func() ([]string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			steps, err := o.Plan(ctx, env, t, v)
			if err != nil {
				return nil, err
			}
			if len(steps) == 0 {
				return []string{"Nothing to do: the box is already in the state this would produce."}, nil
			}
			return plan.Preview(steps), nil
		},
	}
	return a, at
}

func confirmFor(r op.Risk) action.Confirm {
	return map[op.Risk]action.Confirm{op.ReadOnly: action.ConfirmNone, op.Change: action.ConfirmYes, op.Destructive: action.ConfirmTyped}[r]
}

func (m *model) submitForm(f *formState) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := f.op.Validate(ctx, m.env, f.target, f.values); err != nil {
		f.err = err.Error()
		m.form = f
		return
	}
	m.form = nil
	a, t := m.opAction(f.op, f.target, f.values)
	m.ask(a, t)
}

func (m *model) formKey(k tea.KeyMsg) tea.Cmd {
	f := m.form
	fl := f.op.Fields[f.cursor]
	s := k.String()
	f.err = ""
	switch s {
	case "esc", "ctrl+c":
		m.form = nil
		return nil
	case "tab", "down", "ctrl+j", "ctrl+n":
		f.cursor = (f.cursor + 1) % len(f.op.Fields)
		return nil
	case "shift+tab", "up", "ctrl+k", "ctrl+p":
		f.cursor = (f.cursor - 1 + len(f.op.Fields)) % len(f.op.Fields)
		return nil
	case "enter":
		if err := fl.Check(f.values[fl.Key]); err != nil {
			f.err = err.Error()
			return nil
		}
		if f.cursor < len(f.op.Fields)-1 {
			f.cursor++
			return nil
		}
		m.submitForm(f)
		return nil
	case "ctrl+s":
		m.submitForm(f)
		return nil
	}
	switch fl.Kind {
	case op.Choice:
		cs := f.choices[fl.Key]
		if len(cs) == 0 {
			return nil
		}
		i := 0
		for j, c := range cs {
			if c[0] == f.values[fl.Key] {
				i = j
			}
		}
		switch s {
		case "l", "right", " ":
			i = (i + 1) % len(cs)
		case "h", "left":
			i = (i - 1 + len(cs)) % len(cs)
		}
		f.values[fl.Key] = cs[i][0]
	case op.Bool:
		switch s {
		case "y":
			f.values[fl.Key] = "yes"
		case "n":
			f.values[fl.Key] = "no"
		case " ", "h", "l", "left", "right":
			f.values[fl.Key] = map[string]string{"yes": "no", "no": "yes"}[f.values[fl.Key]]
		}
	default:
		switch k.Type {
		case tea.KeyBackspace:
			if r := []rune(f.values[fl.Key]); len(r) > 0 {
				f.values[fl.Key] = string(r[:len(r)-1])
			}
		case tea.KeyCtrlU:
			f.values[fl.Key] = ""
		case tea.KeyRunes, tea.KeySpace:
			if fl.Kind == op.Number && strings.Trim(string(k.Runes), "0123456789") != "" {
				return nil
			}
			f.values[fl.Key] += string(k.Runes)
		}
	}
	return nil
}

func (m *model) formView() string {
	f := m.form
	w := min(m.width-4, 110)
	var b strings.Builder
	title := f.op.Title
	if f.target.Domain != nil {
		title += "  " + sAcc.Render(f.target.Domain.Name)
	}
	b.WriteString(sBold.Render(title) + "\n")
	if f.op.Note != "" {
		b.WriteString(wrap(f.op.Note, w-4) + "\n")
	}
	b.WriteString("\n")
	for i, fl := range f.op.Fields {
		val := f.values[fl.Key]
		switch fl.Kind {
		case op.Choice:
			label := val
			for _, c := range f.choices[fl.Key] {
				if c[0] == val && c[1] != "" {
					label = c[0] + " — " + c[1]
				}
			}
			val = "‹ " + label + " ›"
		case op.Bool:
			val = "[" + map[string]string{"yes": "x", "no": " "}[val] + "] " + val
		case op.Secret:
			val = strings.Repeat("•", len([]rune(val)))
		}
		line := fmt.Sprintf("%-26s %s", fl.Label, val)
		if i == f.cursor && fl.Kind != op.Choice && fl.Kind != op.Bool {
			line += "▏"
		}
		if cur := f.current[fl.Key]; cur != "" {
			line += sDim.Render("   now: " + cur)
		}
		if i == f.cursor {
			b.WriteString(sAcc.Render("▸ ") + sBold.Render(line) + "\n")
			if fl.Help != "" {
				b.WriteString(sDim.Render("    "+wrap(fl.Help, w-8)) + "\n")
			}
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	if f.err != "" {
		b.WriteString("\n" + stateStyle(check.Fail).Render(f.err) + "\n")
	}
	b.WriteString("\n" + sDim.Render("tab/↑↓ field · type to edit (ctrl+u clears) · h/l or space: choose/toggle · enter next → plan · esc cancel"))
	return sBox.Width(w).Render(b.String())
}

// opsHere lists the current tab's operations as actions (the form opens on
// enter; the action here only carries title, note and risk for the list).
func (m *model) opsHere() []action.Action {
	var ops []op.Op
	switch m.scr {
	case scrSection:
		ops = op.ForSection(m.sectionID)
	case scrSite:
		if d, ok := m.domain(m.siteName); ok {
			ops = op.ForSite(isWP(m.env, d))
		}
	}
	var out []action.Action
	for _, o := range ops {
		out = append(out, action.Action{
			ID: "op." + o.ID, Title: o.Title + "…", Site: o.Site, Mode: action.Stream, Note: o.Note, How: o.How, Undo: o.Undo,
			Confirm: confirmFor(o.Risk),
			Command: func(action.Target, string) []string { return []string{"hs", "op", o.ID} },
			Preview: func() ([]string, error) { return []string{"fill in the form first"}, nil },
		})
	}
	return out
}

// forCheck: the routine action or operation for a check that has no specific
// fix (action.ForCheck; an "op.<id>" value names an operation, whose form
// opens). Site-scoped ones target the result's site, else the open site.
func (m *model) forCheck(r check.Result) (string, func(), bool) {
	id, ok := action.ForCheck[r.Check]
	if !ok {
		return "", nil, false
	}
	site := func() (*hestia.Domain, bool) {
		d, ok := m.domain(r.Subject)
		if !ok {
			d, ok = m.domain(m.siteName)
		}
		return &d, ok
	}
	if oid, isOp := strings.CutPrefix(id, "op."); isOp {
		o, ok := op.ByID(oid)
		if !ok {
			return "", nil, false
		}
		var t op.Target
		if o.Site {
			d, ok := site()
			if !ok {
				return "", nil, false
			}
			t.Domain = d
		}
		return o.Title + "…", func() { m.openForm(o, t) }, true
	}
	a, ok := action.ByID(id)
	if !ok {
		return "", nil, false
	}
	t := action.Target{Host: m.host}
	if a.Site {
		d, ok := site()
		if !ok {
			return "", nil, false
		}
		t.Domain = d
	}
	return a.Title, func() { m.ask(a, t) }, true
}
