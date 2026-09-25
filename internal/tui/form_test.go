package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/op"
	"github.com/valolink/hestiascripts/internal/plan"
)

func init() {
	op.Register(op.Op{
		ID: "test.cap", Title: "Set the test cap", Section: "system", Risk: op.Change,
		Note: "test operation",
		Fields: []op.Field{
			{Key: "size", Label: "Cap (MB)", Kind: op.Number, Min: 64, Max: 4096,
				Current: func(context.Context, *check.Env, op.Target) string { return "512M" },
				Default: func(context.Context, *check.Env, op.Target) string { return "768" }},
			{Key: "policy", Label: "Policy", Kind: op.Choice,
				Choices: func(context.Context, *check.Env, op.Target) [][2]string {
					return [][2]string{{"allkeys-lru", "evict least recently used"}, {"noeviction", ""}}
				}},
		},
		Plan: func(_ context.Context, _ *check.Env, _ op.Target, v op.Values) ([]plan.Step, error) {
			return []plan.Step{{Why: "apply", Argv: []string{"redis-cli", "config", "set", "maxmemory", v["size"] + "mb"}}}, nil
		},
	})
}

func TestFormFlow(t *testing.T) {
	m := testModel(t)
	o, _ := op.ByID("test.cap")
	m.openForm(o, op.Target{})
	if m.form == nil || m.form.values["size"] != "768" || m.form.values["policy"] != "allkeys-lru" {
		t.Fatalf("defaults: %+v", m.form)
	}
	if v := m.View(); !strings.Contains(v, "now: 512M") || !strings.Contains(v, "Cap (MB)") {
		t.Errorf("form view:\n%s", v)
	}
	// invalid number is refused on enter
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	press(m, "9")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.form == nil || !strings.Contains(m.form.err, "64–4096") {
		t.Fatalf("validation: %+v", m.form)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	press(m, "1", "0", "2", "4")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // → policy
	press(m, "l")                            // cycle to noeviction
	m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // → plan screen
	if m.form != nil || m.confirming == nil {
		t.Fatalf("should reach the plan screen: form=%v confirming=%v", m.form, m.confirming)
	}
	if !strings.Contains(strings.Join(m.confirming.preview, "\n"), "redis-cli config set maxmemory 1024mb") {
		t.Errorf("preview: %v", m.confirming.preview)
	}
	argv := m.confirming.act.Command(m.confirming.target, "")
	if !strings.Contains(strings.Join(argv, " "), "op test.cap size=1024 policy=noeviction") {
		t.Errorf("runs as hs op with the values: %v", argv)
	}
}
