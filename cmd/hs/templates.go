package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/valolink/hestiascripts/internal/conf"
	"github.com/valolink/hestiascripts/internal/hestia"
	"github.com/valolink/hestiascripts/internal/sys"
	"github.com/valolink/hestiascripts/internal/webtpl"
)

const cacheHeaders = "/etc/nginx/conf.d/valolink-cache-headers.conf"

// cmdTemplates: `hs templates install` — wp-secure, wp-rocket and the
// generated cartbypass into Hestia's proxy templates, wp-secure into the
// Apache templates Hestia actually reads, each verified before writing.
func cmdTemplates(repo string, args []string) int {
	if len(args) != 1 || args[0] != "install" {
		fmt.Fprintln(os.Stderr, "usage: hs templates install")
		return 2
	}
	if repo == "" {
		fmt.Fprintln(os.Stderr, "hs templates: the hestiascripts checkout is not found (HS_REPO_DIR)")
		return 1
	}
	// Ordering is load-bearing: the templates use $vl_asset_expires, which
	// the cache-headers drop-in declares. A domain rebuilt onto them without
	// it leaves nginx unable to start.
	if b, err := os.ReadFile(cacheHeaders); err != nil || !strings.Contains(string(b), "vl_asset_expires") {
		fmt.Fprintln(os.Stderr, "hs templates: "+cacheHeaders+" is missing or does not declare $vl_asset_expires —\n"+
			"install it first (bash setup/install-cache-headers.sh); writing templates that reference it would leave nginx unable to reload")
		return 1
	}
	read := func(p string) (string, bool) {
		b, err := os.ReadFile(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ %s: %v\n", p, err)
			return "", false
		}
		return string(b), true
	}
	failed := 0
	put := func(dst, content string) {
		bak, err := conf.Write(dst, content, 0o644)
		switch {
		case err != nil:
			fmt.Printf("✗ %s: %v\n", dst, err)
			failed++
		case bak != "":
			fmt.Printf("✓ %s (previous kept: %s)\n", dst, bak)
		default:
			fmt.Printf("✓ %s\n", dst)
		}
	}
	snippet, ok1 := read(filepath.Join(repo, "templates/nginx/wp-secure-snippet.conf"))
	if !ok1 {
		return 1
	}
	for _, ext := range []string{"tpl", "stpl"} {
		def, ok := read(hestia.NginxTpl + "/default." + ext)
		if !ok {
			return 1
		}
		if out, err := webtpl.WPSecureNginx(def, snippet, ext); err != nil {
			fmt.Println("✗ " + err.Error())
			failed++
		} else {
			put(hestia.NginxTpl+"/wp-secure."+ext, out)
		}
		rocket, ok := read(filepath.Join(repo, "templates/nginx/wp-rocket."+ext))
		if !ok {
			return 1
		}
		put(hestia.NginxTpl+"/wp-rocket."+ext, rocket)
		if out, err := webtpl.CartBypass(rocket, ext); err != nil {
			fmt.Println("✗ " + err.Error())
			failed++
		} else {
			put(hestia.NginxTpl+"/wp-rocket-cartbypass."+ext, out)
		}
	}
	// Apache: Hestia reads $WEBTPL/apache2/$WEB_BACKEND (apache2/php-fpm on
	// every php-fpm box) — run.sh wrote apache2/, which those boxes never read.
	c := hestia.Conf(sys.Real{})
	if c["WEB_SYSTEM"] == "apache2" {
		dir := filepath.Join(hestia.Root, "data/templates/web/apache2", c["WEB_BACKEND"])
		asn, ok := read(filepath.Join(repo, "templates/apache2/wp-secure-snippet.conf"))
		for _, ext := range []string{"tpl", "stpl"} {
			def, ok2 := read(dir + "/default." + ext)
			if !ok || !ok2 {
				failed++
				continue
			}
			if out, err := webtpl.WPSecureApache(def, asn, ext); err != nil {
				fmt.Println("✗ " + err.Error())
				failed++
			} else {
				put(dir+"/wp-secure."+ext, out)
			}
		}
	}
	fmt.Println("\nA template changes nothing until a domain using it is rebuilt. Domains on hs templates:")
	n := 0
	for _, d := range hestia.WebDomains(sys.Real{}) {
		if strings.HasPrefix(d.Proxy, "wp-") || strings.HasPrefix(d.Tpl, "wp-") {
			fmt.Printf("  %s   (proxy %s, web %s)   → Web server → Rebuild domains on hs templates\n", d.Name, d.Proxy, d.Tpl)
			n++
		}
	}
	if n == 0 {
		fmt.Println("  none yet")
	}
	fmt.Println("\nTest on a staging domain first: these templates change how missing files are answered.")
	if failed > 0 {
		fmt.Printf("\n%d template(s) NOT written — do not assign them until fixed.\n", failed)
		return 1
	}
	return 0
}
