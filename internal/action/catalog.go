package action

// The catalogue. Every run.sh menu item and every root-shell script is
// reachable from here — that is phase 4's done-criterion.

func siteActions() []Action {
	return []Action{
		{ID: "site.info", Title: "Site report", Site: true, WPOnly: true, Mode: Stream, Confirm: ConfirmNone,
			Note:    "Read-only: versions, updates, PHP, database, security posture, plugins.",
			Command: siteScript("v-wp-info")},
		{ID: "site.update-preview", Title: "Preview WordPress updates", Site: true, WPOnly: true, Mode: Stream, Confirm: ConfirmNone,
			Note:    "Read-only (--dry-run): what core, plugins and themes would update to.",
			Command: siteScript("v-wp-update", "--dry-run")},
		{ID: "site.update", Title: "Update WordPress (core, plugins, themes)", Site: true, WPOnly: true, Mode: Stream, Confirm: ConfirmTyped,
			Note:    "Backs up the database first, maintenance mode on during the run. Plugin updates can break a site — preview first.",
			Recheck: []string{"site.updates", "site.core", "site.http"}, Command: siteScript("v-wp-update")},
		{ID: "site.cache-flush", Title: "Flush all caches", Site: true, WPOnly: true, Mode: Stream, Confirm: ConfirmYes,
			Note:    "Object cache, expired transients, WP Rocket, nginx cache. Safe; the next visits are slower while caches refill.",
			Recheck: []string{"site.http"}, Command: siteScript("v-wp-cache-flush")},
		{ID: "site.fix-permissions", Title: "Fix file ownership and permissions", Site: true, WPOnly: true, Mode: Stream, Confirm: ConfirmYes,
			Note:    "chown -R to the site user; dirs 755, files 644, wp-config 640.",
			Recheck: []string{"site.http"}, Command: siteScript("v-wp-fix-permissions")},
		{ID: "site.revisions-preview", Title: "Preview revision cleanup (keep 10)", Site: true, WPOnly: true, Mode: Stream, Confirm: ConfirmNone,
			Note:    "Read-only (--dry-run): revision counts and what keeping 10 per post would delete.",
			Command: siteScript("v-wp-revisions-clean", "--keep=10", "--dry-run")},
		{ID: "site.revisions-clean", Title: "Clean revisions (keep 10 per post)", Site: true, WPOnly: true, Mode: Stream, Confirm: ConfirmTyped,
			Note:    "Deletes post revisions beyond the newest 10 per post. Not recoverable except from backup.",
			Command: siteScript("v-wp-revisions-clean", "--keep=10")},
		{ID: "site.redis-install", Title: "Install Redis object cache", Site: true, WPOnly: true, Mode: Stream, Confirm: ConfirmYes,
			Note:    "Installs redis-cache, sets a unique prefix + WP_REDIS_DISABLE_GROUP_FLUSH + WP_REDIS_MAXTTL. Enable the drop-in separately.",
			Recheck: []string{"redis.sites"}, Command: siteScript("v-wp-redis-install")},
		{ID: "site.valolink-plugin", Title: "Install / update valolink-plugin", Site: true, WPOnly: true, Mode: Stream, Confirm: ConfirmYes,
			Note:    "Latest GitHub release, activated. Enrolment key is printed only with --print-key (not here).",
			Command: siteScript("v-wp-valolink-plugin-install")},
		{ID: "site.db-export", Title: "Export database", Site: true, WPOnly: true, Mode: Stream, Confirm: ConfirmYes,
			Note: "Writes a gzipped dump under the user's backup directory.", Command: siteScript("v-wp-db-export")},
		{ID: "site.purge-nginx", Title: "Purge nginx cache", Site: true, Mode: Stream, Confirm: ConfirmYes,
			Note: "Stock v-purge-nginx-cache for this domain.",
			Command: func(t Target, _ string) []string {
				return []string{bin + "v-purge-nginx-cache", t.Domain.User, t.Domain.Name}
			}},
		{ID: "site.letsencrypt", Title: "Issue / renew Let's Encrypt certificate", Site: true, Mode: Stream, Confirm: ConfirmYes,
			Note:    "Needs DNS for the domain (and aliases) pointing at this box.",
			Recheck: []string{"web.ssl", "site.http"},
			Command: func(t Target, _ string) []string {
				return []string{bin + "v-add-letsencrypt-domain", t.Domain.User, t.Domain.Name}
			}},
		{ID: "site.wp-config", Title: "Edit wp-config constants", Site: true, WPOnly: true, Mode: Interactive, Confirm: ConfirmNone,
			Note: "Interactive: lists defines, then add or change them.", Recheck: []string{"site.debug", "redis.sites"},
			Command: siteScript("v-wp-config")},
		{ID: "site.clone", Title: "Clone this site…", Site: true, WPOnly: true, Mode: Interactive, Confirm: ConfirmNone,
			Note: "Interactive: asks for the destination user and domain. Creates a new site; the source is untouched.",
			Command: func(t Target, _ string) []string {
				return []string{bin + "v-wp-clone-site", "--src-user=" + t.Domain.User, "--src-domain=" + t.Domain.Name}
			}},
		{ID: "site.staging", Title: "Create staging copy…", Site: true, WPOnly: true, Mode: Interactive, Confirm: ConfirmNone,
			Note: "Interactive: asks for the staging user and domain. Isolated build; live is only cache-flushed.",
			Command: func(t Target, _ string) []string {
				return []string{bin + "v-wp-staging-create", "--src-user=" + t.Domain.User, "--src-domain=" + t.Domain.Name}
			}},
		{ID: "site.restore", Title: "Restore from backup…", Site: true, Mode: Interactive, Confirm: ConfirmNone,
			Note:    "Guided restic restore: pick scope and snapshot, safety copies first, typed confirmation inside. Rehearse with the dry-run.",
			Recheck: []string{"site.http", "site.core"}, NeedsRepo: false,
			Command: vscript("v-restore-restic")},
		{ID: "site.restore-rehearse", Title: "Rehearse a restore (dry-run)…", Site: true, Mode: Interactive, Confirm: ConfirmNone,
			Note:    "Walks the whole restore dialogue with real snapshot lists, then prints the commands instead of running them.",
			Command: vscript("v-restore-restic", "--dry-run")},
	}
}

func boxActions() []Action {
	return []Action{
		// --- backups
		{ID: "backups.guard-preview", Title: "Backup guard — what would it fix", Section: "backups", Mode: Stream, Confirm: ConfirmNone,
			Note: "Read-only (--dry-run): per-user incremental flags, repo path, schedule.", Command: vscript("v-server-backup-guard", "--dry-run")},
		{ID: "backups.guard", Title: "Backup guard — fix config drift", Section: "backups", Mode: Stream, Confirm: ConfirmYes,
			Note: "Re-asserts BACKUPS_INCREMENTAL in every package and user.conf.", Recheck: []string{"backups.setup"},
			Command: vscript("v-server-backup-guard")},
		{ID: "backups.hourly-preview", Title: "Hourly DB snapshot — dry-run", Section: "backups", Mode: Stream, Confirm: ConfirmNone,
			Note: "Read-only: which users and databases the hourly job would dump.", Command: vscript("v-server-backup-db-hourly", "--dry-run")},
		{ID: "backups.restore", Title: "Restore from backup…", Section: "backups", Mode: Interactive, Confirm: ConfirmNone,
			Note: "Guided restic restore (user, site, files, database). Typed confirmation inside.", Command: vscript("v-restore-restic")},
		{ID: "backups.restore-rehearse", Title: "Rehearse a restore (dry-run)…", Section: "backups", Mode: Interactive, Confirm: ConfirmNone,
			Note: "The whole dialogue with real snapshots; prints commands instead of running them.", Command: vscript("v-restore-restic", "--dry-run")},
		{ID: "backups.setup", Title: "Set up restic to a Storage Box…", Section: "backups", Mode: Interactive, Confirm: ConfirmNone, NeedsRepo: true,
			Note:    "Onboarding: host key, rclone remote, repo, per-user incremental, schedule. Asks for the Storage Box details.",
			Recheck: []string{"backups.setup", "backups.nightly"}, Command: repoScript("setup-restic-backup.sh")},
		{ID: "backups.dr-check", Title: "Disaster-recovery script — check repos", Section: "backups", Mode: Stream, Confirm: ConfirmNone, NeedsRepo: true,
			DescribeFrom: "make-restore-script.sh",
			Note:         "Generates the DR script to a root-only temp file and runs its --check: opens every repo with its key. Changes nothing remote.",
			Command: func(_ Target, repo string) []string {
				return []string{"bash", "-c", `set -e; f=$(mktemp /root/hs-dr-XXXXXX.sh); trap 'rm -f "$f"' EXIT; bash ` + quote(repo+"/make-restore-script.sh") + ` --stdout > "$f"; bash "$f" --check`}
			}},
		{ID: "backups.dr-generate", Title: "Disaster-recovery script — generate…", Section: "backups", Mode: Interactive, Confirm: ConfirmYes, NeedsRepo: true,
			Note:    "Writes a script holding every restic key and the Storage Box password. Store it in the vault, not on the box.",
			Command: repoScript("make-restore-script.sh")},

		// --- security
		{ID: "security.fail2ban", Title: "Fail2ban…", Section: "security", Mode: Interactive, NeedsRepo: true,
			Note: "run.sh menu: WordPress jail, restart, jail limits.", Recheck: []string{"fail2ban"}, Command: setupFn("menu_fail2ban")},
		{ID: "security.maldet", Title: "Maldet…", Section: "security", Mode: Interactive, NeedsRepo: true,
			Note: "run.sh menu: install, configure, scan now.", Recheck: []string{"maldet"}, Command: setupFn("menu_maldet")},
		{ID: "security.hardening", Title: "Swap, SSH keys, unattended upgrades…", Section: "security", Mode: Interactive, NeedsRepo: true,
			Note:    "run.sh Security menu. Confirm your SSH key works BEFORE disabling passwords.",
			Recheck: []string{"ssh.auth", "updates.unattended", "memory.swap"}, Command: setupFn("menu_security")},
		{ID: "security.updates-preview", Title: "Preview pending package updates", Section: "security", Mode: Interactive, NeedsRepo: true,
			Note: "Read-only apt simulation, grouped.", Command: setupFn("_maintenance_update_dryrun")},
		{ID: "security.updates", Title: "Update system packages…", Section: "security", Mode: Interactive, NeedsRepo: true,
			Note: "Shows the preview, asks, then apt upgrade.", Recheck: []string{"updates.pending", "updates.reboot"},
			Command: setupFn("_maintenance_system_update")},
		{ID: "security.hestia-update", Title: "Update HestiaCP", Section: "security", Mode: Stream, Confirm: ConfirmYes,
			Note: "v-update-sys-hestia-all.", Recheck: []string{"hestia.version"}, Command: vscript("v-update-sys-hestia-all")},
		{ID: "security.audit", Title: "Legacy audit report (v-server-audit)", Section: "security", Mode: Stream, Confirm: ConfirmNone,
			Note: "The bash audit hs replaces — kept for comparison until phase 5.", Command: vscript("v-server-audit")},

		// --- performance
		{ID: "performance.redis", Title: "Redis…", Section: "performance", Mode: Interactive, NeedsRepo: true,
			Note: "run.sh menu: install, maxmemory/policy, PHP extension.", Recheck: []string{"redis", "redis.sites"}, Command: setupFn("menu_redis")},
		{ID: "performance.php-fpm", Title: "PHP-FPM profiles and versions…", Section: "performance", Mode: Interactive, NeedsRepo: true,
			Note: "run.sh menu: install a profile, show details, add a PHP version.", Recheck: []string{"php.fpm-profiles", "memory.fpm-ceiling"},
			Command: setupFn("menu_php_fpm")},
		{ID: "performance.opcache", Title: "OpCache…", Section: "performance", Mode: Interactive, NeedsRepo: true,
			Note: "Apply recommended settings to a PHP version.", Recheck: []string{"php.opcache"}, Command: setupFn("menu_opcache")},
		{ID: "performance.mariadb", Title: "MariaDB buffer pool…", Section: "performance", Mode: Interactive, NeedsRepo: true,
			Note: "Set innodb_buffer_pool_size.", Recheck: []string{"mariadb.buffer"}, Command: setupFn("menu_mariadb")},
		{ID: "performance.memory", Title: "Memory audit", Section: "performance", Mode: Stream, Confirm: ConfirmNone,
			Note: "Read-only: where the RAM goes and whether it matters (~5 s).", Command: vscript("v-server-memory")},

		// --- web
		{ID: "web.templates", Title: "Install / update nginx templates…", Section: "web", Mode: Interactive, NeedsRepo: true,
			Note:    "wp-secure, wp-rocket, wp-rocket-cartbypass + cache headers. Existing domains keep their template until rebuilt.",
			Recheck: []string{"web.templates", "web.cache-headers", "web.nginx"}, Command: setupFn("_nginx_install_profiles")},
		{ID: "web.cache-headers", Title: "Install / update cache-headers drop-in", Section: "web", Mode: Stream, Confirm: ConfirmYes, NeedsRepo: true,
			Note: "http-level Cache-Control default + versioned asset expiry; nginx reload.", Recheck: []string{"web.cache-headers", "web.nginx"},
			Command: repoScript("setup/install-cache-headers.sh")},

		// --- mail
		{ID: "mail.smtp", Title: "SMTP relay and recipients…", Section: "mail", Mode: Interactive, NeedsRepo: true,
			Note: "run.sh menu: dependencies, relay, notification recipients, test email.", Recheck: []string{"mail.delivery", "mail.queue"},
			Command: setupFn("menu_smtp")},
		{ID: "mail.queue", Title: "Show mail queue", Section: "mail", Mode: Stream, Confirm: ConfirmNone,
			Note: "Read-only: mailq.", Command: func(Target, string) []string { return []string{"mailq"} }},

		// --- monitoring
		{ID: "monitoring.netdata", Title: "Netdata…", Section: "monitoring", Mode: Interactive, NeedsRepo: true,
			Note:    "run.sh menu: install, low-footprint tuning. (Its 'open port 19999' option is obsolete — the streamer proxies.)",
			Recheck: []string{"netdata"}, Command: setupFn("menu_netdata")},
		{ID: "monitoring.deploy", Title: "Deploy v-scripts and streamer", Section: "monitoring", Mode: Stream, Confirm: ConfirmYes, NeedsRepo: true,
			Note: "install-scripts.sh: symlinks, systemd unit, cache headers. Restarts hestia-streamer.", Recheck: []string{"streamer"},
			Command: repoScript("install-scripts.sh")},
		{ID: "monitoring.wpcli", Title: "WP-CLI…", Section: "monitoring", Mode: Interactive, NeedsRepo: true,
			Note: "run.sh menu: install, update, fix disabled PHP CLI functions.", Recheck: []string{"wpcli"}, Command: setupFn("menu_wpcli")},

		// --- system
		{ID: "system.disk", Title: "Disk cleanup…", Section: "system", Mode: Interactive, NeedsRepo: true,
			Note: "run.sh menu: apt cache, journal, WP caches/transients, PHP sessions, logs, ncdu.", Recheck: []string{"disk"},
			Command: setupFn("menu_disk")},
		{ID: "system.services", Title: "Remove unneeded services…", Section: "system", Mode: Interactive, NeedsRepo: true,
			Note: "Dovecot, ClamAV, SpamAssassin, vsftpd — also repairs hestia.conf drift.", Recheck: []string{"hestia.services", "services.idle"},
			Command: setupFn("menu_service_cleanup")},
		{ID: "system.repos", Title: "Check apt repositories", Section: "system", Mode: Interactive, NeedsRepo: true,
			Note: "Read-only: which sources fail to refresh.", Command: setupFn("_maintenance_check_repos")},
		{ID: "system.filemanager", Title: "Apply HestiaCP file manager fix", Section: "system", Mode: Interactive, NeedsRepo: true,
			Command: setupFn("_maintenance_filemanager_fix")},
		{ID: "system.reboot", Title: "Safe reboot…", Section: "system", Mode: Interactive, Confirm: ConfirmTyped, NeedsRepo: true,
			Note:    "Staged, guarded reboot: refuses during apt or backups, pre-flushes the database, asks for the hostname again.",
			Command: repoScript("safe-reboot.sh")},
	}
}

// All returns every action.
func All() []Action { return append(siteActions(), boxActions()...) }

func ForSection(section string) []Action {
	var out []Action
	for _, a := range boxActions() {
		if a.Section == section {
			out = append(out, a)
		}
	}
	return out
}

// ForSite returns the actions for a site; WordPress-only ones only when wp.
func ForSite(wp bool) []Action {
	var out []Action
	for _, a := range siteActions() {
		if !a.WPOnly || wp {
			out = append(out, a)
		}
	}
	return out
}

func ByID(id string) (Action, bool) {
	for _, a := range All() {
		if a.ID == id {
			return a, true
		}
	}
	return Action{}, false
}

// ForCheck maps a check to the action that fixes it — the detail pane's
// "enter to fix". Only where one action is the obvious fix.
var ForCheck = map[string]string{
	"site.updates":       "site.update-preview",
	"site.debug":         "site.wp-config",
	"fail2ban":           "security.fail2ban",
	"maldet":             "security.maldet",
	"ssh.auth":           "security.hardening",
	"updates.unattended": "security.hardening",
	"memory.swap":        "security.hardening",
	"updates.pending":    "security.updates-preview",
	"hestia.version":     "security.hestia-update",
	"backups.setup":      "backups.guard-preview",
	"redis":              "performance.redis",
	"redis.sites":        "performance.redis",
	"php.opcache":        "performance.opcache",
	"php.fpm-profiles":   "performance.php-fpm",
	"memory.fpm-ceiling": "performance.php-fpm",
	"mariadb.buffer":     "performance.mariadb",
	"web.templates":      "web.templates",
	"web.cache-headers":  "web.cache-headers",
	"mail.delivery":      "mail.smtp",
	"mail.queue":         "mail.queue",
	"netdata":            "monitoring.netdata",
	"streamer":           "monitoring.deploy",
	"wpcli":              "monitoring.wpcli",
	"disk":               "system.disk",
	"hestia.services":    "system.services",
	"services.idle":      "system.services",
	"updates.reboot":     "system.reboot",
}
