package tui

import (
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/valolink/hestiascripts/internal/action"
	"github.com/valolink/hestiascripts/internal/logsrc"
)

// palette is the ":" command line: fuzzy search over every action, every
// site and every tab; ":q" quits.
type palette struct {
	input  string
	cursor int
}

type entry struct {
	label, hint string
	run         func() tea.Cmd
}

func (m *model) openPalette() { m.palette = &palette{} }

func (m *model) entries() []entry {
	var es []entry
	add := func(label, hint string, f func() tea.Cmd) { es = append(es, entry{label, hint, f}) }
	add("q", "quit", func() tea.Cmd { return tea.Quit })
	add("refresh", "re-run checks (intervals respected)", func() tea.Cmd { return m.startRefresh(false, nil) })
	add("refresh!", "force: probe everything now", func() tea.Cmd { return m.startRefresh(true, nil) })
	add("help", "keys and states", func() tea.Cmd { m.prev, m.scr = m.scr, scrHelp; return nil })
	for _, t := range tabOrder {
		t := t
		name := map[string]string{"o": "Overview", "s": "Sites", "v": "Log"}[t]
		for _, s := range sections {
			if s.key == t {
				name = s.label
			}
		}
		add("tab "+name, "go to tab", func() tea.Cmd { m.gotoTab(t); return nil })
	}
	// current site's actions first, so ":upd" on a site screen means that site
	if d, ok := m.domain(m.siteName); ok && m.scr == scrSite {
		for _, a := range action.ForSite(isWP(m.env, d)) {
			a, d := a, d
			add(a.Title, d.Name, func() tea.Cmd { m.ask(a, action.Target{Domain: &d, Host: m.host}); return nil })
		}
	}
	for _, sec := range sections {
		for _, a := range action.ForSection(sec.id) {
			a := a
			add(a.Title, sec.label, func() tea.Cmd { m.ask(a, action.Target{Host: m.host}); return nil })
		}
	}
	for _, d := range m.domains {
		name := d.Name
		add(name, "open site · "+d.User, func() tea.Cmd { m.openSite(name); return nil })
	}
	if m.logSources == nil {
		m.logSources = logsrc.List(m.env.Sys)
	}
	for _, src := range m.logSources {
		src := src
		add("log "+src.Group+" · "+src.Name, src.Label(), func() tea.Cmd { return m.openLog(src) })
	}
	return es
}

// fuzzy scores a subsequence match; lower is better, -1 = no match.
func fuzzy(pattern, s string) int {
	p, t := strings.ToLower(pattern), strings.ToLower(s)
	if p == "" {
		return 0
	}
	if i := strings.Index(t, p); i >= 0 {
		return i // contiguous matches win, earlier is better
	}
	score, pi, last := 100, 0, -1
	for ti := 0; ti < len(t) && pi < len(p); ti++ {
		if t[ti] == p[pi] {
			if last >= 0 {
				score += ti - last - 1
			}
			last = ti
			pi++
		}
	}
	if pi < len(p) {
		return -1
	}
	return score
}

func (m *model) paletteMatches() []entry {
	type scored struct {
		e entry
		s int
	}
	var out []scored
	for _, e := range m.entries() {
		s := fuzzy(m.palette.input, e.label)
		if h := fuzzy(m.palette.input, e.hint); s < 0 && h >= 0 {
			s = h + 200
		}
		if s >= 0 {
			out = append(out, scored{e, s})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].s < out[j].s })
	es := make([]entry, len(out))
	for i, x := range out {
		es[i] = x.e
	}
	return es
}

func (m *model) paletteKey(k tea.KeyMsg) tea.Cmd {
	p := m.palette
	switch k.String() {
	case "esc", "ctrl+c":
		m.palette = nil
		return nil
	case "enter":
		es := m.paletteMatches()
		m.palette = nil
		if p.input == "q" || p.input == "q!" || p.input == "qa" {
			return tea.Quit
		}
		if p.cursor < len(es) {
			return es[p.cursor].run()
		}
		return nil
	case "ctrl+n", "ctrl+j", "down", "tab":
		p.cursor++
	case "ctrl+p", "ctrl+k", "up", "shift+tab":
		p.cursor = max(p.cursor-1, 0)
	case "backspace", "ctrl+h":
		if r := []rune(p.input); len(r) > 0 {
			p.input = string(r[:len(r)-1])
			p.cursor = 0
		} else {
			m.palette = nil
		}
	case "ctrl+w":
		p.input, p.cursor = "", 0
	default:
		if k.Type == tea.KeyRunes || k.Type == tea.KeySpace {
			p.input += string(k.Runes)
			p.cursor = 0
		}
	}
	if n := len(m.paletteMatches()); p.cursor >= n {
		p.cursor = max(n-1, 0)
	}
	return nil
}
