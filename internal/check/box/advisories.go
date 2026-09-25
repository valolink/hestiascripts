package box

// HestiaCP's published security advisories against the installed version.
// The list is fetched from GitHub's advisory API (twice a day at most) so it
// stays current; the built-in table below — verified against that API on
// 2026-09-25 — is used when GitHub is unreachable.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	. "github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/hestia"
)

type Advisory struct {
	ID       string   `json:"ghsa_id"`
	Date     string   `json:"published_at"`
	Severity string   `json:"severity"`
	Summary  string   `json:"summary"`
	Patched  []string `json:"-"`
}

var builtinAdvisories = []Advisory{
	{"GHSA-8c5w-r7fj-qrqc", "2026-09-15", "medium", "top_panel() suspended-user logout renders the user list", []string{"1.10.5"}},
	{"GHSA-2hxh-jhww-923x", "2026-09-15", "medium", "demoted admin keeps admin execution via a stale session role", []string{"1.10.5"}},
	{"GHSA-r9q4-pmcm-5qqf", "2026-09-15", "critical", "local privilege escalation to root via unescaped config write", []string{"1.10.5"}},
	{"GHSA-vp7q-fjv5-5vg8", "2026-09-15", "medium", "admin can add cron jobs as the protected administrator", []string{"1.10.5"}},
	{"GHSA-pr4w-cfq5-99h2", "2026-08-24", "medium", "user can restore another user's backup into their own account", []string{"1.10.4", "1.9.10"}},
	{"GHSA-xffx-jj33-p2px", "2026-08-05", "critical", "low-privilege user to root RCE via v-update-user-backup-exclusions", []string{"1.9.9"}},
	{"GHSA-c69h-jgpw-h9cj", "2026-07-30", "medium", "any admin account can take over the ROOT_USER account", []string{"1.9.8"}},
	{"GHSA-2xw3-7h62-v4gf", "2026-07-30", "critical", "low-privilege user to root via the restic restore queue", []string{"1.9.8"}},
	{"GHSA-3g4r-pfpf-8697", "2026-07-30", "medium", "stored XSS in notifications", []string{"1.9.8"}},
	{"GHSA-cr7q-frhq-xw4v", "2026-07-17", "critical", "low-privilege to root via web.conf path fields in v-search-user-object", []string{"1.9.7"}},
	{"GHSA-8w7m-g9c2-9q9p", "2026-07-17", "high", "SQL injection in the database password", []string{"1.9.7"}},
	{"GHSA-fcq6-p8cj-xx3c", "2026-07-17", "high", "authenticated admin takeover", []string{"1.9.7"}},
	{"GHSA-w3mx-xq85-8qqc", "2026-07-17", "critical", "root RCE via double eval() in parse_object_kv_list()", []string{"1.9.5"}},
	{"GHSA-47mf-74xr-f8x9", "2026-07-17", "high", "second-order command injection in the queue system → root", []string{"1.9.5"}},
	{"GHSA-5fpv-c8rg-x6r3", "2026-07-17", "critical", "client to root RCE via newline injection in v-add-cron-job", []string{"1.9.5"}},
	{"GHSA-fg7j-gpvw-2m73", "2026-07-17", "high", "cross-site scripting in the panel", []string{"1.9.5"}},
	{"GHSA-73p3-rqpv-59wx", "2026-07-17", "high", "IP spoofing via the CF-Connecting-IP header", []string{"1.9.4"}},
	{"GHSA-gh6f-9gpr-x9m2", "2026-07-17", "critical", "unauthenticated RCE via the web terminal (the May 2026 entry point)", []string{"1.9.6"}},
}

// Affects: v is vulnerable when it is below the fix on its own branch, or —
// when its branch got no fix — below the newest fix (an older branch).
func (a Advisory) Affects(v string) bool {
	branch := func(x string) string {
		p := strings.Split(x, ".")
		if len(p) < 2 {
			return x
		}
		return p[0] + "." + p[1]
	}
	var newest string
	for _, p := range a.Patched {
		if branch(p) == branch(v) {
			return versionLess(v, p)
		}
		if newest == "" || versionLess(newest, p) {
			newest = p
		}
	}
	return newest != "" && versionLess(v, newest)
}

// fetchAdvisories reads GitHub's list; nil when unreachable or unparseable.
func fetchAdvisories(ctx context.Context, env *Env) []Advisory {
	hctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	code, body, err := env.Sys.HTTPGet(hctx, "https://api.github.com/repos/hestiacp/hestiacp/security-advisories?per_page=100",
		map[string]string{"Accept": "application/vnd.github+json", "User-Agent": "hs"})
	if err != nil || code != 200 {
		return nil
	}
	var raw []struct {
		Advisory
		Vulns []struct {
			Patched string `json:"patched_versions"`
		} `json:"vulnerabilities"`
	}
	if json.Unmarshal(body, &raw) != nil || len(raw) == 0 {
		return nil
	}
	var out []Advisory
	for _, r := range raw {
		a := r.Advisory
		if len(a.Date) > 10 {
			a.Date = a.Date[:10]
		}
		for _, v := range r.Vulns {
			for _, p := range strings.Split(v.Patched, ",") {
				if p = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(p), ">=")); p != "" {
					a.Patched = append(a.Patched, p)
				}
			}
		}
		if len(a.Patched) > 0 {
			out = append(out, a)
		}
	}
	return out
}

func checkAdvisories(ctx context.Context, env *Env) []Result {
	cur := hestia.Conf(env.Sys)["VERSION"]
	if cur == "" {
		return []Result{New(Unknown, "installed version not found in hestia.conf")}
	}
	list, source := fetchAdvisories(ctx, env), "GitHub advisories"
	if list == nil {
		list, source = builtinAdvisories, "built-in list (GitHub unreachable), verified 2026-09-25"
	}
	var hit []Advisory
	fix := cur
	for _, a := range list {
		if a.Affects(cur) {
			hit = append(hit, a)
			for _, p := range a.Patched {
				if versionLess(fix, p) {
					fix = p
				}
			}
		}
	}
	if len(hit) == 0 {
		return []Result{New(OK, fmt.Sprintf("%s: none of %d published advisories applies", cur, len(list))).Ev("source: " + source).Valid(24 * time.Hour)}
	}
	rank := map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3}
	sort.SliceStable(hit, func(i, j int) bool { return rank[hit[i].Severity] < rank[hit[j].Severity] })
	var ev []string
	crit := 0
	for _, a := range hit {
		if a.Severity == "critical" {
			crit++
		}
		ev = append(ev, fmt.Sprintf("%s %-8s %s (fixed in %s)", a.Date, a.Severity, a.ID+" — "+a.Summary, strings.Join(a.Patched, ", ")))
	}
	ev = append(ev, "source: "+source)
	st := Warn
	if crit > 0 {
		st = Fail
	}
	return []Result{New(st, fmt.Sprintf("%s is affected by %d published advisories (%d critical)", cur, len(hit), crit)).Ev(ev...).
		Because("Most of these turn any panel login into root. The fixes are in Hestia point releases.").
		Fixed("v-update-sys-hestia-all   # to " + fix + " or later")}
}
