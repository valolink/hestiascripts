// Package tui is hs's interactive dashboard.
//
// It opens on the saved results instantly, then refreshes in the background:
// box checks first (seconds), per-site checks after (WP-CLI, bounded by the
// engine's heavy limit). Every tab has a Checks pane and, where there is
// something to do, an Actions pane; actions show their exact command, ask
// for confirmation, run, and re-run the checks they affect.
//
// Keys follow nvim: ctrl+h/l between tabs, H/L between panes inside a tab,
// counts, gg/G, ctrl+d/u/f/b/e/y, / to filter, : for the palette.
package tui

import (
	"context"
	"os"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"fmt"

	"github.com/valolink/hestiascripts/internal/action"
	"github.com/valolink/hestiascripts/internal/actlog"
	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/check/box"
	"github.com/valolink/hestiascripts/internal/check/site"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/logsrc"
	"github.com/valolink/hestiascripts/internal/state"
)

// Run starts the dashboard.
func Run(env *check.Env, version string) error {
	lipgloss.SetColorProfile(colorProfile(os.Getenv("TERM"), os.Getenv("COLORTERM"), os.Getenv("HS_COLORS"), os.Getenv("NO_COLOR")))
	host, _ := os.Hostname()
	m := newModel(env, host, version)
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	if m.run != nil && m.run.cmd != nil && m.run.cmd.Process != nil && !m.run.done {
		m.run.cmd.Process.Kill()
	}
	return err
}

// colorProfile picks the palette depth. SSH does not forward COLORTERM, so a
// truecolor terminal (wezterm) arrives as xterm-256color and every Gruvbox
// colour would be rounded to the 256 palette — the selection most visibly.
// Plain screen/tmux TERMs are detected as colourless; they do 256.
func colorProfile(term, colorterm, hsColors, noColor string) termenv.Profile {
	switch {
	case noColor != "":
		return termenv.Ascii
	case hsColors == "256":
		return termenv.ANSI256
	case hsColors == "truecolor", colorterm == "truecolor", colorterm == "24bit":
		return termenv.TrueColor
	case strings.Contains(term, "256color"), term == "xterm-kitty", term == "wezterm", term == "alacritty", term == "xterm-ghostty":
		return termenv.TrueColor
	case strings.HasPrefix(term, "screen"), strings.HasPrefix(term, "tmux"), strings.HasPrefix(term, "xterm"):
		return termenv.ANSI256
	case term == "" || term == "dumb":
		return termenv.Ascii
	}
	return termenv.ANSI256
}

type screen int

const (
	scrOverview screen = iota
	scrSites
	scrSite
	scrSection
	scrHelp
	scrRun
	scrLog
)

type section struct {
	key   string
	id    string
	label string
}

var sections = []section{
	{"b", "backups", "Backups"},
	{"x", "security", "Security"},
	{"p", "performance", "Performance"},
	{"w", "web", "Web server"},
	{"m", "mail", "Mail"},
	{"n", "monitoring", "Monitoring"},
	{"y", "system", "System"},
}

// tabOrder is the ctrl+h / ctrl+l ring.
var tabOrder = func() []string {
	t := []string{"o", "s"}
	for _, s := range sections {
		t = append(t, s.key)
	}
	return append(t, "v")
}()

type pane int

const (
	paneChecks pane = iota
	paneActions
)

type list struct {
	cursor, offset int
	filter         string
}

// pendingAction is an action waiting for confirmation.
type pendingAction struct {
	act        action.Action
	target     action.Target
	typed      string
	preview    []string // fixes: computed from the live box when opened
	previewErr error
}

type model struct {
	env     *check.Env
	host    string
	version string

	results   []check.Result
	domains   []hestia.Domain
	refreshed time.Time

	scr, prev  screen
	sectionID  string
	siteName   string
	pane       pane
	showAll    bool
	lists      map[string]*list
	filtering  bool
	count      int  // vim count prefix
	pendingG   bool // first g of gg
	detailOff  int  // ctrl+e / ctrl+y
	confirming *pendingAction
	palette    *palette
	run        *run
	confirmOff int // scroll in the confirmation box

	logRuns    []actlog.Run
	logSources []logsrc.Source

	// the run whose re-checks are in flight: results before, to report changes
	recheckRun    *run
	recheckBefore map[string]check.Result

	refreshing bool
	phase      string
	done, tot  int
	events     chan tea.Msg
	spin       int

	width, height int
	status        string
	statusAt      time.Time
}

func newModel(env *check.Env, host, version string) *model {
	rs, at := state.Load()
	return &model{
		env: env, host: host, version: version,
		results: dropNA(rs), refreshed: at,
		domains: hestia.WebDomains(env.Sys),
		lists:   map[string]*list{},
		width:   100, height: 30,
	}
}

func (m *model) setStatus(s string) { m.status, m.statusAt = s, time.Now() }

// --- messages ----------------------------------------------------------------

type progressMsg struct {
	phase     string
	done, tot int
}
type phaseDoneMsg struct {
	results []check.Result
	final   bool
}
type tickMsg struct{}

func tick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.startRefresh(false, nil), tick())
}

// startRefresh runs checks in a goroutine, feeding progress and results
// back through m.events; results are merged and saved per phase. With only
// set, it re-runs just those checks (after an action), forced.
func (m *model) startRefresh(force bool, only []check.Check) tea.Cmd {
	if m.refreshing {
		return nil
	}
	m.refreshing = true
	m.events = make(chan tea.Msg, 64)
	prev := m.results
	env := m.env
	ch := m.events
	go func() {
		ctx := context.Background()
		send := func(phase string) func(int, int) {
			return func(d, t int) {
				select {
				case ch <- progressMsg{phase, d, t}:
				default:
				}
			}
		}
		if only != nil {
			res := check.RunCached(ctx, env, only, prev, true, send("re-checking"))
			ch <- phaseDoneMsg{results: check.Merge(prev, res), final: true}
			close(ch)
			return
		}
		boxRes := check.RunCached(ctx, env, box.All(), prev, force, send("box checks"))
		merged := check.Merge(prev, boxRes)
		ch <- phaseDoneMsg{results: merged}
		siteRes := check.RunCached(ctx, env, site.All(env), prev, force, send("sites"))
		ch <- phaseDoneMsg{results: check.Merge(merged, siteRes), final: true}
		close(ch)
	}()
	return m.wait()
}

func (m *model) wait() tea.Cmd {
	ch := m.events
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tickMsg:
		m.spin++
		if m.status != "" && time.Since(m.statusAt) > 6*time.Second {
			m.status = ""
		}
		return m, tick()
	case progressMsg:
		m.phase, m.done, m.tot = msg.phase, msg.done, msg.tot
		return m, m.wait()
	case phaseDoneMsg:
		if msg.final && m.recheckRun != nil {
			m.reportRecheck(msg.results)
		}
		m.results = dropNA(msg.results)
		m.refreshed = time.Now()
		m.domains = hestia.WebDomains(m.env.Sys)
		if err := state.Save(m.host, msg.results, m.refreshed); err != nil {
			m.setStatus("could not save results: " + err.Error())
		}
		if msg.final {
			m.refreshing = false
			return m, nil
		}
		return m, m.wait()
	case runLineMsg, runDoneMsg:
		return m, m.runUpdate(msg)
	case execDoneMsg:
		return m, m.execDone(msg)
	case logTickMsg:
		return m, m.reloadLog()
	case tea.KeyMsg:
		return m, m.key(msg)
	}
	return m, nil
}

func dropNA(rs []check.Result) []check.Result {
	var out []check.Result
	for _, r := range rs {
		if r.State != check.NA {
			out = append(out, r)
		}
	}
	return out
}

// --- lists ---------------------------------------------------------------------

func (m *model) listKey() string {
	var k string
	switch m.scr {
	case scrSites:
		k = "sites"
	case scrSite:
		k = "site:" + m.siteName
	case scrSection:
		k = "sec:" + m.sectionID
	case scrRun:
		k = "run"
	case scrLog:
		k = "log"
	default:
		k = "overview"
	}
	if m.pane == paneActions {
		k += ":actions"
	}
	return k
}

func (m *model) cur() *list {
	k := m.listKey()
	if m.lists[k] == nil {
		m.lists[k] = &list{}
	}
	return m.lists[k]
}

// hasActions: tabs with an Actions pane.
func (m *model) hasActions() bool {
	return m.scr == scrSection || m.scr == scrSite || m.scr == scrLog
}

// loadLogs refreshes the Log tab's two lists.
func (m *model) loadLogs() {
	runs, err := actlog.Runs()
	if err != nil {
		m.setStatus("action log: " + err.Error())
	}
	m.logRuns = runs
	m.logSources = logsrc.List(m.env.Sys)
}

func (m *model) logRunRows() []actlog.Run {
	l := m.lists["log"]
	var out []actlog.Run
	for _, r := range m.logRuns {
		if l == nil || matches(l.filter, r.Title, r.Target, r.Plan, r.Operator) {
			out = append(out, r)
		}
	}
	return out
}

func (m *model) logSourceRows() []logsrc.Source {
	l := m.lists["log:actions"]
	var out []logsrc.Source
	for _, s := range m.logSources {
		if l == nil || matches(l.filter, s.Group, s.Name, s.Path, s.Unit) {
			out = append(out, s)
		}
	}
	return out
}

// reportRecheck appends what the action's re-checks found to its viewer:
// the point is to see the effect, not just "done".
func (m *model) reportRecheck(after []check.Result) {
	r := m.recheckRun
	m.recheckRun = nil
	now := m.now()
	var out []string
	for _, a := range after {
		b, ok := m.recheckBefore[a.ID]
		if !ok && !keyIn(a.Key, m.recheckBefore) {
			continue
		}
		was := "new"
		if ok {
			was = stateName(b.Effective(now))
		}
		out = append(out, fmt.Sprintf("  %s → %s  %s %s: %s", was, stateName(a.Effective(now)), a.Title, a.Subject, a.Summary))
	}
	if len(out) == 0 {
		return
	}
	r.lines = append(r.lines, "", "── re-checked afterwards ──")
	r.lines = append(r.lines, out...)
}

func keyIn(key string, m map[string]check.Result) bool {
	for _, r := range m {
		if r.Key == key {
			return true
		}
	}
	return false
}

// --- data for screens ------------------------------------------------------------

func (m *model) now() time.Time { return m.env.Sys.Now() }

func matches(filter string, fields ...string) bool {
	if filter == "" {
		return true
	}
	f := strings.ToLower(filter)
	for _, x := range fields {
		if strings.Contains(strings.ToLower(x), f) {
			return true
		}
	}
	return false
}

func (m *model) resultRows() []check.Result {
	now := m.now()
	l := m.lists[strings.TrimSuffix(m.listKey(), ":actions")]
	filter := ""
	if l != nil {
		filter = l.filter
	}
	var out []check.Result
	for _, r := range m.results {
		st := r.Effective(now)
		switch m.scr {
		case scrOverview:
			if !m.showAll && (st == check.OK || st == check.Configured) {
				continue
			}
		case scrSection:
			if r.Section != m.sectionID {
				continue
			}
		case scrSite:
			if !m.belongsToSite(r) {
				continue
			}
		}
		if matches(filter, r.Title, r.Subject, r.Summary, r.Section) {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Effective(now) > out[j].Effective(now) })
	return out
}

// actionRows: the Actions pane of the current tab.
func (m *model) actionRows() []action.Action {
	var acts []action.Action
	switch m.scr {
	case scrSection:
		acts = action.ForSection(m.sectionID)
	case scrSite:
		if d, ok := m.domain(m.siteName); ok {
			acts = action.ForSite(isWP(m.env, d))
		}
	}
	acts = append(m.fixesHere(), acts...)
	l := m.lists[m.listKey()]
	if l == nil || l.filter == "" {
		return acts
	}
	var out []action.Action
	for _, a := range acts {
		if matches(l.filter, a.Title, a.Note, a.ID) {
			out = append(out, a)
		}
	}
	return out
}

func isWP(env *check.Env, d hestia.Domain) bool {
	_, err := env.Sys.Stat(d.DocRoot() + "/wp-config.php")
	return err == nil
}

func (m *model) domain(name string) (hestia.Domain, bool) {
	for _, d := range m.domains {
		if d.Name == name {
			return d, true
		}
	}
	return hestia.Domain{}, false
}

// belongsToSite: results about the domain itself, plus its user's backup.
func (m *model) belongsToSite(r check.Result) bool {
	if r.Subject == m.siteName || strings.HasPrefix(r.Subject, m.siteName+" ") {
		return true
	}
	d, ok := m.domain(m.siteName)
	return ok && r.Check == "backups.nightly" && (r.Subject == d.User || r.Subject == d.User+" db-hourly")
}

func (m *model) siteRows() []hestia.Domain {
	l := m.lists["sites"]
	filter := ""
	if l != nil {
		filter = l.filter
	}
	var out []hestia.Domain
	for _, d := range m.domains {
		if matches(filter, d.Name, d.User, d.Proxy) {
			out = append(out, d)
		}
	}
	now := m.now()
	sort.SliceStable(out, func(i, j int) bool {
		a, b := m.siteState(out[i], now), m.siteState(out[j], now)
		if a != b {
			return a > b
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (m *model) siteResults(d hestia.Domain) []check.Result {
	saved := m.siteName
	m.siteName = d.Name
	defer func() { m.siteName = saved }()
	var out []check.Result
	for _, r := range m.results {
		if m.belongsToSite(r) {
			out = append(out, r)
		}
	}
	return out
}

func (m *model) siteState(d hestia.Domain, now time.Time) check.State {
	worst := check.NA
	for _, r := range m.siteResults(d) {
		if s := r.Effective(now); s > worst {
			worst = s
		}
	}
	return worst
}

func (m *model) siteData(d hestia.Domain, check, key string) string {
	for _, r := range m.siteResults(d) {
		if r.Check == check && r.Data[key] != "" {
			return r.Data[key]
		}
	}
	return ""
}
