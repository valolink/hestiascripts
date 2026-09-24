package tui

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/valolink/hestiascripts/internal/action"
	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/fix"
	"github.com/valolink/hestiascripts/internal/logsrc"
)

// recheckFor: the checks a fix can change, beyond the one it resolves.
var recheckFor = map[string][]string{
	"dumps":                {"web.exposure"},
	"dumps-all":            {"web.exposure"},
	"debug-log":            {"web.exposure", "site.debug"},
	"wp-debug-off":         {"site.debug"},
	"uploads-php":          {"web.uploads-php"},
	"wp-core":              {"site.core", "site.http"},
	"redis-guard":          {"redis.sites"},
	"letsencrypt":          {"web.ssl", "site.http"},
	"hestia-conf-key":      {"hestia.services"},
	"postfix-start":        {"mail.delivery", "mail.queue"},
	"netdata-port":         {"netdata"},
	"proxy-wp-secure":      {"web.template-usage", "web.nginx", "site.http"},
	"proxy-wp-rocket":      {"web.template-usage", "web.nginx", "site.http"},
	"firewall-restore":     {"firewall", "fail2ban"},
	"firewall-restore-f2b": {"firewall", "fail2ban"},
	"maldet-deps":          {"maldet", "systemd.failed"},
	"redis-guard-all":      {"redis", "redis.sites"},
	"apt-timers":           {"updates.unattended", "updates.pending"},
	"wp-core-noise":        {"site.core"},
	"debug-keep":           {"web.exposure", "site.debug"},
	"debug-keep-on":        {"web.exposure", "site.debug"},
}

var riskConfirm = map[fix.Risk]action.Confirm{fix.ReadOnly: action.ConfirmNone, fix.Change: action.ConfirmYes, fix.Destructive: action.ConfirmTyped}

// fixAction wraps a fix as an action: `hs fix <id> <subject>`, streamed and
// logged like everything else, with a live preview for the plan screen.
func (m *model) fixAction(f fix.Fix, subject string) (action.Action, action.Target) {
	exe, _ := os.Executable()
	t := action.Target{Host: m.host}
	if f.Scope == "site" {
		if d, ok := m.domain(subject); ok {
			t.Domain = &d
		}
	}
	if f.Scope == "all" {
		subject = ""
	}
	argv := []string{exe, "fix", f.ID}
	if subject != "" {
		argv = append(argv, subject)
	}
	env := m.env
	a := action.Action{
		ID: "fix." + f.ID, Title: f.Title, Site: f.Scope == "site", Mode: action.Stream,
		Confirm: riskConfirm[f.Risk], Note: f.Note, How: f.How, Undo: f.Undo, Recheck: recheckFor[f.ID],
		Command: func(action.Target, string) []string { return argv },
		Preview: func() ([]string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			steps, err := f.Plan(ctx, env, subject)
			if err != nil {
				return nil, err
			}
			if len(steps) == 0 {
				return []string{"Nothing to do: the box no longer has what this fix resolves."}, nil
			}
			var out []string
			for _, s := range steps {
				for _, w := range strings.Split(s.Why, "\n") {
					if w != "" {
						out = append(out, "# "+w)
					}
				}
				out = append(out, "$ "+fix.Quote(s.Argv))
			}
			return out, nil
		},
	}
	if f.Scope == "all" {
		a.Title += " (all sites)"
	}
	return a, t
}

// fixesFor: the fixes for one result, as ready-to-run actions.
func (m *model) fixesFor(r check.Result) []action.Action {
	var out []action.Action
	for _, f := range fix.For(r) {
		subject := r.Subject
		if f.Scope == "site" {
			if _, ok := m.domain(subject); !ok {
				continue
			}
		}
		a, _ := m.fixAction(f, subject)
		out = append(out, a)
	}
	return out
}

// fixesHere: fixes for the non-OK results of the current tab, deduplicated —
// listed first in its Actions pane.
func (m *model) fixesHere() []action.Action {
	now := m.now()
	seen := map[string]bool{}
	var out []action.Action
	for _, r := range m.results {
		if st := r.Effective(now); st == check.OK || st == check.Configured {
			continue
		}
		switch m.scr {
		case scrSite:
			if !m.belongsToSite(r) {
				continue
			}
		case scrSection:
			if r.Section != m.sectionID {
				continue
			}
		default:
			continue
		}
		for _, a := range m.fixesFor(r) {
			key := a.Plan(action.Target{}, "")
			if seen[key] {
				continue
			}
			// site-scoped fixes belong on the site's screen, not in a section list
			if m.scr == scrSection && a.Site {
				continue
			}
			seen[key] = true
			out = append(out, a)
		}
	}
	return out
}

// fixTarget re-derives the target for a fix action (site from its argv).
func (m *model) fixTarget(a action.Action) action.Target {
	t := action.Target{Host: m.host}
	if argv := a.Command(t, ""); a.Site && len(argv) >= 4 {
		if d, ok := m.domain(argv[3]); ok {
			t.Domain = &d
		}
	}
	return t
}

// errorLogFor opens the domain's error log (for a site that does not answer).
func (m *model) errorLogFor(domain string) (logsrc.Source, bool) {
	for _, s := range logsrc.List(m.env.Sys) {
		if s.Group == "Site "+domain && strings.Contains(s.Name, "errors") {
			return s, true
		}
	}
	return logsrc.Source{}, false
}
