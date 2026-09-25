package box

// Box hardening checks from docs/security-hardening.md §7, §10, §11, §14.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	. "github.com/valolink/hestiascripts/internal/check"
)

func hardeningChecks() []Check {
	return []Check{
		{ID: "logs.retention", Section: "security", Title: "Log retention", Run: checkLogRetention},
		{ID: "audit.trail", Section: "security", Title: "Audit trail (auditd)", Run: checkAudit},
		{ID: "tmp.noexec", Section: "security", Title: "No programs from /tmp", Run: checkTmpNoexec},
		{ID: "php.functions", Section: "security", Title: "PHP-FPM: shell functions off", Run: checkPHPFunctions},
		{ID: "web.phpmyadmin", Section: "security", Title: "phpMyAdmin exposure", Run: checkPMA},
	}
}

// RetentionWeeks is what the web and Hestia logs should keep (§10).
const RetentionWeeks = 26

// LogrotateFiles are the configs whose rotate count decides how far back
// web and panel logs reach.
var LogrotateFiles = []string{"/etc/logrotate.d/nginx", "/etc/logrotate.d/apache2", "/etc/logrotate.d/hestia"}

func rotateCount(text string) (int, bool) {
	for _, l := range strings.Split(text, "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && f[0] == "rotate" {
			n, err := strconv.Atoi(f[1])
			return n, err == nil
		}
	}
	return 0, false
}

func checkLogRetention(ctx context.Context, env *Env) []Result {
	var short []string
	for _, f := range LogrotateFiles {
		if n, ok := rotateCount(readString(env.Sys, f)); ok && n < RetentionWeeks {
			short = append(short, fmt.Sprintf("%s: rotate %d", f, n))
		}
	}
	jconf := readString(env.Sys, "/etc/systemd/journald.conf.d/hs-retention.conf")
	if !strings.Contains(jconf, "MaxRetentionSec") {
		short = append(short, "journald: no retention set (hs-retention.conf missing) — history is whatever fits the size cap")
	}
	if len(short) == 0 {
		return []Result{New(OK, fmt.Sprintf("web and panel logs kept %d rotations, journal retention set", RetentionWeeks))}
	}
	return []Result{New(Warn, "logs do not reach back far enough").Ev(short...).
		Because("In the May 2026 incident the first shells and downloads could only be reconstructed where the journal happened to reach back; web logs rotated away after five weeks.").
		Fixed("hs op log-retention")}
}

func checkAudit(ctx context.Context, env *Env) []Result {
	if !env.Sys.Have("auditctl") {
		return []Result{New(Warn, "auditd is not installed").
			Because("Nothing records who changed systemd units, cron, authorized_keys or accounts, or what ran from /tmp — bash history only half-answered that in May 2026.").
			Fixed("hs op audit")}
	}
	out, err := env.Sys.Run(ctx, "auditctl", "-l")
	if err != nil || !strings.Contains(out, "hs_") {
		return []Result{New(Warn, "auditd without the hs rules").Ev(strings.TrimSpace(out)).Fixed("hs op audit")}
	}
	return []Result{New(OK, fmt.Sprintf("%d hs audit rules loaded", strings.Count(out, "hs_")))}
}

func mountOpts(ctx context.Context, env *Env, target string) (fstype, opts string, own bool) {
	out, err := env.Sys.Run(ctx, "findmnt", "-no", "TARGET,FSTYPE,OPTIONS", target)
	f := strings.Fields(out)
	if err != nil || len(f) < 3 || f[0] != target {
		return "", "", false
	}
	return f[1], f[2], true
}

func hasOpt(opts, o string) bool {
	for _, x := range strings.Split(opts, ",") {
		if x == o {
			return true
		}
	}
	return false
}

func checkTmpNoexec(ctx context.Context, env *Env) []Result {
	var loose []string
	for _, t := range []string{"/tmp", "/dev/shm"} {
		_, opts, own := mountOpts(ctx, env, t)
		switch {
		case !own:
			loose = append(loose, t+": part of the root filesystem (executable)")
		case !hasOpt(opts, "noexec"):
			loose = append(loose, t+": mounted "+opts)
		}
	}
	if len(loose) == 0 {
		return []Result{New(OK, "/tmp and /dev/shm mounted noexec")}
	}
	return []Result{New(Warn, "programs can run from /tmp or /dev/shm").Ev(loose...).
		Because("The May 2026 tooling ran /tmp/a.elf and /tmp/rg; automated tooling fails on a noexec /tmp.").
		Fixed("hs op tmp-noexec")}
}

// ShellFunctions: what PHP-FPM should not offer to site code — a webshell
// loses most of its use without them.
var ShellFunctions = []string{"exec", "system", "passthru", "shell_exec", "proc_open", "popen", "pcntl_exec"}

func checkPHPFunctions(ctx context.Context, env *Env) []Result {
	var rs []Result
	for _, v := range PHPVersions(env.Sys) {
		ini := readString(env.Sys, "/etc/php/"+v+"/fpm/php.ini")
		if ini == "" {
			continue
		}
		df := ""
		for _, l := range strings.Split(ini, "\n") {
			if k, val, ok := strings.Cut(l, "="); ok && strings.TrimSpace(k) == "disable_functions" {
				df = val
			}
		}
		set := map[string]bool{}
		for _, f := range strings.Split(df, ",") {
			set[strings.TrimSpace(f)] = true
		}
		var open []string
		for _, f := range ShellFunctions {
			if !set[f] {
				open = append(open, f)
			}
		}
		if len(open) > 0 {
			rs = append(rs, New(Warn, "PHP "+v+" (FPM) lets site code run shell commands").For("php"+v).Ev("not disabled: "+strings.Join(open, ", ")).
				Because("A webshell or a vulnerable plugin can then run any command as the site user. Hestia disables these on the versions it installs.").
				Fixed("hs op php-functions"))
		}
	}
	if len(rs) == 0 {
		return []Result{New(OK, "shell functions disabled for every PHP-FPM version")}
	}
	return rs
}

// PMAMarker delimits the access rules hs adds to phpMyAdmin's includes.
const PMAMarker = "hs:pma"

func checkPMA(ctx context.Context, env *Env) []Result {
	inc := readString(env.Sys, "/etc/nginx/conf.d/phpmyadmin.inc")
	if inc == "" {
		return []Result{New(NA, "phpMyAdmin is not served by nginx here")}
	}
	if strings.Contains(inc, PMAMarker) {
		return []Result{New(OK, "phpMyAdmin limited to admin networks")}
	}
	return []Result{New(Warn, "phpMyAdmin answers on every site domain, to everyone").
		Ev("/etc/nginx/conf.d/phpmyadmin.inc: location /phpmyadmin with no allow/deny").
		Because("Its login is a database-password prompt anyone can brute-force, on every customer domain (checked 2026-09-25: valolink.fi, rainset.fi, reservationat8ight.fi, alavusikkunat.fi all answered 200).").
		Fixed("hs op pma-restrict").Valid(24 * time.Hour)}
}
