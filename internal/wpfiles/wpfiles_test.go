package wpfiles

import "testing"

func TestClassifyCoreExtra(t *testing.T) {
	cases := map[string]CoreExtra{
		"wp-includes/sodium_compat/namespaced/Core/Curve25519/Ge/zwfile.php": ExtraSuspicious, // alavus backdoor
		"wp-admin/error_log":    ExtraErrorLog,
		"wp-admin/php_errorlog": ExtraErrorLog,
		"wp-admin/.rnd":         ExtraLeftover,
		"wp-includes/.htaccess": ExtraLeftover,
		"wp-includes/php-ai-client/third-party/Http/Discovery/Exception/NoCandidateFoundException.p": ExtraLeftover, // truncated by a failed update
		"wp-config-backup.php": ExtraSuspicious,
	}
	for p, want := range cases {
		if got := ClassifyCoreExtra(p); got != want {
			t.Errorf("%s: %v, want %v", p, got, want)
		}
	}
}

func TestKnownUploadsPHP(t *testing.T) {
	if KnownUploadsPHP("cache/wpml/twig/fb/fb12.php") == "" || KnownUploadsPHP("2024/05/shell.php") != "" {
		t.Error("known/unknown uploads PHP misclassified")
	}
	if KnownUploadsPHP("code-execution.php") == "" || KnownUploadsPHP("code-execution.php.shell.php") != "" || KnownUploadsPHP("wpallexport/functions.php.bak.php") != "" {
		t.Error("a single-file entry must match that file only")
	}
	if KnownUploadsPHP("mailpoet/cache/f4/f4006dfb.php") == "" {
		t.Error("MailPoet cache not known")
	}
}
