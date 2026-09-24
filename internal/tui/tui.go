// Package tui is hs's interactive dashboard (phase 3: read-only).
//
// It opens on the saved results instantly, then refreshes in the background:
// box checks first (seconds), per-site checks after (WP-CLI, bounded by the
// engine's heavy limit). Keyboard only, one key per section.
package tui

import (
	"context"
	"os"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/check/box"
	"github.com/valolink/hestiascripts/internal/check/site"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/state"
)

// Run starts the dashboard.
func Run(env *check.Env, version string) error {
	host, _ := os.Hostname()
	m := newModel(env, host, version)
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

type screen int

const (
	scrOverview screen = iota
	scrSites
	scrSite
	scrSection
	scrHelp
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

type list struct {
	cursor, offset int
	filter         string
}

type model struct {
	env     *check.Env
	host    string
	version string

	results   []check.Result
	domains   []hestia.Domain
	loadedAt  time.Time
	refreshed time.Time

	scr, prev screen
	sectionID string
	siteName  string
	showAll   bool
	lists     map[string]*list
	filtering bool

	refreshing bool
	phase      string
	done, tot  int
	events     chan tea.Msg
	spin       int

	width, height int
	status        string
}

func newModel(env *check.Env, host, version string) *model {
	rs, at := state.Load()
	return &model{
		env: env, host: host, version: version,
		results: rs, loadedAt: at, refreshed: at,
		domains: hestia.WebDomains(env.Sys),
		lists:   map[string]*list{},
		width:   100, height: 30,
	}
}

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
	return tea.Batch(m.startRefresh(false), tick())
}

// startRefresh runs box then site checks in a goroutine, feeding progress and
// results back through m.events. Results are merged and saved per phase.
func (m *model) startRefresh(force bool) tea.Cmd {
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
		return m, tick()
	case progressMsg:
		m.phase, m.done, m.tot = msg.phase, msg.done, msg.tot
		return m, m.wait()
	case phaseDoneMsg:
		m.results = dropNA(msg.results)
		m.refreshed = time.Now()
		m.domains = hestia.WebDomains(m.env.Sys)
		if err := state.Save(m.host, msg.results, m.refreshed); err != nil {
			m.status = "could not save results: " + err.Error()
		}
		if msg.final {
			m.refreshing = false
			return m, nil
		}
		return m, m.wait()
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

// --- keys --------------------------------------------------------------------

func (m *model) key(k tea.KeyMsg) tea.Cmd {
	l := m.cur()
	if m.filtering {
		switch k.Type {
		case tea.KeyEnter:
			m.filtering = false
		case tea.KeyEsc:
			m.filtering, l.filter = false, ""
		case tea.KeyBackspace:
			if l.filter != "" {
				l.filter = l.filter[:len(l.filter)-1]
			}
		case tea.KeyRunes, tea.KeySpace:
			l.filter += string(k.Runes)
		}
		l.cursor, l.offset = 0, 0
		return nil
	}

	s := k.String()
	switch s {
	case "q", "ctrl+c":
		return tea.Quit
	case "?":
		if m.scr == scrHelp {
			m.scr = m.prev
		} else {
			m.prev, m.scr = m.scr, scrHelp
		}
		return nil
	case "esc":
		switch {
		case m.scr == scrHelp:
			m.scr = m.prev
		case l.filter != "":
			l.filter = ""
		case m.scr == scrSite:
			m.scr = scrSites
		default:
			m.scr = scrOverview
		}
		return nil
	case "o":
		m.scr = scrOverview
		return nil
	case "s":
		m.scr = scrSites
		return nil
	case "r":
		return m.startRefresh(false)
	case "R":
		return m.startRefresh(true)
	case "a":
		m.showAll = !m.showAll
		return nil
	case "/":
		m.filtering = true
		return nil
	case "j", "down":
		l.cursor++
	case "k", "up":
		l.cursor--
	case "pgdown", "ctrl+d":
		l.cursor += m.listHeight()
	case "pgup", "ctrl+u":
		l.cursor -= m.listHeight()
	case "g", "home":
		l.cursor = 0
	case "G", "end":
		l.cursor = 1 << 30
	case "enter", "l", "right":
		if m.scr == scrSites {
			if ds := m.siteRows(); l.cursor < len(ds) {
				m.siteName = ds[l.cursor].Name
				m.scr = scrSite
				m.cur().cursor, m.cur().offset = 0, 0
			}
		}
		return nil
	case "h", "left":
		if m.scr == scrSite {
			m.scr = scrSites
		}
		return nil
	}
	for _, sec := range sections {
		if s == sec.key {
			m.scr, m.sectionID = scrSection, sec.id
			return nil
		}
	}
	return nil
}

func (m *model) listKey() string {
	switch m.scr {
	case scrSites:
		return "sites"
	case scrSite:
		return "site:" + m.siteName
	case scrSection:
		return "sec:" + m.sectionID
	}
	return "overview"
}

func (m *model) cur() *list {
	k := m.listKey()
	if m.lists[k] == nil {
		m.lists[k] = &list{}
	}
	return m.lists[k]
}

// --- data for screens ----------------------------------------------------------

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
	l := m.cur()
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
		if matches(l.filter, r.Title, r.Subject, r.Summary, r.Section) {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Effective(now) > out[j].Effective(now) })
	return out
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
