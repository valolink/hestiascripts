package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/valolink/hestiascripts/internal/action"
	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/check/box"
	"github.com/valolink/hestiascripts/internal/check/site"
)

func (m *model) key(k tea.KeyMsg) tea.Cmd {
	// Fast typing, tmux and pastes can deliver "3j" as one event. Outside
	// text entry, replay it one key at a time so counts and gg work.
	if k.Type == tea.KeyRunes && len(k.Runes) > 1 && !k.Paste && m.palette == nil && !m.filtering &&
		(m.confirming == nil || m.confirming.act.Confirm != action.ConfirmTyped) &&
		!(m.scr == scrRun && m.run != nil && m.run.filtering) {
		var cmds []tea.Cmd
		for _, r := range k.Runes {
			cmds = append(cmds, m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}))
		}
		return tea.Batch(cmds...)
	}
	s := k.String()
	switch {
	case m.palette != nil:
		return m.paletteKey(k)
	case m.confirming != nil:
		return m.confirmKey(k)
	case m.filtering:
		m.filterKey(k)
		return nil
	case m.scr == scrRun:
		return m.runKey(k)
	}

	// vim count prefix: 5j, 12G
	if len(s) == 1 && s[0] >= '1' && s[0] <= '9' || (s == "0" && m.count > 0) {
		m.count = m.count*10 + int(s[0]-'0')
		if m.count > 9999 {
			m.count = 9999
		}
		return nil
	}
	n := max(m.count, 1)
	hadCount := m.count > 0
	m.count = 0
	if s != "g" {
		m.pendingG = false
	}

	l := m.cur()
	page := m.listHeight()
	switch s {
	case "q", "ctrl+c":
		return tea.Quit
	case ":":
		m.openPalette()
		return nil
	case "!":
		return m.shellHere()
	case "?":
		if m.scr == scrHelp {
			m.scr = m.prev
		} else {
			m.prev, m.scr = m.scr, scrHelp
		}
		return nil
	case "esc", "h", "left":
		switch {
		case m.scr == scrHelp:
			m.scr = m.prev
		case s == "esc" && l.filter != "":
			l.filter = ""
		case m.pane == paneActions && s != "esc":
			m.pane = paneChecks
		case m.scr == scrSite:
			m.scr, m.pane = scrSites, paneChecks
		case s == "esc":
			m.gotoTab("o")
		}
		return nil

	// tabs and panes
	case "ctrl+l":
		m.cycleTab(1)
		return nil
	case "ctrl+h":
		m.cycleTab(-1)
		return nil
	case "L", "tab":
		if m.hasActions() {
			m.pane = paneActions
			m.detailOff = 0
		}
		return nil
	case "H", "shift+tab":
		m.pane = paneChecks
		m.detailOff = 0
		return nil
	case "]", "[":
		if m.scr == scrSite {
			m.stepSite(map[string]int{"]": 1, "[": -1}[s])
		}
		return nil

	// movement
	case "j", "down":
		m.move(l, n)
	case "k", "up":
		m.move(l, -n)
	case "ctrl+d":
		m.move(l, page/2)
	case "ctrl+u":
		m.move(l, -page/2)
	case "ctrl+f", "pgdown":
		m.move(l, page)
	case "ctrl+b", "pgup":
		m.move(l, -page)
	case "g":
		if m.pendingG {
			l.cursor, m.pendingG, m.detailOff = 0, false, 0
		} else {
			m.pendingG = true
		}
	case "home":
		l.cursor, m.detailOff = 0, 0
	case "G", "end":
		if hadCount {
			l.cursor = n - 1
		} else {
			l.cursor = 1 << 30
		}
		m.detailOff = 0
	case "ctrl+e":
		m.detailOff++
	case "ctrl+y":
		m.detailOff = max(m.detailOff-1, 0)

	case "r":
		m.setStatus("refreshing — restic and WordPress checksums keep their interval (R forces)")
		return m.startRefresh(false, nil)
	case "R":
		m.setStatus("force refresh — probing everything")
		return m.startRefresh(true, nil)
	case "a":
		if m.scr == scrOverview {
			m.showAll = !m.showAll
		}
	case "/":
		m.filtering = true
	case "enter", "l", "right":
		return m.enter()
	default:
		for _, sec := range sections {
			if s == sec.key {
				m.gotoTab(s)
			}
		}
		if s == "o" || s == "s" || s == "v" {
			m.gotoTab(s)
		}
	}
	return nil
}

func (m *model) move(l *list, d int) {
	l.cursor += d
	if l.cursor < 0 {
		l.cursor = 0
	}
	m.detailOff = 0
}

func (m *model) filterKey(k tea.KeyMsg) {
	l := m.cur()
	switch k.Type {
	case tea.KeyEnter:
		m.filtering = false
	case tea.KeyEsc:
		m.filtering, l.filter = false, ""
	case tea.KeyBackspace:
		if l.filter != "" {
			r := []rune(l.filter)
			l.filter = string(r[:len(r)-1])
		}
	case tea.KeyRunes, tea.KeySpace:
		l.filter += string(k.Runes)
	}
	l.cursor, l.offset = 0, 0
}

// currentTab is the tab key for the screen (a site belongs to Sites).
func (m *model) currentTab() string {
	switch m.scr {
	case scrSites, scrSite:
		return "s"
	case scrLog:
		return "v"
	case scrSection:
		for _, s := range sections {
			if s.id == m.sectionID {
				return s.key
			}
		}
	}
	return "o"
}

func (m *model) cycleTab(d int) {
	cur := m.currentTab()
	for i, t := range tabOrder {
		if t == cur {
			m.gotoTab(tabOrder[(i+d+len(tabOrder))%len(tabOrder)])
			return
		}
	}
}

func (m *model) gotoTab(key string) {
	m.pane, m.detailOff = paneChecks, 0
	switch key {
	case "o":
		m.scr = scrOverview
	case "s":
		m.scr = scrSites
	case "v":
		m.scr = scrLog
		m.loadLogs()
	default:
		for _, s := range sections {
			if s.key == key {
				m.scr, m.sectionID = scrSection, s.id
			}
		}
	}
}

func (m *model) stepSite(d int) {
	rows := m.siteRows()
	for i, r := range rows {
		if r.Name == m.siteName {
			j := (i + d + len(rows)) % len(rows)
			m.siteName = rows[j].Name
			m.lists["sites"].cursor = j
			m.detailOff = 0
			return
		}
	}
}

// enter: open a site, or start the selected action / the fix for a check.
func (m *model) enter() tea.Cmd {
	l := m.cur()
	switch {
	case m.scr == scrLog && m.pane == paneChecks:
		if rs := m.logRunRows(); l.cursor < len(rs) {
			m.openTranscript(rs[l.cursor])
		}
	case m.scr == scrLog:
		if ss := m.logSourceRows(); l.cursor < len(ss) {
			return m.openLog(ss[l.cursor])
		}
	case m.scr == scrSites:
		if ds := m.siteRows(); l.cursor < len(ds) {
			m.openSite(ds[l.cursor].Name)
		}
	case m.pane == paneActions:
		if acts := m.actionRows(); l.cursor < len(acts) {
			a := acts[l.cursor]
			if strings.HasPrefix(a.ID, "fix.") {
				m.ask(a, m.fixTarget(a))
			} else {
				m.ask(a, m.targetForScreen())
			}
		}
	default:
		rows := m.resultRows()
		if l.cursor >= len(rows) {
			return nil
		}
		r := rows[l.cursor]
		if fs := m.fixesFor(r); len(fs) > 0 {
			m.ask(fs[0], m.fixTarget(fs[0]))
			return nil
		}
		if r.Check == "site.http" && r.Effective(m.now()) >= check.Warn {
			if src, ok := m.errorLogFor(r.Subject); ok {
				return m.openLog(src)
			}
		}
		id, ok := action.ForCheck[r.Check]
		if !ok {
			// A site result opens its site; others have no single fix.
			if _, isSite := m.domain(r.Subject); isSite && m.scr != scrSite {
				m.openSite(r.Subject)
			}
			return nil
		}
		a, _ := action.ByID(id)
		t := action.Target{Host: m.host}
		if a.Site {
			d, ok := m.domain(r.Subject)
			if !ok {
				d, ok = m.domain(m.siteName)
			}
			if !ok {
				return nil
			}
			t.Domain = &d
		}
		m.ask(a, t)
	}
	return nil
}

func (m *model) openSite(name string) {
	m.siteName, m.scr, m.pane, m.detailOff = name, scrSite, paneChecks, 0
	l := m.cur()
	l.cursor, l.offset = 0, 0
}

func (m *model) targetForScreen() action.Target {
	t := action.Target{Host: m.host}
	if m.scr == scrSite {
		if d, ok := m.domain(m.siteName); ok {
			t.Domain = &d
		}
	}
	return t
}

// ask opens the confirmation overlay (every action shows its plan first).
func (m *model) ask(a action.Action, t action.Target) {
	if a.NeedsRepo && m.env.RepoDir == "" {
		m.setStatus("unavailable: the hestiascripts checkout was not found (set HS_REPO_DIR)")
		return
	}
	if a.Site && t.Domain == nil {
		return
	}
	m.confirming = &pendingAction{act: a, target: t}
	m.confirmOff = 0
	if a.Preview != nil {
		m.confirming.preview, m.confirming.previewErr = a.Preview()
	}
}

func (m *model) confirmKey(k tea.KeyMsg) tea.Cmd {
	p := m.confirming
	switch {
	case k.Type == tea.KeyEsc || k.String() == "ctrl+c":
		m.confirming = nil
		return nil
	case k.String() == "ctrl+e" || k.String() == "ctrl+d":
		m.confirmOff += map[string]int{"ctrl+e": 1, "ctrl+d": 8}[k.String()]
		return nil
	case k.String() == "ctrl+y" || k.String() == "ctrl+u":
		m.confirmOff = max(m.confirmOff-map[string]int{"ctrl+y": 1, "ctrl+u": 8}[k.String()], 0)
		return nil
	case p.act.Confirm == action.ConfirmTyped:
		switch k.Type {
		case tea.KeyEnter:
			if p.typed == p.target.Name() {
				m.confirming = nil
				return m.start(p.act, p.target)
			}
			m.setStatus("type " + p.target.Name() + " exactly to run it")
		case tea.KeyBackspace:
			if r := []rune(p.typed); len(r) > 0 {
				p.typed = string(r[:len(r)-1])
			}
		case tea.KeyRunes:
			p.typed += string(k.Runes)
		}
		return nil
	case p.act.Confirm == action.ConfirmYes:
		if k.String() == "y" {
			m.confirming = nil
			return m.start(p.act, p.target)
		}
		if k.String() == "n" {
			m.confirming = nil
		}
		return nil
	default: // ConfirmNone: enter runs
		if k.Type == tea.KeyEnter || k.String() == "l" {
			m.confirming = nil
			return m.start(p.act, p.target)
		}
	}
	return nil
}

// recheckChecks: the checks an action said it affects, scoped to its site.
func (m *model) recheckChecks(a action.Action, t action.Target) []check.Check {
	want := map[string]bool{}
	for _, id := range a.Recheck {
		want[id] = true
	}
	if len(want) == 0 {
		return nil
	}
	var out []check.Check
	for _, c := range box.All() {
		if want[c.ID] {
			out = append(out, c)
		}
	}
	for _, c := range site.All(m.env) {
		if want[c.ID] && (t.Domain == nil || c.Subject == t.Domain.Name) {
			out = append(out, c)
		}
	}
	return out
}

// shellHere opens a shell for what is on screen: the selected or open site
// as its user in public_html, otherwise root. No confirmation box — a shell
// is self-evident — but it is logged like any action.
func (m *model) shellHere() tea.Cmd {
	name := ""
	switch m.scr {
	case scrSite:
		name = m.siteName
	case scrSites:
		if rows := m.siteRows(); m.cur().cursor < len(rows) {
			name = rows[m.cur().cursor].Name
		}
	}
	if d, ok := m.domain(name); ok {
		a, _ := action.ByID("site.shell")
		return m.start(a, action.Target{Domain: &d, Host: m.host})
	}
	a, _ := action.ByID("system.shell")
	return m.start(a, action.Target{Host: m.host})
}
