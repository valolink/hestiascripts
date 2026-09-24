package tui

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/valolink/hestiascripts/internal/action"
	"github.com/valolink/hestiascripts/internal/actlog"
	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/logsrc"
)

// run is the viewer: a live streamed action, a stored transcript, or a
// server log. One screen, one set of keys.
type run struct {
	title, sub string
	lines      []string
	live       bool // a process is attached
	done       bool
	code       *int
	started    time.Time
	ended      time.Time
	follow     bool
	offset     int
	filter     string
	filtering  bool
	stopAsk    bool
	back       screen

	// live action
	act    action.Action
	target action.Target
	cmd    *exec.Cmd
	lineCh chan tea.Msg
	logEv  actlog.Event
	logF   *os.File

	// server log: re-read for follow mode
	src *logsrc.Source
}

type runLineMsg struct{ line string }
type runDoneMsg struct{ code int }
type execDoneMsg struct {
	act    action.Action
	target action.Target
	ev     actlog.Event
	err    error
}
type logTickMsg struct{}

const tailLines = 5000

func modeName(a action.Action) string {
	if a.Mode == action.Interactive {
		return "interactive"
	}
	return "stream"
}

// start launches an action — logged first, then streamed inside the TUI, or
// with the terminal handed over (recorded with script(1)) for interactive ones.
func (m *model) start(a action.Action, t action.Target) tea.Cmd {
	repo := m.env.RepoDir
	plan := a.Plan(t, repo)
	ev, lerr := actlog.Start(a.ID, a.Title, t.Name(), a.Exact(t, repo), modeName(a))
	if lerr != nil {
		m.setStatus("action log unavailable: " + lerr.Error())
	}

	if a.Mode == action.Interactive {
		argv := a.Command(t, repo)
		var cmd *exec.Cmd
		if lerr == nil && haveScript() {
			// script(1) records what the terminal shows, prompts included;
			// input typed with echo off (passwords) never reaches the file.
			// Pre-create the transcript 0600 and let script append (-a):
			// script itself would create it world-readable.
			if f, err := actlog.Transcript(ev); err == nil {
				f.Close()
			}
			cmd = exec.Command("script", "-q", "-a", "-e", "-f", "-c", shellJoin(argv), ev.Log)
		} else {
			cmd = exec.Command(argv[0], argv[1:]...)
		}
		cmd.Env = append(os.Environ(), "SCRIPT_DIR="+repo)
		m.setStatus("running " + a.Title + " …")
		return tea.ExecProcess(cmd, func(err error) tea.Msg { return execDoneMsg{a, t, ev, err} })
	}

	cmd := a.Cmd(t, repo)
	cmd.Env = append(os.Environ(), "SCRIPT_DIR="+repo, "TERM=dumb", "NO_COLOR=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // stop = the whole group
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	r := &run{title: a.Title, sub: "$ " + plan, act: a, target: t, cmd: cmd, live: true, follow: true,
		started: time.Now(), lineCh: make(chan tea.Msg, 256), back: m.scr, logEv: ev}
	if lerr == nil {
		if f, err := actlog.Transcript(ev); err == nil {
			r.logF = f
			fmt.Fprintf(f, "# %s — %s\n# $ %s\n# started %s by %s\n\n", a.Title, t.Name(), plan, ev.At.Format(time.RFC3339), ev.Operator)
		}
	}
	m.run = r
	m.scr = scrRun
	if err := cmd.Start(); err != nil {
		r.lines = append(r.lines, "could not start: "+err.Error())
		code := -1
		r.done, r.code, r.ended = true, &code, time.Now()
		m.finishLog(r, -1, err.Error())
		return nil
	}
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			line := stripCR(sc.Text())
			if r.logF != nil {
				r.logF.WriteString(line + "\n")
			}
			r.lineCh <- runLineMsg{line}
		}
		io.Copy(io.Discard, pr)
	}()
	go func() {
		err := cmd.Wait()
		pw.Close()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			code = -1
		}
		time.Sleep(50 * time.Millisecond) // let the reader drain first
		r.lineCh <- runDoneMsg{code}
	}()
	return m.waitRun()
}

func (m *model) finishLog(r *run, code int, errText string) {
	if r.logF != nil {
		fmt.Fprintf(r.logF, "\n# exit %d after %s\n", code, time.Since(r.started).Round(time.Second))
		r.logF.Close()
		r.logF = nil
	}
	if r.logEv.ID != "" {
		actlog.End(r.logEv, code, errText)
	}
}

func haveScript() bool {
	_, err := exec.LookPath("script")
	return err == nil
}

func shellJoin(argv []string) string {
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

// stripCR keeps the last carriage-return segment of a progress line and
// drops ANSI codes, which the viewer renders on its own.
func stripCR(s string) string {
	if i := strings.LastIndex(s, "\r"); i >= 0 && i < len(s)-1 {
		s = s[i+1:]
	}
	return ansiStrip(strings.TrimRight(s, "\r"))
}

func (m *model) waitRun() tea.Cmd {
	r := m.run
	return func() tea.Msg { return <-r.lineCh }
}

func (m *model) runUpdate(msg tea.Msg) tea.Cmd {
	r := m.run
	if r == nil {
		return nil
	}
	switch msg := msg.(type) {
	case runLineMsg:
		r.lines = append(r.lines, msg.line)
		return m.waitRun()
	case runDoneMsg:
		code := msg.code
		r.done, r.code, r.ended = true, &code, time.Now()
		m.finishLog(r, code, "")
		if only := m.recheckChecks(r.act, r.target); len(only) > 0 {
			m.setStatus(fmt.Sprintf("%s — exit %d; re-checking %d affected checks", r.act.Title, code, len(only)))
			m.rememberBefore(r, only)
			r.lines = append(r.lines, "", fmt.Sprintf("── exit %d · re-checking %d affected checks … ──", code, len(only)))
			return m.startRefresh(true, only)
		}
		m.setStatus(fmt.Sprintf("%s — exit %d", r.act.Title, code))
	}
	return nil
}

func (m *model) execDone(msg execDoneMsg) tea.Cmd {
	code, res := 0, "finished"
	if msg.err != nil {
		code, res = -1, "ended: "+msg.err.Error()
		if ee, ok := msg.err.(*exec.ExitError); ok {
			code, res = ee.ExitCode(), fmt.Sprintf("exit %d", ee.ExitCode())
		}
	}
	if msg.ev.ID != "" {
		actlog.End(msg.ev, code, "")
	}
	if strings.HasSuffix(msg.act.ID, ".shell") {
		// a shell's exit code is just the last command typed in it
		m.setStatus("shell closed — session saved to the Log tab")
		return nil
	}
	if only := m.recheckChecks(msg.act, msg.target); len(only) > 0 {
		m.setStatus(fmt.Sprintf("%s %s; re-checking %d affected checks", msg.act.Title, res, len(only)))
		return m.startRefresh(true, only)
	}
	m.setStatus(msg.act.Title + " " + res)
	return nil
}

// openTranscript shows a logged run's transcript.
func (m *model) openTranscript(lr actlog.Run) {
	lines, err := logsrc.Tail(context.Background(), m.env.Sys, logsrc.Source{Path: lr.Log}, tailLines)
	if err != nil {
		lines = []string{"transcript unavailable: " + err.Error()}
	}
	for i := range lines {
		lines[i] = stripCR(lines[i])
	}
	r := &run{title: lr.Title + " — " + lr.Target, sub: "$ " + lr.Plan, lines: lines, done: true,
		started: lr.At, ended: lr.Ended, code: lr.Exit, back: m.scr}
	m.run, m.scr = r, scrRun
}

// openLog shows a server log's tail; G follows it.
func (m *model) openLog(src logsrc.Source) tea.Cmd {
	lines, err := logsrc.Tail(context.Background(), m.env.Sys, src, tailLines)
	if err != nil {
		lines = []string{"cannot read: " + err.Error()}
	}
	s := src
	m.run = &run{title: src.Group + " · " + src.Name, sub: src.Label(), lines: lines, done: true, follow: true,
		back: m.scr, src: &s}
	m.scr = scrRun
	return logTick()
}

func logTick() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return logTickMsg{} })
}

// reloadLog re-reads a followed server log.
func (m *model) reloadLog() tea.Cmd {
	r := m.run
	if m.scr != scrRun || r == nil || r.src == nil {
		return nil
	}
	if r.follow {
		if lines, err := logsrc.Tail(context.Background(), m.env.Sys, *r.src, tailLines); err == nil {
			r.lines = lines
		}
	}
	return logTick()
}

// visible applies the viewer's / filter.
func (r *run) visible() []string {
	if r.filter == "" {
		return r.lines
	}
	f := strings.ToLower(r.filter)
	var out []string
	for _, l := range r.lines {
		if strings.Contains(strings.ToLower(l), f) {
			out = append(out, l)
		}
	}
	return out
}

func (m *model) runKey(k tea.KeyMsg) tea.Cmd {
	r := m.run
	s := k.String()
	h := m.height - 7
	if r.filtering {
		switch k.Type {
		case tea.KeyEnter:
			r.filtering = false
		case tea.KeyEsc:
			r.filtering, r.filter = false, ""
		case tea.KeyBackspace:
			if rs := []rune(r.filter); len(rs) > 0 {
				r.filter = string(rs[:len(rs)-1])
			}
		case tea.KeyRunes, tea.KeySpace:
			r.filter += string(k.Runes)
		}
		r.offset, r.follow = 0, true
		return nil
	}
	switch s {
	case "ctrl+c":
		if !r.live || r.done {
			return tea.Quit
		}
		if r.stopAsk {
			syscall.Kill(-r.cmd.Process.Pid, syscall.SIGTERM)
			m.setStatus("sent SIGTERM to " + r.title)
			r.stopAsk = false
		} else {
			r.stopAsk = true
			m.setStatus("ctrl+c again stops it — scripts clean up on TERM, but a half-done update may need a re-run")
		}
		return nil
	case "esc", "q", "h":
		if s == "esc" && r.filter != "" {
			r.filter = ""
			return nil
		}
		if r.live && !r.done {
			m.setStatus("still running — wait for it, or ctrl+c twice to stop")
			return nil
		}
		m.scr = r.back
		return nil
	case "/":
		r.filtering = true
		return nil
	case "j", "down", "ctrl+e":
		r.follow = false
		r.offset++
	case "k", "up", "ctrl+y":
		r.follow = false
		r.offset = max(r.offset-1, 0)
	case "ctrl+d":
		r.follow = false
		r.offset += h / 2
	case "ctrl+u":
		r.follow = false
		r.offset = max(r.offset-h/2, 0)
	case "ctrl+f", "pgdown":
		r.follow = false
		r.offset += h
	case "ctrl+b", "pgup":
		r.follow = false
		r.offset = max(r.offset-h, 0)
	case "g":
		if m.pendingG {
			r.follow, r.offset, m.pendingG = false, 0, false
		} else {
			m.pendingG = true
			return nil
		}
	case "G", "F":
		r.follow = true
		if r.src != nil {
			return m.reloadLog()
		}
	}
	m.pendingG = false
	return nil
}

// rememberBefore snapshots the results the re-check will replace.
func (m *model) rememberBefore(r *run, only []check.Check) {
	keys := map[string]bool{}
	for _, c := range only {
		keys[c.Key()] = true
	}
	m.recheckRun, m.recheckBefore = r, map[string]check.Result{}
	for _, res := range m.results {
		if keys[res.Key] {
			m.recheckBefore[res.ID] = res
		}
	}
	// keys with no prior result still count as "new"
	for k := range keys {
		m.recheckBefore["key:"+k] = check.Result{Key: k}
	}
}
