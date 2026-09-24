package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/valolink/hestiascripts/internal/action"
	"github.com/valolink/hestiascripts/internal/actlog"
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
	sBox   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cAcc).Padding(0, 1)
)

func ansiStrip(s string) string { return ansi.Strip(s) }

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
	var body string
	switch m.scr {
	case scrHelp:
		body = m.help()
	case scrSites:
		body = m.sitesView()
	case scrRun:
		body = m.runView()
	case scrLog:
		body = m.logView()
	default:
		body = m.resultsView()
	}
	out := m.header() + "\n" + m.tabs() + "\n" + body
	switch {
	case m.palette != nil:
		out = m.overlay(out, m.paletteView())
	case m.confirming != nil:
		out = m.overlay(out, m.confirmView())
	}
	return out
}

// overlay puts box over the bottom of the screen, above the footer line.
func (m *model) overlay(screen, box string) string {
	lines := strings.Split(screen, "\n")
	bl := strings.Split(box, "\n")
	start := max(len(lines)-1-len(bl), 3)
	for i, l := range bl {
		if start+i < len(lines)-1 {
			lines[start+i] = l
		}
	}
	return strings.Join(lines, "\n")
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
	case m.status != "":
		right = stateStyle(check.Warn).Render(m.status)
	case m.refreshing:
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		right = sAcc.Render(frames[m.spin%len(frames)]) + sDim.Render(fmt.Sprintf(" %s %d/%d", m.phase, m.done, m.tot))
	case !m.refreshed.IsZero():
		right = sDim.Render("checked " + ago(time.Since(m.refreshed)) + " ago")
	default:
		right = sDim.Render("no saved results")
	}
	if m.count > 0 {
		right = sAcc.Render(strconv.Itoa(m.count)) + " " + right
	}
	return padBetween(left, right, m.width)
}

func tabName(key string) string {
	if n, ok := map[string]string{"o": "Overview", "s": "Sites", "v": "Log"}[key]; ok {
		return n
	}
	for _, s := range sections {
		if s.key == key {
			return s.label
		}
	}
	return key
}

func (m *model) tabs() string {
	cur := m.currentTab()
	var parts []string
	for _, key := range tabOrder {
		// Label and badge styled separately: re-styling an already styled
		// string (underline on the active tab) leaks raw escapes.
		label := sTab.Render(key + " " + tabName(key))
		if key == cur && m.scr != scrHelp && m.scr != scrRun {
			label = sTabOn.Render(key + " " + tabName(key))
		}
		if n := m.badCount(key); n > 0 {
			label += stateStyle(check.Warn).Render(strconv.Itoa(n))
		}
		parts = append(parts, label+" ")
	}
	return truncate(strings.Join(parts, ""), m.width) + "\n" + sDim.Render(strings.Repeat("─", max(m.width, 10)))
}

// badCount: Fail+Warn per tab, so problems are visible before opening it.
func (m *model) badCount(key string) int {
	if key == "o" || key == "v" {
		return 0
	}
	now := m.now()
	n := 0
	for _, r := range m.results {
		st := r.Effective(now)
		if st != check.Fail && st != check.Warn {
			continue
		}
		if key == "s" {
			if r.Section == "sites" {
				n++
			}
			continue
		}
		for _, s := range sections {
			if s.key == key && s.id == r.Section {
				n++
			}
		}
	}
	return n
}

func (m *model) listHeight() int {
	return max(m.height-4-m.detailHeight()-1, 3)
}

func (m *model) detailHeight() int {
	if m.scr == scrSites || m.scr == scrHelp || m.scr == scrRun {
		return 0
	}
	return max(m.height*2/5, 8)
}

func (m *model) footer(extra string) string {
	l := m.cur()
	keys := "j/k · ^h/^l tabs · / filter · : palette · r refresh · ? help · q quit"
	if m.hasActions() {
		keys = "H/L panes · " + keys
	}
	if extra != "" {
		keys = extra + " · " + keys
	}
	if m.filtering {
		return sAcc.Render("/"+l.filter+"▏") + sDim.Render("  enter keep · esc clear")
	}
	if l.filter != "" {
		keys = sAcc.Render("filter: "+l.filter) + sDim.Render(" (esc clears) · ") + keys
	}
	return sDim.Render(truncate(keys, m.width))
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

func (m *model) paneBar() string {
	if !m.hasActions() {
		return ""
	}
	left, right := "Checks", "Actions"
	if m.scr == scrLog {
		left, right = "hs actions", "Server logs"
	}
	c, a := sTab.Render(left), sTab.Render(right)
	if m.pane == paneChecks {
		c = sTabOn.Render(left)
	} else {
		a = sTabOn.Render(right)
	}
	return c + sDim.Render("⇄") + a + sDim.Render("  H/L") + "\n"
}

// listRows renders a window of rows with the cursor highlighted.
func (m *model) listRows(b *strings.Builder, l *list, n, room int, row func(i int) (string, check.State)) int {
	clamp(l, n, room)
	lines := 0
	for i := l.offset; i < n && lines < room; i++ {
		line, st := row(i)
		if i == l.cursor {
			line = selected(st, line, m.width)
		} else {
			line = truncate(line, m.width)
		}
		b.WriteString(line + "\n")
		lines++
	}
	return lines
}

func (m *model) resultsView() string {
	l := m.cur()
	h := m.listHeight()
	var b strings.Builder
	title := ""
	switch m.scr {
	case scrOverview:
		title = "Everything that is not proven working, worst first"
		if m.showAll {
			title = "All results, worst first"
		}
	case scrSection:
		title = tabName(m.currentTab())
	case scrSite:
		title = m.siteTitle()
	}
	b.WriteString(sBold.Render(truncate(title, m.width)) + "\n")
	lines := 1
	if bar := m.paneBar(); bar != "" {
		b.WriteString(bar)
		lines++
	}

	var detail string
	if m.pane == paneActions {
		acts := m.actionRows()
		if len(acts) == 0 {
			b.WriteString(sDim.Render("No actions here.") + "\n")
			lines++
		}
		lines += m.listRows(&b, l, len(acts), h-lines, func(i int) (string, check.State) {
			a := acts[i]
			if a.NeedsRepo && m.env.RepoDir == "" {
				return sDim.Render("  " + a.Title + " — needs the hestiascripts checkout"), check.Unknown
			}
			return fmt.Sprintf("%s %s%s", actionMark(a), sBold.Render(a.Title), sDim.Render(" — "+confirmLabel(a))), check.Configured
		})
		if len(acts) > 0 {
			detail = m.actionDetail(acts[l.cursor])
		}
	} else {
		rows := m.resultRows()
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
		lines += m.listRows(&b, l, len(rows), h-lines, func(i int) (string, check.State) {
			r := rows[i]
			return fmt.Sprintf("%s %s", icon(r.Effective(now)), rowText(r, m.scr)), r.Effective(now)
		})
		if len(rows) > 0 {
			detail = m.detail(rows[l.cursor])
		}
	}
	for ; lines < h; lines++ {
		b.WriteString("\n")
	}
	b.WriteString(sPane.Width(m.width).Render(fitLines(detail, m.detailHeight()-1, m.detailOff)) + "\n")
	extra := ""
	switch m.scr {
	case scrOverview:
		extra = "a all/problems"
	case scrSite:
		extra = "[ ] prev/next site · h back"
	}
	b.WriteString(m.footer(extra))
	return b.String()
}

func (m *model) logView() string {
	l := m.cur()
	h := m.listHeight()
	var b strings.Builder
	b.WriteString(sBold.Render("Log") + sDim.Render("  every hs action with its full output · every log on this box") + "\n")
	b.WriteString(m.paneBar())
	lines := 2
	var detail string
	if m.pane == paneChecks {
		runs := m.logRunRows()
		if len(runs) == 0 {
			b.WriteString(sDim.Render("No actions logged yet. They appear here as soon as one starts.") + "\n")
			lines++
		}
		lines += m.listRows(&b, l, len(runs), h-lines, func(i int) (string, check.State) {
			r := runs[i]
			st, res := runState(r)
			return fmt.Sprintf("%s %s  %s %s %s", icon(st), sDim.Render(r.At.Local().Format("2006-01-02 15:04")),
				sBold.Render(r.Title), sAcc.Render(r.Target), sDim.Render(res)), st
		})
		if len(runs) > 0 {
			detail = m.runDetail(runs[l.cursor])
		}
	} else {
		srcs := m.logSourceRows()
		lines += m.listRows(&b, l, len(srcs), h-lines, func(i int) (string, check.State) {
			s := srcs[i]
			size := ""
			if s.Size > 0 {
				size = humanSize(s.Size)
			}
			return fmt.Sprintf("  %s %s %s", sDim.Render(fmt.Sprintf("%-26s", trunc(s.Group, 26))), sBold.Render(s.Name), sDim.Render(size)), check.Configured
		})
		if len(srcs) > 0 {
			s := srcs[l.cursor]
			detail = sBold.Render(s.Group+" · "+s.Name) + "\n" + sAcc.Render(s.Label()) + "\n" +
				sDim.Render(fmt.Sprintf("enter opens the last %d lines, following new ones · / filters lines", tailLines))
		}
	}
	for ; lines < h; lines++ {
		b.WriteString("\n")
	}
	b.WriteString(sPane.Width(m.width).Render(fitLines(detail, m.detailHeight()-1, m.detailOff)) + "\n")
	b.WriteString(m.footer("enter open"))
	return b.String()
}

func runState(r actlog.Run) (check.State, string) {
	switch {
	case r.Interrupted():
		return check.Unknown, "no end recorded — still running, or interrupted"
	case r.Exit != nil && *r.Exit == 0:
		return check.OK, "exit 0 · " + ago(r.Ended.Sub(r.At))
	case r.Exit != nil:
		return check.Fail, fmt.Sprintf("exit %d · %s", *r.Exit, ago(r.Ended.Sub(r.At)))
	}
	return check.Unknown, ""
}

func (m *model) runDetail(r actlog.Run) string {
	st, res := runState(r)
	var b strings.Builder
	b.WriteString(icon(st) + " " + sBold.Render(r.Title) + " " + sAcc.Render(r.Target) + sDim.Render("  "+res) + "\n")
	b.WriteString(sDim.Render(fmt.Sprintf("%s · by %s · %s", r.At.Local().Format("2006-01-02 15:04:05"), r.Operator, r.Mode)) + "\n")
	b.WriteString(sAcc.Render("$ ") + wrap(r.Plan, m.width-4) + "\n")
	b.WriteString(sDim.Render("transcript " + r.Log + " · enter opens it"))
	return b.String()
}

func actionMark(a action.Action) string {
	if a.Mode == action.Interactive {
		return sAcc.Render("▸")
	}
	return sAcc.Render("›")
}

func confirmLabel(a action.Action) string {
	switch a.Confirm {
	case action.ConfirmTyped:
		return "destructive · typed confirmation"
	case action.ConfirmYes:
		return "changes the box · y to confirm"
	}
	if a.Mode == action.Interactive {
		return "interactive"
	}
	return "read-only"
}

func (m *model) actionDetail(a action.Action) string {
	var b strings.Builder
	t := m.targetForScreen()
	b.WriteString(sBold.Render(a.Title) + sDim.Render("  "+confirmLabel(a)) + "\n")
	if a.Note != "" {
		b.WriteString(wrap(a.Note, m.width-2) + "\n")
	}
	if !(a.NeedsRepo && m.env.RepoDir == "") {
		b.WriteString(sAcc.Render("$ ") + wrap(a.Plan(t, m.env.RepoDir), m.width-4) + "\n")
		for _, l := range action.Describe(a, t, m.env.RepoDir) {
			b.WriteString(sDim.Render("│ "+l) + "\n")
		}
	}
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
	if r.Why != "" && st != check.OK {
		b.WriteString(wrap(r.Why, m.width-2) + "\n")
	}
	if id, ok := action.ForCheck[r.Check]; ok && st != check.OK {
		if a, ok := action.ByID(id); ok {
			b.WriteString(sAcc.Render("⏎ ") + sBold.Render(a.Title) + sDim.Render("  (enter shows what it does first)") + "\n")
		}
	} else if r.Fix != "" && st != check.OK {
		b.WriteString(sAcc.Render("→ ") + r.Fix + "\n")
	}
	for _, e := range r.Evidence {
		b.WriteString(sDim.Render("  · "+e) + "\n")
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
	now := m.now()

	var b strings.Builder
	b.WriteString(sBold.Render(fmt.Sprintf("%d sites", len(rows))) + sDim.Render(" across every Hestia user — l/enter opens one · DNS other = visitors reach another server") + "\n")
	hdr := fmt.Sprintf("  %-34s %-14s %-5s %-9s %-7s %-22s %-6s %s", "DOMAIN", "USER", "DNS", "WP", "UPDATES", "PROXY TEMPLATE", "SSL", "BACKUP")
	b.WriteString(sDim.Render(truncate(hdr, m.width)) + "\n")
	lines := 2 + m.listRows(&b, l, len(rows), h-2, func(i int) (string, check.State) {
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
		st := m.siteState(d, now)
		return fmt.Sprintf("%s %-34s %-14s %-5s %-9s %-7s %-22s %-6s %s", icon(st),
			trunc(d.Name, 34), trunc(d.User, 14), orDash(dns), trunc(wp, 9), upd, trunc(proxy, 22), ssl, bk), st
	})
	for ; lines < m.height-4; lines++ {
		b.WriteString("\n")
	}
	b.WriteString(m.footer(""))
	return b.String()
}

func (m *model) runView() string {
	r := m.run
	var b strings.Builder
	var state string
	switch {
	case r.live && !r.done:
		state = sAcc.Render("● running " + ago(time.Since(r.started)))
	case r.code != nil:
		st := check.OK
		if *r.code != 0 {
			st = check.Fail
		}
		state = stateStyle(st).Render(fmt.Sprintf("exit %d", *r.code))
		if !r.ended.IsZero() {
			state += sDim.Render(" after " + ago(r.ended.Sub(r.started)))
		}
	case r.src != nil && r.follow:
		state = sAcc.Render("following")
	case r.src != nil:
		state = sDim.Render("paused — G follows")
	default:
		state = sDim.Render("no end recorded")
	}
	b.WriteString(padBetween(sBold.Render(r.title), state, m.width) + "\n")
	b.WriteString(sDim.Render(truncate(r.sub, m.width)) + "\n")
	h := m.height - 7
	vis := r.visible()
	if r.follow {
		r.offset = max(len(vis)-h, 0)
	}
	r.offset = min(r.offset, max(len(vis)-h, 0))
	lines := 0
	for i := r.offset; i < len(vis) && lines < h; i++ {
		line := vis[i]
		if strings.HasPrefix(line, "── ") {
			line = sAcc.Render(line)
		}
		b.WriteString(truncate(line, m.width) + "\n")
		lines++
	}
	for ; lines < h; lines++ {
		b.WriteString("\n")
	}
	pos := fmt.Sprintf("%d–%d of %d", min(r.offset+1, len(vis)), min(r.offset+h, len(vis)), len(vis))
	keys := "j/k · ^d/^u · gg top · G follow · / filter · "
	if r.live && !r.done {
		keys += "ctrl+c twice stops it"
	} else {
		keys += "h/esc back"
	}
	if r.filtering {
		keys = sAcc.Render("/"+r.filter+"▏") + "  enter keep · esc clear"
	} else if r.filter != "" {
		keys = sAcc.Render("filter: "+r.filter) + " · " + keys
	}
	b.WriteString(sDim.Render(strings.Repeat("─", m.width)) + "\n" + padBetween(sDim.Render(keys), sDim.Render(pos), m.width))
	return b.String()
}

func (m *model) confirmView() string {
	p := m.confirming
	a := p.act
	w := min(m.width-4, 110)
	var b strings.Builder
	b.WriteString(sBold.Render(a.Title) + "  " + sAcc.Render(p.target.Name()) + sDim.Render("  "+confirmLabel(a)) + "\n")
	if a.Note != "" {
		b.WriteString(wrap(a.Note, w-4) + "\n")
	}
	b.WriteString(sAcc.Render("$ ") + wrap(a.Plan(p.target, m.env.RepoDir), w-6) + "\n")
	desc := action.Describe(a, p.target, m.env.RepoDir)
	if len(desc) > 0 {
		room := max(m.height/2-8, 4)
		off := min(m.confirmOff, max(len(desc)-room, 0))
		m.confirmOff = off
		b.WriteString(sDim.Render("What it does, from its source:") + "\n")
		for i := off; i < len(desc) && i < off+room; i++ {
			b.WriteString(sDim.Render("│ "+trunc(desc[i], w-8)) + "\n")
		}
		if len(desc) > room {
			b.WriteString(sDim.Render(fmt.Sprintf("│ (%d–%d of %d · ctrl+e/ctrl+y scroll)", off+1, min(off+room, len(desc)), len(desc))) + "\n")
		}
	}
	if len(a.Recheck) > 0 {
		b.WriteString(sDim.Render("Afterwards it re-checks: "+strings.Join(a.Recheck, ", ")) + "\n")
	}
	b.WriteString(sDim.Render("Output is shown live and saved to the Log tab.") + "\n\n")
	switch a.Confirm {
	case action.ConfirmTyped:
		b.WriteString(stateStyle(check.Fail).Render("Destructive.") + " Type " + sBold.Render(p.target.Name()) + " and enter to run:  " +
			sAcc.Render(p.typed+"▏") + "   " + sDim.Render("esc cancels"))
	case action.ConfirmYes:
		b.WriteString(stateStyle(check.Warn).Render("Changes the box.") + " y runs it · n / esc cancels")
	default:
		if a.Mode == action.Interactive {
			b.WriteString("Interactive: the terminal is handed to it and comes back when it exits. " + sBold.Render("enter") + " runs · esc cancels")
		} else {
			b.WriteString("Read-only. " + sBold.Render("enter") + " runs · esc cancels")
		}
	}
	return sBox.Width(w).Render(b.String())
}

func (m *model) paletteView() string {
	p := m.palette
	es := m.paletteMatches()
	w := min(m.width-4, 110)
	var b strings.Builder
	b.WriteString(sAcc.Render(":") + p.input + sAcc.Render("▏") + "\n")
	n := min(len(es), 12)
	start := max(0, min(p.cursor-n+1, len(es)-n))
	for i := start; i < start+n; i++ {
		e := es[i]
		line := fmt.Sprintf("%-50s %s", trunc(e.label, 50), sDim.Render(e.hint))
		if i == p.cursor {
			line = selected(check.Configured, line, w-4)
		} else {
			line = truncate(line, w-4)
		}
		b.WriteString(line + "\n")
	}
	if len(es) == 0 {
		b.WriteString(sDim.Render("no match") + "\n")
	}
	b.WriteString(sDim.Render("ctrl+n/ctrl+p move · enter run · esc close · :q quits"))
	return sBox.Width(w).Render(b.String())
}

func (m *model) help() string {
	rows := [][2]string{
		{"ctrl+h ctrl+l", "previous / next tab"},
		{"H L", "switch pane inside a tab: Checks ⇄ Actions, or on Log: hs actions ⇄ Server logs"},
		{"o s b x p w m n y v", "Overview · Sites · Backups · Security · Performance · Web · Mail · Monitoring · System · Log"},
		{"j k  5j", "move, with a count"},
		{"gg G  12G", "top · bottom · line 12"},
		{"ctrl+d ctrl+u", "half page down / up        ctrl+f ctrl+b  full page"},
		{"ctrl+e ctrl+y", "scroll the detail pane (and the confirmation box)"},
		{"l enter  h esc", "open / run · back"},
		{"[ ]", "previous / next site, on a site screen"},
		{"/", "filter the current list, or the lines of a log (enter keeps it, esc clears)"},
		{":", "palette: every action, site, log and tab, fuzzy · :q quits"},
		{"r  R", "refresh (slow probes keep their interval) · force"},
		{"a", "Overview: problems only / all results"},
		{"q", "quit"},
	}
	var b strings.Builder
	b.WriteString(sBold.Render("Keys") + "\n\n")
	for _, r := range rows {
		b.WriteString(fmt.Sprintf("  %s  %s\n", sAcc.Render(fmt.Sprintf("%-20s", r[0])), r[1]))
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
	b.WriteString("\n" + sBold.Render("Actions") + "\n\n")
	b.WriteString("  " + sAcc.Render("›") + " output streamed here    " + sAcc.Render("▸") + " interactive: the terminal is handed over\n")
	b.WriteString("  Nothing runs unseen: every action shows its exact command and what the script says it does\n")
	b.WriteString("  first. Read-only runs on enter, changes need y, destructive ones need the name typed. Output\n")
	b.WriteString("  is live, saved with the command to the Log tab, and the checks it affects re-run afterwards.\n")
	b.WriteString("\n" + sDim.Render("hs "+m.version) + "\n")
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

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%d MB", n>>20)
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
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
	if len(r) <= n || n < 2 {
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

func padBetween(left, right string, w int) string {
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return truncate(left+" "+right, w)
	}
	return left + strings.Repeat(" ", gap) + right
}

// fitLines shows n lines of s starting at off (ctrl+e / ctrl+y).
func fitLines(s string, n, off int) string {
	ls := strings.Split(strings.TrimRight(s, "\n"), "\n")
	off = min(off, max(len(ls)-n, 0))
	ls = ls[off:]
	if len(ls) > n {
		ls = append(ls[:n-1], sDim.Render(fmt.Sprintf("… %d more lines (ctrl+e)", len(ls)-n+1)))
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
