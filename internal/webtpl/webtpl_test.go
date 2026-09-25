package webtpl

import (
	"os"
	"strings"
	"testing"
)

// The Go port must produce byte-for-byte what setup/nginx-templates.sh's awk
// produced from Hestia's own default templates (HS_TPL_GOLDEN points at a
// directory with old-secure.{tpl,stpl} / old-apache.{tpl,stpl} made by the
// awk, and HS_HESTIA_TPL at hestiacp/install/deb/templates/web).
func TestMatchesTheShellInjection(t *testing.T) {
	gold, hst := os.Getenv("HS_TPL_GOLDEN"), os.Getenv("HS_HESTIA_TPL")
	if gold == "" || hst == "" {
		t.Skip("golden files not provided")
	}
	sn, _ := os.ReadFile("../../templates/nginx/wp-secure-snippet.conf")
	asn, _ := os.ReadFile("../../templates/apache2/wp-secure-snippet.conf")
	for _, ext := range []string{"tpl", "stpl"} {
		def, _ := os.ReadFile(hst + "/nginx/default." + ext)
		got, err := WPSecureNginx(string(def), string(sn), ext)
		want, _ := os.ReadFile(gold + "/old-secure." + ext)
		if err != nil || got != string(want) {
			t.Errorf("nginx %s differs from the shell version (%v)", ext, err)
		}
		adef, _ := os.ReadFile(hst + "/apache2/php-fpm/default." + ext)
		got, err = WPSecureApache(string(adef), string(asn), ext)
		if ext == "stpl" {
			// The HTTPS template opens <Directory %sdocroot%>; the shell's
			// pattern matched only %docroot%, so it never got the rules.
			if err != nil || !strings.Contains(got, "<Directory %sdocroot%>\n"+strings.SplitN(string(asn), "\n", 2)[0]) {
				t.Errorf("apache stpl: rules not inside <Directory %%sdocroot%%> (%v)", err)
			}
			continue
		}
		want, _ = os.ReadFile(gold + "/old-apache." + ext)
		if err != nil || got != string(want) {
			t.Errorf("apache %s differs from the shell version (%v)", ext, err)
		}
	}
}

func TestInjectionRefusesAnUnknownLayout(t *testing.T) {
	if _, err := WPSecureNginx("server {\n    root /x;\n}\n", "# --- START: Valolink security rules ---\n", "tpl"); err == nil {
		t.Error("no location / — must refuse, not write a rule-less template")
	}
}

func TestCartBypass(t *testing.T) {
	base, err := os.ReadFile("../../templates/nginx/wp-rocket.tpl")
	if err != nil {
		t.Fatal(err)
	}
	out, err := CartBypass(string(base), "tpl")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"(wordpress_logged_in_|wp-postpass_|comment_author_|woocommerce_items_in_cart|woocommerce_cart_hash)"`) {
		t.Error("bypass line not extended")
	}
	if _, err := CartBypass(out, "tpl"); err == nil {
		t.Error("a source that already bypasses must be refused")
	}
}
