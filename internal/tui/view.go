package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/valolink/hestiascripts/internal/check"
)

var (
	cFail = lipgloss.AdaptiveColor{Light: "#c0392b", Dark: "#ff6b6b"}
	cWarn = lipgloss.AdaptiveColor{Light: "#b7791f", Dark: "#f6c453"}
	cOK   = lipgloss.AdaptiveColor{Light: "#2f855a", Dark: "#68d391"}
	cDim  = lipgloss.AdaptiveColor{Light: "#718096", Dark: "#8a93a3"}
	cAcc  = lipgloss.AdaptiveColor{Light: "#2b6cb0", Dark: "#7fb3ff"}
	cSel  = lipgloss.AdaptiveColor{Light: "#e2e8f0", Dark: "#2d3748"}

	sBold  = lipgloss.NewStyle().Bold(true)
	sDim   = lipgloss.NewStyle().Foreground(cDim)
	sAcc   = lipgloss.NewStyle().Foreground(cAcc)
	sSel   = lipgloss.NewStyle().Background(cSel)
	sTab   = lipgloss.NewStyle().Padding(0, 1).Foreground(cDim)
	sTabOn = lipgloss.NewStyle().Padding(0, 1).Bold(true).Foreground(cAcc).Underline(true)
	sPane  = lipgloss.NewStyle().Border(lipgloss.NormalBorder(), true, false, false, false).BorderForeground(cDim)
)

func stateStyle(s check.State) lipgloss.Style {
	switch s {
	case check.Fail:
		return lipgloss.NewStyle().Foreground(cFail).Bold(true)
	case check.Warn:
		return lipgloss.NewStyle().Foreground(cWarn)
	case check.OK:
		return lipgloss.NewStyle().Foreground(cOK)
	}
	return sDim
}

func icon(s check.State) string {
	g := map[check.State]string{check.Fail: "✗", check.Warn: "!", check.Unknown: "?", check.Configured: "○", check.OK: "✓", check.NA: "·"}[s]
	return stateStyle(s).Render(g)
}

func (m *model) View() string {
	var b strings.Builder
	b.WriteString(m.header() + "\n")
	b.WriteString(m.tabs() + "\n")
	var body string
	switch m.scr {
	case scrHelp:
		body = m.help()
	case scrSites:
		body = m.sitesView()
	default:
		body = m.resultsView()
	}
	b.WriteString(body)
	return b.String()
}

func (m *model) header() string {
	c := check.Count(m.results, m.now())
	counts := strings.Join([]string{
		stateStyle(check.Fail).Render(fmt.Sprintf("%d fail", c[check.Fail])),
		stateStyle(check.Warn).Render(fmt.Sprintf("%d warn", c[check.Warn])),
		sDim.Render(fmt.Sprintf("%d unknown", c[check.Unknown])),
		sDim.Render(fmt.Sprintf("%d configured", c[check.Configured])),
		stateStyle(check.OK).Render(fmt.Sprintf("%d ok", c[check.OK])),
	}, " · ")
	left := sBold.Render("hs") + " " + sAcc.Render(m.host) + "   " + counts
	var right string
	switch {
	case m.refreshing:
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		right = sAcc.Render(frames[m.spin%len(frames)]) + sDim.Render(fmt.Sprintf(" %s %d/%d", m.phase, m.done, m.tot))
	case !m.refreshed.IsZero():
		right = sDim.Render("checked " + ago(time.Since(m.refreshed)) + " ago")
	default:
		right = sDim.Render("no saved results")
	}
	if m.status != "" {
		right = stateStyle(check.Warn).Render(m.status)
	}
	return padBetween(left, right, m.width)
}

func (m *model) tabs() string {
	type tab struct {
		key, label string
		on         bool
	}
	ts := []tab{{"o", "Overview", m.scr == scrOverview}, {"s", "Sites", m.scr == scrSites || m.scr == scrSite}}
	for _, s := range sections {
		ts = append(ts, tab{s.key, s.label, m.scr == scrSection && m.sectionID == s.id})
	}
	var parts []string
	for _, t := range ts {
		// Style the label and the badge separately: re-styling an already
		// styled string (underline on the active tab) leaks raw escapes.
		label := sTab.Render(t.key + " " + t.label)
		if t.on {
			label = sTabOn.Render(t.key + " " + t.label)
		}
		if n := m.badCount(t.key); n > 0 {
			label += stateStyle(check.Warn).Render(strconv.Itoa(n))
		}
		parts = append(parts, label+" ")
	}
	line := strings.Join(parts, "")
	return line + "\n" + sDim.Render(strings.Repeat("─", max(m.width, 10)))
}

// badCount: Fail+Warn per tab, so problems are visible before opening it.
func (m *model) badCount(key string) int {
	now := m.now()
	n := 0
	for _, r := range m.results {
		st := r.Effective(now)
		if st != check.Fail && st != check.Warn {
			continue
		}
		switch key {
		case "o":
		case "s":
			if r.Section != "sites" {
				continue
			}
		default:
			ok := false
			for _, s := range sections {
				if s.key == key && s.id == r.Section {
					ok = true
				}
			}
			if !ok {
				continue
			}
		}
		n++
	}
	if key == "o" {
		return 0 // the header already carries the totals
	}
	return n
}

func (m *model) listHeight() int {
	h := m.height - 4 - m.detailHeight() - 1
	return max(h, 3)
}

func (m *model) detailHeight() int {
	if m.scr == scrSites {
		return 0
	}
	return max(m.height*2/5, 8)
}

func (m *model) footer(extra string) string {
	l := m.cur()
	keys := "j/k move · / filter · r refresh · R force · ? help · q quit"
	if extra != "" {
		keys = extra + " · " + keys
	}
	if m.filtering {
		return sAcc.Render("/"+l.filter+"▏") + sDim.Render("  enter keep · esc clear")
	}
	if l.filter != "" {
		keys = sAcc.Render("filter: "+l.filter) + sDim.Render(" (esc clears) · ") + keys
	}
	return sDim.Render(keys)
}

// clamp keeps the cursor in range and the window around it.
func clamp(l *list, n, height int) {
	if l.cursor >= n {
		l.cursor = n - 1
	}
	if l.cursor < 0 {
		l.cursor = 0
	}
	if l.cursor < l.offset {
		l.offset = l.cursor
	}
	if l.cursor >= l.offset+height {
		l.offset = l.cursor - height + 1
	}
	if l.offset < 0 {
		l.offset = 0
	}
}

func (m *model) resultsView() string {
	rows := m.resultRows()
	l := m.cur()
	h := m.listHeight()
	clamp(l, len(rows), h)

	var b strings.Builder
	title := ""
	switch m.scr {
	case scrOverview:
		title = "Everything that is not proven working, worst first"
		if m.showAll {
			title = "All results, worst first"
		}
	case scrSection:
		for _, s := range sections {
			if s.id == m.sectionID {
				title = s.label
			}
		}
	case scrSite:
		title = m.siteTitle()
	}
	b.WriteString(sBold.Render(title) + "\n")
	lines := 1
	if len(rows) == 0 {
		msg := "Nothing here."
		if m.scr == scrOverview && !m.refreshing {
			msg = "Everything checked is working or configured. a shows all results."
		}
		if m.results == nil {
			msg = "No results yet — the first refresh is running."
		}
		b.WriteString(sDim.Render(msg) + "\n")
		lines++
	}
	now := m.now()
	for i := l.offset; i < len(rows) && i < l.offset+h-1; i++ {
		r := rows[i]
		line := fmt.Sprintf("%s %s", icon(r.Effective(now)), rowText(r, m.scr))
		if i == l.cursor {
			line = selected(r.Effective(now), line, m.width)
		} else {
			line = truncate(line, m.width)
		}
		b.WriteString(line + "\n")
		lines++
	}
	for ; lines < h; lines++ {
		b.WriteString("\n")
	}
	var detail string
	if len(rows) > 0 {
		detail = m.detail(rows[l.cursor])
	}
	b.WriteString(sPane.Width(m.width).Render(fitLines(detail, m.detailHeight()-1)) + "\n")
	extra := ""
	switch m.scr {
	case scrOverview:
		extra = "a all/problems"
	case scrSite:
		extra = "esc back to sites"
	}
	b.WriteString(m.footer(extra))
	return b.String()
}

func rowText(r check.Result, scr screen) string {
	subj := ""
	if r.Subject != "" && scr != scrSite {
		subj = sAcc.Render(r.Subject) + " "
	}
	sec := ""
	if scr == scrOverview {
		sec = sDim.Render(fmt.Sprintf("%-11s ", r.Section))
	}
	return sec + subj + sBold.Render(r.Title) + sDim.Render(" — ") + r.Summary
}

func (m *model) detail(r check.Result) string {
	now := m.now()
	var b strings.Builder
	st := r.Effective(now)
	head := icon(st) + " " + sBold.Render(r.Title)
	if r.Subject != "" {
		head += " " + sAcc.Render(r.Subject)
	}
	head += sDim.Render("  " + stateName(st) + " · checked " + ago(now.Sub(r.CheckedAt)) + " ago")
	b.WriteString(head + "\n")
	b.WriteString(r.Summary + "\n")
	if r.State == check.OK && st == check.Configured {
		b.WriteString(sDim.Render("Proven working "+ago(now.Sub(r.CheckedAt))+" ago; nothing has proven it since.") + "\n")
	}
	for _, e := range r.Evidence {
		b.WriteString(sDim.Render("  · "+e) + "\n")
	}
	if r.Why != "" && st != check.OK {
		b.WriteString(wrap(r.Why, m.width-2) + "\n")
	}
	if r.Fix != "" && st != check.OK {
		b.WriteString(sAcc.Render("→ ") + r.Fix + "\n")
	}
	return b.String()
}

func stateName(s check.State) string {
	return map[check.State]string{check.Fail: "fail", check.Warn: "warn", check.Unknown: "could not check",
		check.Configured: "configured, not proven", check.OK: "working", check.NA: "n/a"}[s]
}

func (m *model) siteTitle() string {
	d, ok := m.domain(m.siteName)
	if !ok {
		return m.siteName
	}
	parts := []string{"user " + d.User, "proxy " + orDash(d.Proxy), "php " + orDash(d.Backend)}
	if v := m.siteData(d, "site.core", "wp"); v != "" {
		parts = append(parts, "WordPress "+v)
	}
	return d.Name + sDim.Render("  "+strings.Join(parts, " · ")+"  "+d.DocRoot())
}

func (m *model) sitesView() string {
	rows := m.siteRows()
	l := m.cur()
	h := m.listHeight()
	clamp(l, len(rows), h-1)
	now := m.now()

	var b strings.Builder
	b.WriteString(sBold.Render(fmt.Sprintf("%d sites", len(rows))) + sDim.Render(" across every Hestia user — enter opens one · DNS other = visitors reach another server") + "\n")
	hdr := fmt.Sprintf("  %-34s %-14s %-5s %-9s %-7s %-22s %-6s %s", "DOMAIN", "USER", "DNS", "WP", "UPDATES", "PROXY TEMPLATE", "SSL", "BACKUP")
	b.WriteString(sDim.Render(truncate(hdr, m.width)) + "\n")
	lines := 2
	for i := l.offset; i < len(rows) && i < l.offset+h-2; i++ {
		d := rows[i]
		wp := orDash(m.siteData(d, "site.core", "wp"))
		upd := orDash(m.siteData(d, "site.updates", "updates"))
		ssl := "—"
		if v := m.siteData(d, "site.http", "sslDays"); v != "" {
			ssl = v + "d"
		} else if !d.SSL {
			ssl = "none"
		}
		bk := "—"
		for _, r := range m.results {
			if r.Check == "backups.nightly" && r.Subject == d.User && r.Data["ageHours"] != "" {
				bk = r.Data["ageHours"] + "h " + r.Data["source"]
			}
		}
		proxy := orDash(d.Proxy)
		if d.Suspend {
			proxy += " (suspended)"
		}
		dns := map[string]string{"here": "here", "elsewhere": "other", "unresolved": "none"}[m.siteData(d, "site.http", "dns")]
		line := fmt.Sprintf("%s %-34s %-14s %-5s %-9s %-7s %-22s %-6s %s", icon(m.siteState(d, now)),
			trunc(d.Name, 34), trunc(d.User, 14), orDash(dns), trunc(wp, 9), upd, trunc(proxy, 22), ssl, bk)
		if i == l.cursor {
			line = selected(m.siteState(d, now), line, m.width)
		} else {
			line = truncate(line, m.width)
		}
		b.WriteString(line + "\n")
		lines++
	}
	for ; lines < m.height-4; lines++ {
		b.WriteString("\n")
	}
	b.WriteString(m.footer("enter open"))
	return b.String()
}

func (m *model) help() string {
	rows := [][2]string{
		{"o", "Overview — everything not proven working, worst first (a toggles all results)"},
		{"s", "Sites — every domain of every Hestia user; enter opens one"},
		{"b x p w m n y", "Backups · Security · Performance · Web server · Mail · Monitoring · System"},
		{"j k ↑ ↓", "move    g G  top / bottom    ctrl+d ctrl+u  page"},
		{"/", "filter the current list (enter keeps it, esc clears)"},
		{"r", "refresh — slow probes (restic, WP core checksums) keep their minimum interval"},
		{"R", "force refresh — probe everything now"},
		{"esc", "back"},
		{"q", "quit"},
	}
	var b strings.Builder
	b.WriteString(sBold.Render("Keys") + "\n\n")
	for _, r := range rows {
		b.WriteString(fmt.Sprintf("  %s  %s\n", sAcc.Render(fmt.Sprintf("%-14s", r[0])), r[1]))
	}
	b.WriteString("\n" + sBold.Render("States") + "\n\n")
	for _, s := range []check.State{check.OK, check.Configured, check.Unknown, check.Warn, check.Fail} {
		desc := map[check.State]string{
			check.OK:         "a probe saw it working, recently enough to trust",
			check.Configured: "set up, but nothing has proven it works (or the proof went stale)",
			check.Unknown:    "the probe could not run — never counts as working",
			check.Warn:       "works, but degraded or risky",
			check.Fail:       "broken or dangerous",
		}[s]
		b.WriteString(fmt.Sprintf("  %s %-24s %s\n", icon(s), stateName(s), sDim.Render(desc)))
	}
	b.WriteString("\n" + sDim.Render("hs "+m.version+" · read-only: nothing here changes the server") + "\n")
	return b.String()
}

// selected re-renders a row on the highlight background. Wrapping an already
// styled line fails: its first reset code ends the background after one cell.
func selected(st check.State, line string, w int) string {
	plain := ansi.Strip(line)
	r := []rune(plain)
	if len(r) > w {
		r = r[:w]
	}
	for len(r) < w {
		r = append(r, ' ')
	}
	head, rest := string(r[:1]), string(r[1:])
	return stateStyle(st).Background(cSel).Render(head) + sSel.Bold(true).Render(rest)
}

// --- text helpers ----------------------------------------------------------------

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// truncate cuts a styled line to the terminal width.
func truncate(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(w).Render(s)
}

func padRight(s string, w int) string {
	if n := w - lipgloss.Width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

func padBetween(left, right string, w int) string {
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left + " " + right
	}
	return left + strings.Repeat(" ", gap) + right
}

func fitLines(s string, n int) string {
	ls := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(ls) > n {
		ls = append(ls[:n-1], sDim.Render(fmt.Sprintf("… %d more lines", len(ls)-n+1)))
	}
	for len(ls) < n {
		ls = append(ls, "")
	}
	return strings.Join(ls, "\n")
}

func wrap(s string, w int) string {
	if w < 20 {
		return s
	}
	var out, line []string
	n := 0
	for _, word := range strings.Fields(s) {
		if n+len(word)+1 > w && n > 0 {
			out = append(out, strings.Join(line, " "))
			line, n = nil, 0
		}
		line = append(line, word)
		n += len(word) + 1
	}
	if len(line) > 0 {
		out = append(out, strings.Join(line, " "))
	}
	return strings.Join(out, "\n")
}
