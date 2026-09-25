package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/termenv"

	"github.com/valolink/hestiascripts/internal/action"
	"github.com/valolink/hestiascripts/internal/actlog"
)

func ctrl(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

func TestCtrlHLCycleTabsAndWrap(t *testing.T) {
	m := testModel(t)
	m.Update(ctrl(tea.KeyCtrlL))
	if m.currentTab() != "s" {
		t.Fatalf("ctrl+l from overview → %s", m.currentTab())
	}
	m.Update(ctrl(tea.KeyCtrlH))
	m.Update(ctrl(tea.KeyCtrlH))
	if m.currentTab() != "v" {
		t.Errorf("ctrl+h should wrap to the last tab (Log), got %s", m.currentTab())
	}
}

func TestPanesAndVimMotions(t *testing.T) {
	m := testModel(t)
	press(m, "b", "L")
	if m.pane != paneActions {
		t.Fatal("L should switch to the Actions pane")
	}
	if len(m.actionRows()) == 0 {
		t.Fatal("Backups has actions")
	}
	press(m, "3", "j")
	if m.cur().cursor != 3 {
		t.Errorf("3j → cursor %d", m.cur().cursor)
	}
	press(m, "g", "g")
	if m.cur().cursor != 0 {
		t.Errorf("gg → %d", m.cur().cursor)
	}
	press(m, "2", "G")
	if m.cur().cursor != 1 {
		t.Errorf("2G → %d", m.cur().cursor)
	}
	press(m, "H")
	if m.pane != paneChecks {
		t.Error("H should return to Checks")
	}
}

func TestPaletteQuit(t *testing.T) {
	m := testModel(t)
	press(m, ":", "q")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal(":q should quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error(":q did not return tea.Quit")
	}
}

func TestTypedConfirmRefusesWrongName(t *testing.T) {
	m := testModel(t)
	a, _ := action.ByID("site.update")
	d, _ := m.domain("alavusikkunat.fi")
	m.ask(a, action.Target{Domain: &d})
	press(m, "a", "l", "a")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.confirming == nil || m.scr == scrRun {
		t.Fatal("a wrong name must not run")
	}
	press(m, "esc")
	if m.confirming != nil {
		t.Error("esc cancels")
	}
}

// End to end: a streamed action runs, streams, logs its transcript, and
// shows up in the Log tab.
func TestStreamedActionIsLiveAndLogged(t *testing.T) {
	m := testModel(t)
	t.Setenv("HS_LOG_DIR", t.TempDir())
	script := filepath.Join(t.TempDir(), "demo.sh")
	os.WriteFile(script, []byte("#!/bin/sh\necho first\necho second >&2\nexit 4\n"), 0o755)
	a := action.Action{ID: "test.demo", Title: "Demo", Mode: action.Stream, Confirm: action.ConfirmNone,
		Command: func(action.Target, string) []string { return []string{script} }}

	m.ask(a, action.Target{Host: "hzdemo"})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.scr != scrRun || cmd == nil {
		t.Fatal("enter should start the run")
	}
	deadline := time.After(5 * time.Second)
	for !m.run.done {
		select {
		case <-deadline:
			t.Fatalf("run did not finish; lines %v", m.run.lines)
		default:
		}
		msg := cmd()
		_, cmd = m.Update(msg)
		if cmd == nil && !m.run.done {
			t.Fatal("stream stalled")
		}
	}
	if *m.run.code != 4 || strings.Join(m.run.lines, "|") != "first|second" {
		t.Fatalf("code %d lines %q", *m.run.code, m.run.lines)
	}
	if !strings.Contains(m.View(), "exit 4") {
		t.Error("viewer should show the exit code")
	}
	runs, _ := actlog.Runs()
	if len(runs) != 1 || runs[0].Exit == nil || *runs[0].Exit != 4 || runs[0].Plan != script {
		t.Fatalf("log: %+v", runs)
	}
	tr, _ := os.ReadFile(runs[0].Log)
	if !strings.Contains(string(tr), "first\nsecond\n") || !strings.Contains(string(tr), "# exit 4") {
		t.Errorf("transcript:\n%s", tr)
	}
	press(m, "esc", "v")
	if !strings.Contains(m.View(), "Demo") {
		t.Error("Log tab should list the run")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.scr != scrRun || !strings.Contains(strings.Join(m.run.lines, "\n"), "second") {
		t.Error("enter on a logged run opens its transcript")
	}
}

// tmux delivered "3j" as one event on hzdemolink; the count was lost.
func TestMultiRuneKeyEventIsReplayed(t *testing.T) {
	m := testModel(t)
	press(m, "b", "L")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("3j")})
	if m.cur().cursor != 3 {
		t.Errorf("3j as one event → cursor %d", m.cur().cursor)
	}
}

func TestColorProfile(t *testing.T) {
	cases := []struct {
		term, ct, hs, no string
		want             termenv.Profile
	}{
		{"xterm-256color", "", "", "", termenv.TrueColor}, // wezterm over SSH
		{"screen", "", "", "", termenv.ANSI256},           // default tmux
		{"tmux-256color", "", "", "", termenv.TrueColor},
		{"xterm-256color", "", "256", "", termenv.ANSI256},
		{"linux", "truecolor", "", "", termenv.TrueColor},
		{"xterm-256color", "", "", "1", termenv.Ascii},
		{"dumb", "", "", "", termenv.Ascii},
	}
	for _, c := range cases {
		if got := colorProfile(c.term, c.ct, c.hs, c.no); got != c.want {
			t.Errorf("%+v → %v", c, got)
		}
	}
}
