// Package webtpl builds the proxy and backend templates hs installs into
// Hestia: wp-secure (Hestia's default + the security snippet), wp-rocket
// (from the repo), wp-rocket-cartbypass (generated from wp-rocket) and the
// Apache wp-secure. A Go port of setup/nginx-templates.sh's
// _nginx_install_profiles; every result is verified before it is written,
// because this repo once shipped a wp-secure that contained no rules and a
// file-exists check called it installed.
package webtpl

import (
	"fmt"
	"regexp"
	"strings"
)

const Marker = "Valolink security rules"

var (
	locationRoot = regexp.MustCompile(`^[[:space:]]*location /[[:space:]]*\{`)
	directoryDoc = regexp.MustCompile(`^[[:space:]]*<Directory %s?docroot%>`)
	cartLine     = regexp.MustCompile(`(?m)^.*http_cookie.*woocommerce_items_in_cart.*$`)
	loggedInLine = regexp.MustCompile(`(?m)^.*http_cookie.*wordpress_logged_in_.*$`)
)

// injectAt puts snippet before (or after) the first line matching re.
func injectAt(content, snippet string, re *regexp.Regexp, after bool) (string, bool) {
	lines := strings.Split(content, "\n")
	for i, l := range lines {
		if re.MatchString(l) {
			sn := strings.Split(strings.TrimSuffix(snippet, "\n"), "\n")
			at := i
			if after {
				at = i + 1
			}
			out := append(append(append([]string{}, lines[:at]...), sn...), lines[at:]...)
			return strings.Join(out, "\n"), true
		}
	}
	return content, false
}

// WPSecureNginx: Hestia's default proxy template with the security snippet
// before the first `location / {` (any indentation — Hestia's default.tpl is
// tab-indented, and a four-space pattern once matched nothing). The SSL
// template roots its blocks at %sdocroot%, so the snippet follows it there.
func WPSecureNginx(def, snippet, ext string) (string, error) {
	if strings.Contains(def, Marker) {
		return def, nil
	}
	if ext == "stpl" {
		snippet = strings.ReplaceAll(snippet, "%docroot%", "%sdocroot%")
	}
	out, ok := injectAt(def, snippet, locationRoot, false)
	if !ok || !strings.Contains(out, Marker) {
		return "", fmt.Errorf("no `location / {` line in default.%s — the template layout changed; not writing wp-secure.%s", ext, ext)
	}
	return out, nil
}

// WPSecureApache: the snippet inside <Directory %docroot%> (per-directory
// rewrite rules must live in that block).
func WPSecureApache(def, snippet, ext string) (string, error) {
	if strings.Contains(def, Marker) {
		return def, nil
	}
	out, ok := injectAt(def, snippet, directoryDoc, true)
	if !ok || !strings.Contains(out, Marker) {
		return "", fmt.Errorf("no `<Directory %%docroot%%>` line in apache2 default.%s; not writing wp-secure.%s", ext, ext)
	}
	return out, nil
}

func header(ext string) string {
	return `#=========================================================================#
# GENERATED FILE — do not edit here.                                      #
# Source: hestiascripts templates/nginx/wp-rocket.` + ext + `
# Regenerate: hs → Web server → Install / update web templates            #
#                                                                         #
# Difference from wp-rocket: woocommerce_items_in_cart and                #
# woocommerce_cart_hash ARE in the cache-bypass list, so a shopper with   #
# anything in their cart is sent to PHP instead of being served the file. #
#                                                                         #
# >> THIS TEMPLATE ALONE DOES NOTHING. <<                                 #
#                                                                         #
# Skipping the nginx serve only hands the request to PHP, where WP        #
# Rocket's advanced-cache.php answers from the SAME cached file — it does #
# not reject those cookies by default. Measured on www.kuumalahde.fi      #
# 2026-09-02: with a cart cookie the ETag disappeared (nginx did bypass)   #
# but the body was byte-identical with the same cached@ stamp.            #
#                                                                         #
# To actually bypass, add BOTH cookie names to WP Rocket ->               #
# Advanced Rules -> Never Cache Cookies, or filter                        #
# rocket_cache_reject_cookies. Verify with:                               #
#   curl -sI -H 'Cookie: woocommerce_items_in_cart=1' https://SITE/       #
#   curl -s  -H 'Cookie: woocommerce_items_in_cart=1' https://SITE/ \     #
#     | grep -c 'optimized by WP Rocket'                                  #
# A working bypass has no ETag AND no WP Rocket footprint.                #
#                                                                         #
# Use this only where cart state is rendered server-side on cacheable     #
# pages. If the cart repaints client-side, prefer wp-rocket: this costs   #
# a full PHP render per page view for every shopper who has added         #
# something, which on a busy shop is most of the traffic that matters.    #
#=========================================================================#
`
}

// CartBypass generates wp-rocket-cartbypass from wp-rocket: one anchored
// substitution on the bypass line, verified three ways. The checks match
// the `if ($http_cookie …)` line, never the file — wp-rocket's comments name
// both cookies while explaining their absence.
func CartBypass(base, ext string) (string, error) {
	if cartLine.MatchString(base) {
		return "", fmt.Errorf("wp-rocket.%s already bypasses on cart cookies — the templates would be identical", ext)
	}
	out := header(ext) + strings.Replace(base, "comment_author_)", "comment_author_|woocommerce_items_in_cart|woocommerce_cart_hash)", -1)
	switch {
	case !cartLine.MatchString(out):
		return "", fmt.Errorf("wp-rocket-cartbypass.%s: the substitution did not apply (bypass line changed)", ext)
	case !loggedInLine.MatchString(out):
		return "", fmt.Errorf("wp-rocket-cartbypass.%s: bypass line mangled — logged-in users would be served cache", ext)
	}
	return out, nil
}
