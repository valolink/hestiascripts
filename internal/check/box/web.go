package box

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	. "github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/sys"
	"github.com/valolink/hestiascripts/internal/wpfiles"
)

func webChecks() []Check {
	return []Check{
		{ID: "web.nginx", Section: "web", Title: "nginx config", Run: checkNginxConfig},
		{ID: "web.templates", Section: "web", Title: "Web templates", Run: checkTemplates},
		{ID: "web.template-usage", Section: "web", Title: "Hardened template coverage", Run: checkTemplateUsage},
		{ID: "web.cache-headers", Section: "web", Title: "Cache-headers drop-in", Run: checkCacheHeaders},
		{ID: "web.ssl", Section: "web", Title: "SSL certificates", Run: checkSSL},
	}
}

// Proxy templates that carry the Valolink security rules.
var hardenedProxy = map[string]bool{"wp-secure": true, "wp-rocket": true, "wp-rocket-cartbypass": true}

const securityMarker = "Valolink security rules"

func checkNginxConfig(ctx context.Context, env *Env) []Result {
	if !env.Sys.Have("nginx") {
		return []Result{New(NA, "nginx not installed")}
	}
	_, err := env.Sys.Run(ctx, "nginx", "-t", "-q")
	if err != nil {
		detail := err.Error()
		if ee, ok := err.(*sys.ExitError); ok && strings.TrimSpace(ee.Stderr) != "" {
			detail = lastLine(ee.Stderr)
		}
		return []Result{New(Fail, "nginx -t fails").Ev(detail).
			Because("The running config still serves, but the next reload or restart (a rebuild, a certificate renewal) takes every site down.").
			Fixed("nginx -t")}
	}
	return []Result{New(OK, "nginx -t passes")}
}

// WPSecureBuilt is what setup-status reports as nginxTemplates.wpSecure: both
// files present AND the rules actually injected — a byte copy of default.tpl
// once passed a file-exists check while protecting nothing.
func WPSecureBuilt(env *Env) bool {
	return exists(env.Sys, hestia.NginxTpl+"/wp-secure.stpl") &&
		strings.Contains(readString(env.Sys, hestia.NginxTpl+"/wp-secure.tpl"), securityMarker)
}

func WPRocketInstalled(env *Env) bool {
	return exists(env.Sys, hestia.NginxTpl+"/wp-rocket.tpl") && exists(env.Sys, hestia.NginxTpl+"/wp-rocket.stpl")
}

func checkTemplates(ctx context.Context, env *Env) []Result {
	s := env.Sys
	if !exists(s, hestia.NginxTpl) {
		return []Result{New(NA, "no nginx templates directory")}
	}
	// wp-secure matters only through the sites on it: a site on wp-rocket
	// carries the same security rules. So an empty or missing wp-secure is a
	// finding only when a domain uses it, or when no hardened template exists.
	var rs []Result
	var onSecure []string
	for _, d := range hestia.WebDomains(s) {
		if d.Proxy == "wp-secure" {
			onSecure = append(onSecure, d.Name)
		}
	}
	anyHardened := false
	if ents, err := s.ReadDir(hestia.NginxTpl); err == nil {
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), ".tpl") && ProxyHardened(env, strings.TrimSuffix(e.Name(), ".tpl")) {
				anyHardened = true
			}
		}
	}
	switch {
	case !anyHardened:
		rs = append(rs, New(Warn, "no proxy template on this box carries the security rules").For("hardened templates").
			Because("There is no hardened template to put sites on.").Fixed("Web server → Install / update nginx templates"))
	case len(onSecure) > 0 && !WPSecureBuilt(env):
		rs = append(rs, New(Fail, "wp-secure.tpl contains no security rules, and sites use it").For("wp-secure").Ev(onSecure...).
			Because("It is a plain copy of default.tpl, so these domains are unhardened while reporting as protected.").
			Fixed("Web server → Install / update nginx templates, then rebuild those domains"))
	}
	if env.RepoDir != "" {
		for _, f := range []string{"wp-rocket.tpl", "wp-rocket.stpl"} {
			same, ok := sameFile(s, filepath.Join(env.RepoDir, "templates/nginx", f), hestia.NginxTpl+"/"+f)
			switch {
			case !ok && !exists(s, hestia.NginxTpl+"/"+f):
				rs = append(rs, New(Warn, f+" is not installed").For(f).Fixed("hs op web-templates"))
			case ok && !same:
				rs = append(rs, New(Warn, f+" differs from the repo").For(f).
					Because("Domains on it run rules nobody reviewed, or miss fixes that shipped since.").
					Fixed("hs op web-templates, then hs op web-rebuild"))
			}
		}
		if exists(s, hestia.NginxTpl+"/wp-rocket.tpl") && !exists(s, hestia.NginxTpl+"/wp-rocket-cartbypass.tpl") {
			rs = append(rs, New(Warn, "wp-rocket-cartbypass was not generated").For("wp-rocket-cartbypass").
				Fixed("hs op web-templates"))
		}
	}
	if len(rs) > 0 {
		return rs
	}
	if env.RepoDir == "" {
		return []Result{New(Configured, "hardened templates present; repo not found, so wp-rocket was not compared")}
	}
	return []Result{New(Configured, "hardened templates present, wp-rocket identical to the repo, cartbypass generated")}
}

// The layer nothing caught for months: a template only protects domains
// assigned to it (kuumalahde: all three domains sat on PROXY='default').
func checkTemplateUsage(ctx context.Context, env *Env) []Result {
	doms := hestia.WebDomains(env.Sys)
	if len(doms) == 0 {
		return []Result{New(NA, "no web domains")}
	}
	var rs []Result
	for _, d := range doms {
		if !ProxyHardened(env, d.Proxy) {
			rs = append(rs, New(Warn, "on proxy template "+orDash(d.Proxy)+", which has no security rules").For(d.Name).
				Because("None of the deny rules (dumps, logs, xmlrpc, PHP in uploads) apply to it, whatever is installed.").
				Fixed("v-change-web-domain-proxy-tpl "+d.User+" "+d.Name+" wp-secure   # or wp-rocket for WP Rocket sites"))
		}
	}
	if len(rs) > 0 {
		return rs
	}
	return []Result{New(Configured, fmt.Sprintf("all %d domains on a hardened proxy template", len(doms)))}
}

func checkCacheHeaders(ctx context.Context, env *Env) []Result {
	dst := "/etc/nginx/conf.d/valolink-cache-headers.conf"
	if !env.Sys.Have("nginx") {
		return []Result{New(NA, "nginx not installed")}
	}
	if !exists(env.Sys, dst) {
		return []Result{New(Warn, "not installed").
			Because("Unversioned CSS/JS gets a ten-year max-age, so plugin updates stay invisible to returning browsers.").
			Fixed("bash setup/install-cache-headers.sh")}
	}
	if env.RepoDir == "" {
		return []Result{New(Configured, "installed; repo not found, not compared")}
	}
	same, ok := sameFile(env.Sys, filepath.Join(env.RepoDir, "templates/nginx/valolink-cache-headers.conf"), dst)
	if ok && !same {
		return []Result{New(Warn, "differs from the repo").Fixed("bash setup/install-cache-headers.sh")}
	}
	return []Result{New(Configured, "installed, identical to the repo")}
}

func checkSSL(ctx context.Context, env *Env) []Result {
	s := env.Sys
	crts, _ := s.Glob("/home/*/conf/web/*/ssl/*.crt")
	now := s.Now()
	type exp struct {
		dom  string
		days int
	}
	var soon []exp
	valid := 0
	for _, f := range crts {
		b, err := s.ReadFile(f)
		if err != nil {
			continue
		}
		blk, _ := pem.Decode(b)
		if blk == nil {
			continue
		}
		cert, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			continue
		}
		days := int(cert.NotAfter.Sub(now) / (24 * time.Hour))
		if days < 14 {
			soon = append(soon, exp{strings.TrimSuffix(filepath.Base(f), ".crt"), days})
		} else {
			valid++
		}
	}
	if len(crts) == 0 {
		return []Result{New(NA, "no SSL certificates")}
	}
	sort.Slice(soon, func(i, j int) bool { return soon[i].days < soon[j].days })
	var rs []Result
	for _, e := range soon {
		state := Warn
		summary := fmt.Sprintf("expires in %d days", e.days)
		if e.days < 0 {
			state, summary = Fail, fmt.Sprintf("expired %d days ago", -e.days)
		}
		rs = append(rs, New(state, summary).For(e.dom).
			Because("Let's Encrypt renewal is failing for it; browsers hard-fail the site once it expires.").
			Fixed("v-add-letsencrypt-domain <user> "+e.dom))
	}
	if len(rs) > 0 {
		return rs
	}
	return []Result{New(OK, fmt.Sprintf("%d certificates, none expiring within 14 days", valid))}
}

func lastLine(s string) string {
	l := nonEmpty(s)
	if len(l) == 0 {
		return ""
	}
	return l[len(l)-1]
}

// ProxyHardened reads the template itself: hardened means it carries the
// security rules, whatever it is called (soutuveneet's hand-rolled
// "wprocket" does not; a stale wp-rocket copy might not either).
func ProxyHardened(env *Env, proxy string) bool {
	if proxy == "" {
		return false
	}
	return strings.Contains(readString(env.Sys, hestia.NginxTpl+"/"+proxy+".tpl"), wpfiles.SecurityMarker)
}
