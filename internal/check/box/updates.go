package box

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/valolink/hestiascripts/internal/aptrepo"
	. "github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/state"
)

// ProbeDir is where the repository probe keeps its InRelease copies and
// dearmored keyrings: hs's own state, never /var/lib/apt.
func ProbeDir() string { return filepath.Join(state.Dir(), "apt-probe") }

// Every repository still delivering updates. viona 2026-09-30: an empty
// Hestia keyring (never updated past 1.10.2) and an expired sury key (PHP and
// Apache on stale lists), while apt-daily reported success daily.
func checkRepos(ctx context.Context, env *Env) []Result {
	sts := aptrepo.Check(ctx, env.Sys, ProbeDir())
	total, _, failing := aptrepo.Summary(sts)
	if total == 0 {
		return []Result{New(Unknown, "no apt repositories found to check")}
	}
	if len(failing) == 0 {
		return []Result{New(OK, plural(total, "repository verified", "repositories verified")+" (signed InRelease, current key)").Valid(12 * time.Hour)}
	}
	var rs []Result
	for _, st := range failing {
		rs = append(rs, New(Fail, st.Repo.Host()+" "+st.Repo.Suite+": updates are failing").For(st.Repo.URI+" "+st.Repo.Suite).
			Ev(st.Reason, "defined in "+st.Repo.File, "InRelease "+st.Repo.InReleaseURL()).
			Because("A repository that cannot be verified stops delivering updates for its vendor's packages, and nothing says so: apt keeps the stale lists and apt-daily still reports success (viona: Hestia stuck on 1.10.2 for six weeks).").
			Fixed(st.Fix))
	}
	return rs
}

// AptStatus is the one-line JSON `hs apt status --json` prints and
// v-server-health embeds as "updates" for EngineLink.
type AptStatus struct {
	Repos struct {
		Total   int          `json:"total"`
		OK      int          `json:"ok"`
		Failing []RepoStatus `json:"failing"`
	} `json:"repos"`
	SecurityPending int `json:"securityPending"`
	Unattended      struct {
		Configured bool   `json:"configured"`
		Scope      string `json:"scope"`
		LastRun    string `json:"lastRun,omitempty"`
	} `json:"unattended"`
}

type RepoStatus struct {
	URL    string `json:"url"`
	Suite  string `json:"suite"`
	Host   string `json:"host"`
	Reason string `json:"reason"`
	Fix    string `json:"fix"`
}

func GetAptStatus(ctx context.Context, env *Env) AptStatus {
	var a AptStatus
	sts := aptrepo.Check(ctx, env.Sys, ProbeDir())
	total, ok, failing := aptrepo.Summary(sts)
	a.Repos.Total, a.Repos.OK = total, ok
	a.Repos.Failing = []RepoStatus{}
	for _, st := range failing {
		a.Repos.Failing = append(a.Repos.Failing, RepoStatus{URL: st.Repo.URI, Suite: st.Repo.Suite, Host: st.Repo.Host(), Reason: st.Reason, Fix: st.Fix})
	}
	sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	if out, err := env.Sys.Run(sctx, "apt-get", "-s", "upgrade"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "Inst ") && strings.Contains(line, "security") {
				a.SecurityPending++
			}
		}
	}
	cancel()
	a.Unattended.Scope = UnattendedScope(ctx, env)
	a.Unattended.Configured = a.Unattended.Scope == "security-only"
	if last := lastUnattendedRun(env); !last.IsZero() {
		a.Unattended.LastRun = last.Format(time.RFC3339)
	}
	return a
}
