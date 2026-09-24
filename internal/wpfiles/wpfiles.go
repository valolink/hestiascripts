// Package wpfiles knows which files on a WordPress site are what — shared by
// the checks that find them and the fixes that move them, so both always
// agree on what counts.
//
// Every entry here came from a real box (fleet sweep 2026-09-24).
package wpfiles

import (
	"path"
	"strings"
)

// DumpPatterns is the find(1) expression for database dumps and wp-config
// copies in a web root. ykiveneet.fi had a wp-config-backup.php the first
// list missed.
var DumpPatterns = []string{"(",
	"-name", "wp-config*.bak*", "-o", "-name", "wp-config*.save", "-o", "-name", "wp-config*.old",
	"-o", "-name", "wp-config*backup*", "-o", "-name", "wp-config*.orig", "-o", "-name", "wp-config*.txt",
	"-o", "-name", "*.sql", "-o", "-name", "*.sql.gz", "-o", "-name", "*.sql.zip", "-o", "-name", "*.sql.bz2", "-o", "-name", "*.sql.xz",
	")", "-type", "f"}

// knownUploadsPHP: plugins that legitimately keep PHP under uploads/.
var knownUploadsPHP = []struct{ prefix, plugin string }{
	{"cache/wpml/twig/", "WPML template cache"},                     // ykiveneet.fi: 162 files
	{"sucuri/", "Sucuri data files (exit-guarded)"},                 // aina.demolink.fi
	{"wpallimport/functions.php", "WP All Import custom functions"}, // river*, renea*
	{"wpallimport/uploads/", "WP All Import upload guard"},
}

// KnownUploadsPHP names the plugin behind a PHP file in uploads (rel is
// relative to wp-content/uploads/), or "" when it is not a known case.
func KnownUploadsPHP(rel string) string {
	for _, k := range knownUploadsPHP {
		if strings.HasPrefix(rel, k.prefix) {
			return k.plugin
		}
	}
	return ""
}

// CoreExtra classifies a file `wp core verify-checksums` says should not
// exist. Most are not backdoors; the one class that looks like one gets
// the alarm.
type CoreExtra int

const (
	ExtraSuspicious CoreExtra = iota // a PHP file core does not ship — the backdoor shape (alavus zwfile.php)
	ExtraErrorLog                    // PHP error log written into a core dir — served over the web
	ExtraLeftover                    // non-PHP leftovers: truncated names from a failed update, .rnd, .htaccess, .DS_Store
)

func ClassifyCoreExtra(rel string) CoreExtra {
	base := path.Base(rel)
	switch {
	case base == "error_log" || base == "php_errorlog" || strings.HasSuffix(base, ".log"):
		return ExtraErrorLog
	case hasExt(base, ".php", ".phtml", ".phar", ".inc", ".php5", ".php7", ".phps"):
		return ExtraSuspicious
	}
	return ExtraLeftover
}

func hasExt(name string, exts ...string) bool {
	l := strings.ToLower(name)
	for _, e := range exts {
		if strings.HasSuffix(l, e) {
			return true
		}
	}
	return false
}

// SecurityMarker is the comment every hardened proxy template carries.
const SecurityMarker = "Valolink security rules"
