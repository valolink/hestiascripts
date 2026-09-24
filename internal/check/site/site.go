// Package site holds the per-domain checks. Unlike box checks the list is
// built per run from every web domain of every Hestia user.
//
// WP-CLI always runs as the site's own user (runuser), never as root: even
// "before WordPress loads" commands include the site's PHP, and a compromised
// site must not get a root interpreter out of a health check.
package site

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	neturl "net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/logcap"
	"github.com/valolink/hestiascripts/internal/sys"
	"github.com/valolink/hestiascripts/internal/wpfiles"
)

// All builds the site checks for every domain on the box.
func All(env *Env) []Check {
	var cs []Check
	for _, d := range hestia.WebDomains(env.Sys) {
		d := d
		cs = append(cs, Check{ID: "site.http", Section: "sites", Title: "Site answers", Subject: d.Name,
			Timeout: 15 * time.Second, Run: func(ctx context.Context, env *Env) []Result { return checkHTTP(ctx, env, d) }})
		if !isWordPress(env.Sys, d) {
			continue
		}
		cs = append(cs,
			Check{ID: "site.core", Section: "sites", Title: "WordPress core", Subject: d.Name, Heavy: true,
				Timeout: 90 * time.Second, MinInterval: 6 * time.Hour, // checksums come from api.wordpress.org
				Run: func(ctx context.Context, env *Env) []Result { return checkCore(ctx, env, d) }},
			Check{ID: "site.updates", Section: "sites", Title: "WordPress updates", Subject: d.Name, Heavy: true,
				Timeout: 90 * time.Second, MinInterval: time.Hour,
				Run: func(ctx context.Context, env *Env) []Result { return checkUpdates(ctx, env, d) }},
			Check{ID: "site.debug", Section: "sites", Title: "WordPress debug mode", Subject: d.Name,
				Run: func(ctx context.Context, env *Env) []Result { return checkDebug(env, d) }},
		)
	}
	return cs
}

func isWordPress(s sys.Sys, d hestia.Domain) bool {
	_, err := s.Stat(d.DocRoot() + "/wp-config.php")
	return err == nil
}

// WP runs wp-cli as the domain's user.
func WP(ctx context.Context, env *Env, d hestia.Domain, args ...string) (string, error) {
	full := append([]string{"-u", d.User, "--", "env", "HOME=/home/" + d.User, "wp", "--path=" + d.DocRoot()}, args...)
	return env.Sys.Run(ctx, "runuser", full...)
}

func certDays(env *Env, d hestia.Domain) (int, bool) {
	b, err := env.Sys.ReadFile("/home/" + d.User + "/conf/web/" + d.Name + "/ssl/" + d.Name + ".crt")
	if err != nil {
		return 0, false
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return 0, false
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return 0, false
	}
	return int(c.NotAfter.Sub(env.Sys.Now()) / (24 * time.Hour)), true
}

func checkHTTP(ctx context.Context, env *Env, d hestia.Domain) []Result {
	if d.Suspend {
		return []Result{New(NA, "suspended").With("http", "suspended")}
	}
	scheme := "http"
	if d.SSL {
		scheme = "https"
	}
	// Follow redirects that stay on this site (http→https, apex→www): a 301
	// in front of a dead database must not read as "answers".
	own := map[string]bool{d.Name: true}
	for _, a := range d.Aliases {
		own[a] = true
	}
	url := scheme + "://" + d.Name + "/"
	var hops []string
	redirects := 0
	var code int
	var loc string
	var err error
	for i := 0; i < 4; i++ {
		code, loc, err = env.Sys.HTTPLocal(ctx, url, d.IP)
		if err != nil || code < 300 || code >= 400 || loc == "" {
			break
		}
		next, ok := sameSite(url, loc, own)
		if !ok {
			break
		}
		hops = append(hops, fmt.Sprintf("%d → %s", code, next))
		redirects++
		url = next
	}
	// Is this copy the one visitors reach? Demo boxes hold stale copies of
	// sites whose DNS moved (riverfinland.fi 2026-09-24).
	dns, live := "unresolved", false
	lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	if ips, lerr := env.Sys.LookupHost(lctx, d.Name); lerr == nil && len(ips) > 0 {
		dns = "elsewhere"
		for _, ip := range ips {
			if ip == d.IP {
				dns, live = "here", true
			}
		}
		if !live {
			hops = append(hops, "DNS → "+strings.Join(ips, ", ")+" (this box serves "+orIP(d.IP)+")")
		}
	}
	cancel()
	loop := err == nil && code >= 300 && code < 400 && redirects >= 4

	var r Result
	switch {
	case loop && live:
		r = New(Fail, "redirects in a loop").Because("Browsers give up with \"too many redirects\".").
			Fixed("check the redirect rules in nginx/.htaccess and WordPress's siteurl/home against the SSL setting")
	case loop:
		r = New(Warn, "redirects in a loop on this box (not the live copy)")
	case err != nil:
		r = New(Fail, "does not answer on this box").Ev(err.Error()).
			Because("The domain is configured here but its web server does not respond to it.").
			Fixed("curl -skI --resolve " + d.Name + ":443:" + orIP(d.IP) + " https://" + d.Name + "/ ; systemctl status nginx php*-fpm")
	case code >= 500:
		r = New(Fail, fmt.Sprintf("HTTP %d", code)).
			Because("Visitors get an error page.").
			Fixed("tail /var/log/" + webLog(d))
	case code >= 400:
		r = New(Warn, fmt.Sprintf("HTTP %d on the front page", code)).
			Because("The site answers but refuses or cannot find its own home page.")
	case code >= 300:
		r = New(OK, fmt.Sprintf("HTTP %d → %s", code, loc))
	default:
		r = New(OK, fmt.Sprintf("HTTP %d", code))
	}
	if len(hops) > 0 {
		r = r.Ev(hops...)
	}
	r = r.With("dns", dns)
	r = r.With("http", strconv.Itoa(code))
	if days, ok := certDays(env, d); ok && d.SSL {
		r = r.With("sslDays", strconv.Itoa(days))
	}
	return []Result{r}
}

// sameSite resolves loc against cur and reports whether it stays on one of
// the site's own hostnames.
func sameSite(cur, loc string, own map[string]bool) (string, bool) {
	base, err := neturl.Parse(cur)
	if err != nil {
		return "", false
	}
	u, err := base.Parse(loc)
	if err != nil || !own[u.Hostname()] || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	return u.String(), true
}

func webLog(d hestia.Domain) string { return "apache2/domains/" + d.Name + ".error.log" }

// verify-checksums flags files that should not exist or do not match — the
// check that would have found the alavus webshell in wp-includes/ (2026-09-24).
func checkCore(ctx context.Context, env *Env, d hestia.Domain) []Result {
	ver, err := WP(ctx, env, d, "core", "version")
	ver = strings.TrimSpace(ver)
	if err != nil || ver == "" {
		return []Result{New(Unknown, "wp core version failed").Ev(errDetail(err))}
	}
	out, err := WP(ctx, env, d, "core", "verify-checksums")
	detail := out
	if ee, ok := err.(*sys.ExitError); ok {
		detail += "\n" + ee.Stderr
	}
	// Classify: of everything verify-checksums reports, only modified files
	// and unexpected PHP look like a backdoor. PHP error logs in core dirs,
	// update leftovers (.rnd, truncated names) and missing files are real
	// but different problems — the fleet sweep found them on almost every
	// box, and one blanket Fail hid the case that matters.
	var modified, suspicious, errlogs, leftovers, missing []string
	for _, l := range strings.Split(detail, "\n") {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "Warning:") {
			continue // the closing "Error: … doesn't verify" line is a summary, not a file
		}
		l = strings.TrimSpace(strings.TrimPrefix(l, "Warning:"))
		if f, ok := strings.CutPrefix(l, "File should not exist: "); ok {
			switch wpfiles.ClassifyCoreExtra(f) {
			case wpfiles.ExtraSuspicious:
				suspicious = append(suspicious, f)
			case wpfiles.ExtraErrorLog:
				errlogs = append(errlogs, f)
			default:
				leftovers = append(leftovers, f)
			}
		} else if f, ok := strings.CutPrefix(l, "File doesn't verify against checksum: "); ok {
			modified = append(modified, f)
		} else if f, ok := strings.CutPrefix(l, "File doesn't exist: "); ok {
			missing = append(missing, f)
		}
	}
	var parts []string
	var ev []string
	add := func(list []string, label string) {
		if len(list) == 0 {
			return
		}
		parts = append(parts, fmt.Sprintf("%d %s", len(list), label))
		for _, f := range list[:min(len(list), 6)] {
			ev = append(ev, label+": "+f)
		}
		if len(list) > 6 {
			ev = append(ev, fmt.Sprintf("%s: … %d more", label, len(list)-6))
		}
	}
	add(modified, "modified")
	add(suspicious, "unexpected PHP")
	add(missing, "missing")
	add(errlogs, "PHP error logs")
	add(leftovers, "leftovers")
	switch {
	case len(modified)+len(suspicious) > 0:
		return []Result{New(Fail, fmt.Sprintf("WordPress %s: %s", ver, strings.Join(parts, ", "))).Ev(ev...).With("wp", ver).
			Because("Core files do not change in normal operation. A modified core file, or a PHP file core does not ship, is the classic backdoor (alavus: zwfile.php in wp-includes).").
			Fixed("inspect them; the fix quarantines extras and re-downloads core " + ver)}
	case len(missing) > 0:
		return []Result{New(Warn, fmt.Sprintf("WordPress %s: incomplete core — %s", ver, strings.Join(parts, ", "))).Ev(ev...).With("wp", ver).
			Because("Usually an interrupted update; the site breaks in odd places.").
			Fixed("re-download core " + ver)}
	case len(errlogs) > 0:
		return []Result{New(Warn, fmt.Sprintf("WordPress %s: %s in core directories", ver, strings.Join(parts, ", "))).Ev(ev...).With("wp", ver).
			Because("PHP wrote error logs into wp-admin/ or wp-includes/; they are downloadable over the web and carry paths and internals.").
			Fixed("move them to quarantine; find what errors in the site's own logs")}
	case len(leftovers) > 0:
		return []Result{New(Warn, fmt.Sprintf("WordPress %s: %s from an update or a tool", ver, strings.Join(parts, ", "))).Ev(ev...).With("wp", ver).
			Because("Not code, but not core either: truncated names mean an update was interrupted; .rnd and .htaccess come from tools.").
			Fixed("quarantine them; re-download core if names were truncated")}
	case strings.Contains(detail, "Couldn't get checksums") || strings.Contains(detail, "Could not retrieve"):
		return []Result{New(Unknown, "WordPress "+ver+": checksums unavailable").With("wp", ver).Ev(lastNonEmpty(detail))}
	case err != nil && !strings.Contains(detail, "verifies against checksums"):
		return []Result{New(Unknown, "WordPress "+ver+": verify-checksums failed").With("wp", ver).Ev(lastNonEmpty(detail))}
	}
	return []Result{New(OK, "WordPress "+ver+", core files match the release").With("wp", ver).Valid(24 * time.Hour)}
}

var csvLine = regexp.MustCompile(`^([^,]+),([^,]*),([^,]*)$`)

func checkUpdates(ctx context.Context, env *Env, d hestia.Domain) []Result {
	// --skip-update-check and reading the core transient keep this read-only:
	// `plugin list --update=available` and `core check-update` would refresh
	// WordPress's update transients (a write). The data is therefore as fresh
	// as WordPress's own twice-daily check.
	out, err := WP(ctx, env, d, "plugin", "list", "--update=available", "--skip-update-check",
		"--fields=name,version,update_version", "--format=csv", "--skip-plugins", "--skip-themes")
	if err != nil {
		return []Result{wpBootFailure(err, d)}
	}
	var plugins []string
	for _, l := range strings.Split(out, "\n") {
		if m := csvLine.FindStringSubmatch(strings.TrimSpace(l)); m != nil && m[1] != "name" {
			plugins = append(plugins, fmt.Sprintf("%s %s → %s", m[1], m[2], m[3]))
		}
	}
	var coreUp []string
	if raw, err := WP(ctx, env, d, "transient", "get", "update_core", "--network", "--format=json", "--skip-plugins", "--skip-themes"); err == nil {
		var t struct {
			Updates []struct {
				Response string `json:"response"`
				Current  string `json:"current"`
			} `json:"updates"`
		}
		if json.Unmarshal([]byte(raw), &t) == nil {
			for _, u := range t.Updates {
				if u.Response == "upgrade" {
					coreUp = append(coreUp, u.Current)
					break
				}
			}
		}
	}
	n := len(plugins) + len(coreUp)
	if n == 0 {
		return []Result{New(OK, "core and plugins up to date").With("updates", "0").
			Ev("as of WordPress's own last update check")}
	}
	ev := plugins
	if len(coreUp) > 0 {
		ev = append([]string{"core → " + strings.Join(coreUp, ", ")}, ev...)
	}
	return []Result{New(Warn, plural(n, "update", "updates")+" pending").Ev(ev...).With("updates", strconv.Itoa(n)).
		Because("Plugin updates carry most WordPress security fixes; the unauthenticated ones are exploited within days of disclosure.").
		Fixed("v-wp-update --user=" + d.User + " --domain=" + d.Name + " --dry-run")}
}

var (
	wpDebugOn  = regexp.MustCompile(`(?m)^\s*define\s*\(\s*['"]WP_DEBUG['"]\s*,\s*(true|1)\s*\)`)
	displayOff = regexp.MustCompile(`(?m)^\s*define\s*\(\s*['"]WP_DEBUG_DISPLAY['"]\s*,\s*(false|0)\s*\)`)
	debugLog   = regexp.MustCompile(`(?m)^\s*define\s*\(\s*['"]WP_DEBUG_LOG['"]\s*,\s*['"]([^'"]+)['"]\s*\)`)
)

// Debug on can be a choice: log kept in private/ (never served), capped by
// hs logcap, errors not printed to visitors. That reads as OK, not a warning.
func checkDebug(env *Env, d hestia.Domain) []Result {
	conf, err := env.Sys.ReadFile(d.DocRoot() + "/wp-config.php")
	if err != nil {
		return []Result{New(Unknown, "wp-config.php unreadable")}
	}
	if !wpDebugOn.Match(conf) {
		return []Result{New(OK, "WP_DEBUG off")}
	}
	private := "/home/" + d.User + "/web/" + d.Name + "/private/"
	var logPath string
	if m := debugLog.FindSubmatch(conf); m != nil {
		logPath = string(m[1])
	}
	switch {
	case !displayOff.Match(conf):
		return []Result{New(Warn, "WP_DEBUG is on and errors are shown to visitors").
			Because("WP_DEBUG_DISPLAY defaults to true: notices and paths print into the pages.").
			Fixed("keep debug on safely, or turn it off")}
	case logPath == "" || !strings.HasPrefix(logPath, private):
		return []Result{New(Warn, "WP_DEBUG is on, logging inside the web root").
			Because("wp-content/debug.log is served unless a deny rule is deployed, and grows without bound (alavus: 11.8 GB).").
			Fixed("keep debug on safely (log to private/, capped), or turn it off")}
	}
	size, capped := logcap.Managed(logPath)
	if !capped {
		return []Result{New(Warn, "WP_DEBUG is on, log in private/ but not size-capped").Ev(logPath).
			Because("Outside the web root, but nothing stops it growing.").Fixed("keep debug on safely (adds the cap)")}
	}
	r := New(OK, "debug on by choice — log in private/, capped at "+size).Ev(logPath)
	if fi, err := env.Sys.Stat(logPath); err == nil {
		r = r.Ev(fmt.Sprintf("current size %d MB", fi.Size()>>20))
	}
	return []Result{r}
}

// wpBootFailure turns "WordPress would not start" into the fault it is,
// rather than an Unknown: a site that cannot reach its database is down.
func wpBootFailure(err error, d hestia.Domain) Result {
	detail := errDetail(err)
	full := detail
	if ee, ok := err.(*sys.ExitError); ok {
		full = ee.Stderr
	}
	switch {
	case strings.Contains(full, "Error establishing a database connection"):
		return New(Fail, "WordPress cannot connect to its database").Ev(detail).
			Because("WordPress pages and wp-admin fail: wp-config.php points at a database, user or password that does not work.").
			Fixed("check DB_NAME/DB_USER/DB_PASSWORD in " + d.DocRoot() + "/wp-config.php against v-list-databases " + d.User)
	case strings.Contains(full, "wp core install"):
		return New(Fail, "WordPress tables are missing from its database").Ev("wp-cli: run `wp core install` to create database tables").
			Because("The database is reachable but empty or on another table prefix — the site shows the installer.").
			Fixed("check $table_prefix in wp-config.php; restore the database if it was emptied")
	}
	return New(Unknown, "wp-cli could not load the site").Ev(detail)
}

func orIP(ip string) string {
	if ip == "" {
		return "127.0.0.1"
	}
	return ip
}

func errDetail(err error) string {
	if ee, ok := err.(*sys.ExitError); ok && strings.TrimSpace(ee.Stderr) != "" {
		return lastNonEmpty(ee.Stderr)
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

func lastNonEmpty(s string) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(ls[len(ls)-1])
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
