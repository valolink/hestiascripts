package op

// Monitoring: run.sh's WP-CLI (2) and Netdata (6) menus. Netdata's "open
// port 19999" is not carried over — the streamer proxies Netdata, and the
// netdata check's fix removes such a rule.

import (
	"context"
	"fmt"
	"strings"

	"github.com/valolink/hestiascripts/internal/check"
	"github.com/valolink/hestiascripts/internal/check/box"
	"github.com/valolink/hestiascripts/internal/conf"
)

const wpPhar = "https://raw.githubusercontent.com/wp-cli/builds/gh-pages/phar/wp-cli.phar"

// cliBlocked lists functions WP-CLI needs that a version's CLI php.ini disables.
func cliBlocked(env *check.Env, v string) []string {
	df, _ := conf.Get(readFile(env, "/etc/php/"+v+"/cli/php.ini"), conf.Opts{}, "disable_functions")
	var out []string
	for _, f := range strings.Split(df, ",") {
		switch f = strings.TrimSpace(f); f {
		case "exec", "system", "passthru", "shell_exec", "proc_open", "popen":
			out = append(out, f)
		}
	}
	return out
}

func init() {
	register(Op{
		ID: "wpcli", Resolves: "wpcli", Applies: summaryHas("not installed"), Title: "Install / update WP-CLI", Section: "monitoring", Risk: Change,
		Note:    "Installs the current WP-CLI phar (or updates an existing one) and the wp-rocket-cli package the cache flush uses.",
		How:     "Fresh install: curl downloads the phar to /tmp, `php … --version` proves it runs before it replaces anything, `install -m 755` puts it at /usr/local/bin/wp. Existing install: `wp cli update --yes` (keeps a backup of the old phar itself). `wp package install wp-media/wp-rocket-cli` adds `wp rocket clean`, which v-wp-cache-flush and staging use — only when missing.",
		Undo:    "Remove /usr/local/bin/wp (the v-wp-* scripts and site actions stop working).",
		Recheck: []string{"wpcli"},
		Plan: func(ctx context.Context, env *check.Env, _ Target, _ Values) ([]Step, error) {
			var steps []Step
			if !have(env, "wp") {
				steps = append(steps,
					Step{Why: "download the current release", Argv: []string{"curl", "-fsSL", "-o", "/tmp/wp-cli.phar", wpPhar}},
					Step{Why: "it runs before it is installed", Argv: []string{"php", "/tmp/wp-cli.phar", "--version", "--allow-root"}},
					Step{Why: "install", Argv: []string{"install", "-m", "755", "/tmp/wp-cli.phar", "/usr/local/bin/wp"}},
					Step{Argv: []string{"rm", "-f", "/tmp/wp-cli.phar"}})
			} else {
				steps = append(steps, Step{Why: "update in place", Argv: []string{"wp", "cli", "update", "--yes", "--allow-root"}})
			}
			out, _ := env.Sys.Run(ctx, "wp", "package", "list", "--fields=name", "--format=csv", "--allow-root")
			if !strings.Contains(out, "wp-rocket-cli") {
				steps = append(steps, Step{Why: "`wp rocket clean` for cache flushes", Argv: []string{"wp", "package", "install", "wp-media/wp-rocket-cli:trunk", "--allow-root"}})
			}
			return append(steps, Step{Why: "check", Argv: []string{"wp", "--version", "--allow-root"}}), nil
		},
	})
	register(Op{
		ID: "php-cli-functions", Resolves: "wpcli", Applies: summaryHas("fails"), Title: "Let PHP CLI run programs (WP-CLI)", Section: "monitoring", Risk: Change,
		Note:    "WP-CLI needs proc_open and exec in the command-line PHP. Sets the CLI php.ini's disable_functions to Hestia's own CLI value (the pcntl_* list only); the PHP that serves websites keeps exec disabled.",
		How:     "For each version whose /etc/php/<ver>/cli/php.ini disables exec/proc_open/…, `hs conf set` replaces the disable_functions line with the value Hestia's installer writes for the CLI (install/upgrade/manual/secure_php.sh — the script run.sh called). Only cli/php.ini is touched; fpm/php.ini is not. Prints before → after and keeps the previous file.",
		Undo:    "cp -p the kept php.ini back (paths printed).",
		Recheck: []string{"wpcli"},
		Plan: func(_ context.Context, env *check.Env, _ Target, _ Values) ([]Step, error) {
			var steps []Step
			for _, v := range box.PHPVersions(env.Sys) {
				if len(cliBlocked(env, v)) > 0 {
					steps = append(steps, confSet("PHP "+v+" CLI blocks "+strings.Join(cliBlocked(env, v), ", "), "/etc/php/"+v+"/cli/php.ini", conf.Opts{Style: conf.INI, Comment: ";"}, "disable_functions", fpmCLIDisabled))
				}
			}
			if len(steps) > 0 && have(env, "wp") {
				steps = append(steps, Step{Why: "check", Argv: []string{"wp", "--version", "--allow-root"}})
			}
			return steps, nil
		},
	})

	netdataTune := func(env *check.Env) ([]Step, error) {
		src := env.RepoDir + "/templates/netdata/netdata.conf"
		if env.RepoDir == "" || !exists(env, src) {
			return nil, fmt.Errorf("templates/netdata/netdata.conf not found — is the hestiascripts checkout next to hs?")
		}
		return []Step{
			confInstall("the low-footprint profile (previous file kept)", src, "/etc/netdata/netdata.conf"),
			{Why: "apply", Argv: []string{"systemctl", "restart", "netdata"}},
			{Why: "running, and the alarms API EngineLink polls answers", Argv: []string{"sh", "-c", "sleep 3; systemctl is-active netdata && curl -fsS -o /dev/null -w 'alarms API: HTTP %{http_code}\\n' http://127.0.0.1:19999/api/v1/alarms"}},
		}, nil
	}
	const tuneHow = "`hs conf install` writes templates/netdata/netdata.conf over /etc/netdata/netdata.conf, printing the lines that change and keeping the previous file under /var/lib/hs/backups: collection every 2 s instead of 1, machine-learning anomaly training off (its periodic CPU/RAM spikes caused the web1 OOM, 2026-07-06), one storage tier, 32 MB page cache, 512 MB on disk. The health engine stays on — EngineLink's alarm polling reads it. Netdata restarts (a few seconds of metrics missing)."
	register(Op{
		ID: "netdata-install", Resolves: "netdata", Applies: summaryHas("not installed"), Title: "Install Netdata", Section: "monitoring", Risk: Change,
		Note:    "Netdata's official kickstart installer, non-interactive, then the low-footprint profile. Port 19999 stays closed to the outside — EngineLink reads Netdata through the streamer.",
		How:     "wget fetches get.netdata.cloud/kickstart.sh; `sh kickstart.sh --non-interactive` installs Netdata's packages and starts the service. " + tuneHow,
		Undo:    "/usr/libexec/netdata/netdata-uninstaller.sh --yes (or apt purge netdata for a native package install).",
		Recheck: []string{"netdata"},
		Plan: func(_ context.Context, env *check.Env, _ Target, _ Values) ([]Step, error) {
			if have(env, "netdata") {
				return nil, fmt.Errorf("Netdata is already installed — use Netdata low-footprint profile")
			}
			tune, err := netdataTune(env)
			if err != nil {
				return nil, err
			}
			return append([]Step{
				{Why: "the official installer", Argv: []string{"wget", "-q", "-O", "/tmp/netdata-kickstart.sh", "https://get.netdata.cloud/kickstart.sh"}},
				{Why: "install without prompts", Argv: []string{"sh", "/tmp/netdata-kickstart.sh", "--non-interactive"}},
				{Argv: []string{"rm", "-f", "/tmp/netdata-kickstart.sh"}},
			}, tune...), nil
		},
	})
	register(Op{
		ID: "netdata-tune", Resolves: "netdata", Applies: summaryHas("stock profile"), Title: "Netdata low-footprint profile", Section: "monitoring", Risk: Change,
		Note:    "Halves collection frequency, turns ML off and caps memory; keeps health alarms on.",
		How:     tuneHow,
		Undo:    "cp -p the kept netdata.conf back (path printed) and restart netdata.",
		Recheck: []string{"netdata"},
		Plan: func(_ context.Context, env *check.Env, _ Target, _ Values) ([]Step, error) {
			if !have(env, "netdata") {
				return nil, fmt.Errorf("Netdata is not installed")
			}
			if box.NetdataTuned(env) && readFile(env, "/etc/netdata/netdata.conf") == readFile(env, env.RepoDir+"/templates/netdata/netdata.conf") {
				return nil, nil
			}
			return netdataTune(env)
		},
	})
}
