// Package watch is what makes the security checks noticed: run from cron,
// silent while nothing changes, and it reports — and mails — only findings
// that are new since a person last accepted the state of the box
// (docs/security-hardening.md, "The watch"). A finding already reported is
// mailed again at most once a day while it lasts.
package watch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/valolink/hestiascripts/internal/check"
)

// Fast checks run every 15 minutes; the sweep adds the filesystem-wide ones
// (hourly from cron; each also has its own MinInterval).
var (
	Fast = []string{"hestia.web-terminal", "persist.iocs", "persist.processes", "persist.outbound", "persist.accounts", "persist.cron",
		"ssh.authorized-keys", "ssh.private-keys", "firewall.ipv6", "egress", "hestia.advisories", "hestia.api", "web.exposure", "updates.signing"}
	Sweep = []string{"persist.unowned", "persist.units", "web.webshell", "web.root-owned", "web.uploads-php", "pkg.integrity"}
)

const DefaultMailTo = "tuotanto@valolink.fi"

func Dir(stateDir string) string { return filepath.Join(stateDir, "watch") }

var digits = regexp.MustCompile(`[0-9]+`)

// Key identifies a finding across runs: numbers in the summary are ignored,
// so "3 keys" becoming "4 keys" is the same finding (its subject changes
// when something new appears).
func Key(r check.Result) string {
	return r.Check + "|" + r.Subject + "|" + digits.ReplaceAllString(r.Summary, "#")
}

// Finding is a Warn or Fail result.
type Finding struct {
	Key     string `json:"-"`
	Check   string `json:"check"`
	Subject string `json:"subject,omitempty"`
	Summary string `json:"summary"`
	State   string `json:"state"`
}

func Findings(rs []check.Result, now time.Time) []Finding {
	var out []Finding
	for _, r := range rs {
		if st := r.Effective(now); st == check.Warn || st == check.Fail {
			out = append(out, Finding{Key: Key(r), Check: r.Check, Subject: r.Subject, Summary: r.Summary, State: st.String()})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].State == "fail" && out[j].State != "fail" })
	return out
}

// State is what the watch remembers.
type State struct {
	Baseline map[string]bool      `json:"baseline"` // accepted finding keys
	Alerted  map[string]time.Time `json:"alerted"`  // key → last mailed
	MailErr  string               `json:"mailError,omitempty"`
	MailAt   time.Time            `json:"mailErrorAt,omitempty"`
}

func path(dir string) string { return filepath.Join(dir, "state.json") }

func Load(dir string) State {
	st := State{Baseline: map[string]bool{}, Alerted: map[string]time.Time{}}
	if b, err := os.ReadFile(path(dir)); err == nil {
		json.Unmarshal(b, &st)
	}
	if st.Baseline == nil {
		st.Baseline = map[string]bool{}
	}
	if st.Alerted == nil {
		st.Alerted = map[string]time.Time{}
	}
	return st
}

func (s State) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := path(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path(dir))
}

// New: findings not in the baseline.
func (s State) New(fs []Finding) []Finding {
	var out []Finding
	for _, f := range fs {
		if !s.Baseline[f.Key] {
			out = append(out, f)
		}
	}
	return out
}

// Due: new findings not mailed in the last day.
func (s State) Due(fs []Finding, now time.Time) []Finding {
	var out []Finding
	for _, f := range fs {
		if t, ok := s.Alerted[f.Key]; !ok || now.Sub(t) >= 24*time.Hour {
			out = append(out, f)
		}
	}
	return out
}

// Accept records the current findings as the baseline (replacing it).
func (s *State) Accept(fs []Finding) {
	s.Baseline = map[string]bool{}
	for _, f := range fs {
		s.Baseline[f.Key] = true
	}
}

// Forget drops alert records of findings that are gone (so a recurrence
// is mailed at once).
func (s *State) Forget(current []Finding) {
	live := map[string]bool{}
	for _, f := range current {
		live[f.Key] = true
	}
	for k := range s.Alerted {
		if !live[k] {
			delete(s.Alerted, k)
		}
	}
}

// Summary is the JSON last line (the v-server-health contract: EngineLink
// parses the final line).
type Summary struct {
	New       []Finding `json:"new"`
	Critical  int       `json:"critical"`
	Warn      int       `json:"warn"`
	Accepted  int       `json:"accepted"`
	MailError string    `json:"mailError,omitempty"`
	CheckedAt string    `json:"checkedAt"`
}

func Summarise(newF []Finding, accepted int, mailErr string, now time.Time) Summary {
	s := Summary{New: newF, Accepted: accepted, MailError: mailErr, CheckedAt: now.UTC().Format(time.RFC3339)}
	if s.New == nil {
		s.New = []Finding{}
	}
	for _, f := range newF {
		if f.State == "fail" {
			s.Critical++
		} else {
			s.Warn++
		}
	}
	return s
}

// Mail renders the alert body.
func Mail(host string, fs []Finding) (subject, body string) {
	crit := 0
	for _, f := range fs {
		if f.State == "fail" {
			crit++
		}
	}
	subject = "[hs watch] " + host + ": " + plural(len(fs), "new security finding")
	if crit > 0 {
		subject = "[hs watch] " + host + ": " + plural(crit, "CRITICAL finding") + " — " + plural(len(fs), "new finding")
	}
	var b strings.Builder
	b.WriteString("New since the last accepted state of " + host + ":\n\n")
	for _, f := range fs {
		b.WriteString(strings.ToUpper(f.State) + "  " + f.Check)
		if f.Subject != "" {
			b.WriteString(" — " + f.Subject)
		}
		b.WriteString("\n      " + f.Summary + "\n")
	}
	b.WriteString("\nOn the box: hs check --section security   (details and fixes)\n")
	b.WriteString("Known and fine: hs watch --baseline   (accepts everything currently found)\n")
	b.WriteString("This mail repeats at most once a day per finding while it lasts.\n")
	return subject, b.String()
}

func plural(n int, s string) string {
	if n == 1 {
		return "1 " + s
	}
	return itoa(n) + " " + s + "s"
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
