package fix

import (
	"context"
	"fmt"
	neturl "net/url"
	"strings"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/check/site"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/robots"
)

// robotsNow reads the site the way `hs robots write` will: the same facts,
// the same rendering, so the plan shows exactly the file that gets written.
func robotsNow(ctx context.Context, env *check.Env, d hestia.Domain) (existing string, exists bool, f robots.Facts, err error) {
	path := robots.Path(d)
	if fi, lerr := env.Sys.Lstat(path); lerr == nil {
		if !fi.Mode().IsRegular() {
			return "", false, f, fmt.Errorf("%s is not a regular file — not touching it", path)
		}
		b, _ := env.Sys.ReadFile(path)
		existing, exists = string(b), true
	}
	f, err = robots.Gather(ctx, env.Sys, d, func(ctx context.Context, a ...string) (string, error) { return site.WP(ctx, env, d, a...) })
	return existing, exists, f, err
}

func init() {
	register(Fix{
		ID: "robots", Title: "Add the hs block to robots.txt", Check: "site.robots", Scope: "site", Risk: Change,
		Applies: func(r check.Result) bool { return r.Data["route"] == "file" },
		Note:    "soutuveneet.fi 2026-09-30: a crawler rendered every ?product_orderby= permutation uncached behind Hestia's placeholder robots.txt — load 15 on two cores.",
		How: "`hs robots write` (as the site's user, never root) keeps every line outside the `# hs:robots begin/end` markers and replaces what is between them: " +
			"Disallow /wp-admin/ with Allow for admin-ajax.php when the file lacks them, a Sitemap line (the first of the SEO plugin's index, WordPress's wp-sitemap.xml that answers 200 on this box) when it has none, " +
			"and for WooCommerce shops Disallow rules for sort/filter/add-to-cart query strings and the cart, checkout and my-account pages at the paths WooCommerce's own page settings give (they are localised: /ostoskori/, not /cart/). " +
			"Hestia's placeholder comment is dropped; its User-agent and Crawl-delay lines stay. The file is 644 and owned by the site user. The write refuses content other than the file shown here. robots.txt is advisory — crawlers that ignore it need an nginx rule (see docs/hs-design.md).",
		Undo: "copy the previous file back from private/hs-backup-<date>/robots.txt.<time> (or /var/lib/hs/backups); if there was none, remove robots.txt and WordPress serves its own again.",
		Plan: func(ctx context.Context, env *check.Env, subject string) ([]Step, error) {
			d, err := domainByName(env.Sys, subject)
			if err != nil {
				return nil, err
			}
			existing, exists, f, err := robotsNow(ctx, env, d)
			if err != nil {
				return nil, err
			}
			switch st := robots.Assess(existing, exists, f); st.Route {
			case "file":
			case "plugin":
				return nil, fmt.Errorf("%s serves robots.txt rules on %s; a physical file would override it — add these lines in its robots.txt editor instead:\n  %s", f.SEOPlugin, d.Name, strings.Join(robots.Rules(f), "\n  "))
			case "unshadow":
				return nil, fmt.Errorf("%s serves robots.txt rules on %s: remove Hestia's placeholder instead (hs fix robots-unshadow %s)", f.SEOPlugin, d.Name, d.Name)
			default:
				return nil, fmt.Errorf("%s needs nothing (%s robots.txt) — nothing to change", d.Name, st.Kind)
			}
			content := robots.Render(existing, f)
			if exists && content == existing {
				return nil, fmt.Errorf("%s already has the current hs block — nothing to change", robots.Path(d))
			}
			host := d.Name
			if u, err := neturl.Parse(f.Home); err == nil && u.Host != "" {
				host = u.Host
			}
			return []Step{
				{Why: "write " + robots.Path(d) + " (" + f.String() + ") — the whole file after:\n" + indent(content),
					Argv: []string{selfExe(), "robots", "write", d.Name, "--sum", robots.Sum(content)}},
				{Why: "what the site serves now (nginx may cache for a minute)",
					Argv: []string{"sh", "-c", `curl -sk --max-time 10 --resolve "$1:443:$2" "https://$1/robots.txt" | head -60`, "sh", host, orLocal(d.IP)}},
			}, nil
		},
	})

	register(Fix{
		ID: "robots-unshadow", Title: "Move Hestia's placeholder robots.txt aside", Check: "site.robots", Scope: "site", Risk: Change,
		Applies: func(r check.Result) bool { return r.Data["route"] == "unshadow" },
		Note:    "Hestia puts a Crawl-delay-only robots.txt in every new web domain; on a site whose SEO plugin builds robots.txt, that file hides the plugin's rules and sitemap.",
		How:     "The placeholder moves with `mv -n` to private/hs-moved-<date>/ (MANIFEST.tsv beside it); WordPress and the SEO plugin then serve robots.txt themselves. Add the WooCommerce rules in the plugin's robots.txt editor.",
		Undo:    "mv the file back from private/hs-moved-<date>/robots.txt to public_html/robots.txt.",
		Plan: func(ctx context.Context, env *check.Env, subject string) ([]Step, error) {
			d, err := domainByName(env.Sys, subject)
			if err != nil {
				return nil, err
			}
			existing, exists, _, err := robotsNow(ctx, env, d)
			if err != nil {
				return nil, err
			}
			if !exists || !strings.Contains(existing, robots.HestiaMarker) {
				return nil, fmt.Errorf("%s is not Hestia's placeholder — not moving it", robots.Path(d))
			}
			return moveSteps([]string{robots.Path(d)}, d.DocRoot(), private(d), d.User), nil
		},
	})
}

func indent(s string) string {
	var out []string
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		out = append(out, "    "+l)
	}
	return strings.Join(out, "\n")
}

func orLocal(ip string) string {
	if ip == "" {
		return "127.0.0.1"
	}
	return ip
}
