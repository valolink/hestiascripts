# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

WordPress automation and streaming toolkit for **HestiaCP** (Hestia Control Panel). Primarily targets WordPress sites hosted on HestiaCP (Debian 12). Two main components:

1. **hestia-streamer** — Go HTTP server that streams custom `v-script` output in real time via Server-Sent Events (SSE). HestiaCP's own API only responds after a script finishes, so this streamer exists to surface progress output while the script is still running.
2. **Custom `v-scripts`** — Bash scripts placed in `/usr/local/hestia/bin/` that extend HestiaCP with WordPress-specific operations (cloning, info gathering, updates, etc.)

## Repo structure

```
run.sh               # Server setup / maintenance entry point (run as root)
install-scripts.sh     # Deploys v-scripts + hestia-streamer (also callable from run.sh)
setup/                 # Modules sourced by run.sh
  common.sh            # Shared utilities (colors, status_line, confirm, run_action, helpers)
  status.sh            # Status dashboard printed on every launch
  wpcli.sh / redis.sh / fail2ban.sh / maldet.sh / netdata.sh / security.sh
  php-fpm.sh / opcache.sh / mariadb.sh / nginx-templates.sh / maintenance.sh / disk.sh / smtp.sh
templates/
  nginx/
    wp-rocket.tpl/.stpl      # WP Rocket cache-proxy templates (complete, version-controlled)
    wp-secure-snippet.conf   # Security rules injected into HestiaCP default nginx template
  php-fpm/
    production.conf / standard.conf / staging.conf / small.conf  # pm settings per profile
```

## Build & Run

```bash
# Build static Linux binary (target: Debian 12, amd64)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o hestia-streamer main.go

# Server setup / maintenance (interactive menu)
sudo bash run.sh

# Deploy v-scripts + streamer only
sudo bash install-scripts.sh

# Restart the service after changes
systemctl restart hestia-streamer

# Test the SSE endpoint
curl "http://localhost:8091/execute?script=v-wp-test-stream"
```

The streamer runs on port **8091**. The `/execute` endpoint accepts `?script=SCRIPT_NAME&arg=ARG1&arg=ARG2`. The `/netdata/alarms` endpoint proxies the box-local Netdata raised-alarms API (see below).

## Architecture

### hestia-streamer (`main.go`)

Two-endpoint Go HTTP server. Both share the optional `X-Streamer-Token` gate (constant-time compare, disabled when `HESTIA_STREAMER_TOKEN` is unset).

**`/netdata/alarms`** — plain JSON passthrough (not SSE) to box-local Netdata `http://127.0.0.1:19999/api/v1/alarms` (8s timeout, 502 on dial failure). Lets EngineLink's `poll-server-alarms` cron read raised alarms over the same token-authed channel it already uses for `/execute`, so Netdata (:19999) never needs to be reachable from the Nuxt host. If a future Netdata build drops the v1 alarms API, `NetdataAlarmsURL` is the single line to update.

**`/netdata/data`** — read-only proxy to Netdata's `/api/v1/data` time-series API so EngineLink renders native charts (CPU/RAM/load/disk) instead of iframing :19999 into the browser. The upstream query is **rebuilt from validated params** (`chart` `^[a-zA-Z0-9_.]+$`, `after` negative-int window, `points` 1–2000, `group` average/max/min/sum, `dimensions` safe chars) — never a forwarded raw path, so a caller can only ever reach `/api/v1/data` with safe values. With both passthroughs in place, **:19999 can be firewalled off from everything but localhost** — EngineLink reaches a box only through this streamer (:8091, token-authed).

**`/execute`** — SSE script runner. Flow:
- Validates script name with `^v-[a-zA-Z0-9-]+$` regex — must start with `v-`
- Resolves script path under `/usr/local/hestia/bin/` using `filepath.Join()` (prevents directory traversal)
- Runs script directly via `exec.Command()` — no shell invocation (prevents injection)
- Streams stdout line-by-line as SSE events using `http.Flusher`
- Ends every run with a named SSE event `event: exit` carrying the script's exit code (consumers use it to distinguish success from failure — EngineLink auto-logs maintenance events / stores enrollment keys only on exit 0)
- Optional shared-secret auth: when the `HESTIA_STREAMER_TOKEN` env is set (systemd loads `/etc/hestia-streamer.env`, generated + printed by `install-scripts.sh`), requests must carry a matching `X-Streamer-Token` header or get a 403 (constant-time compare). Unset = auth disabled, for pre-token installs.
- Sets `X-Accel-Buffering: no` to disable nginx proxy buffering

### Custom v-scripts

All scripts follow the `v-wp-<noun>-<verb>` naming convention and are symlinked into `/usr/local/hestia/bin/` by `install-scripts.sh`. Current scripts:

- **`v-wp-clone-site.sh`** — clones a WordPress site. Accepts flags for non-interactive use (e.g. via hestia-streamer) or runs fully interactively over SSH when flags are omitted:
  - `--src-user=<user>` — source HestiaCP user
  - `--src-domain=<domain>` — domain to clone
  - `--dest-user=<user>` — destination HestiaCP user (must already exist; creating new users is interactive-only)
  - `--new-domain=<domain>` — new domain name
  - `--force` — skip overwrite confirmation if the destination domain already exists (required when running non-interactively; without it the script exits with an error rather than hanging)

  Any omitted flag falls back to an interactive prompt. Steps: create domain + conditional SSL → export DB via WP-CLI → rsync files (excludes cache/backups) → create DB → update `wp-config.php` with new credentials, fresh salts, and unique Redis cache prefix → import DB → search-replace URLs and absolute paths → flush object cache. Supports cross-user cloning; handles overwrite mode (reuses existing DB if connectable); DB credentials generated with `openssl rand`.
- **`v-wp-staging-create.sh`** — creates a staging copy of a WordPress site, or tears one down with `--teardown`. Flags: `--src-user` `--src-domain` `--dest-user` `--new-domain` `--force` `--teardown`. **Strict-isolation build:** the staging site is assembled in `public_html.setup` and only swapped into `public_html` after wp-config, DB import, search-replace, and a pre-publish verification all succeed — so the staging URL returns 404 until everything is correct (closes the traffic window where stock-rsynced live wp-config would otherwise execute under live's Redis prefix). After publish, live uploads are bind-mounted read-only into the staging site and a write-probe as root confirms the mount is actually RO before declaring success (OS-level enforcement, survives any WP/plugin write attempt, but does not survive a server reboot). Forces `WP_HOME` / `WP_SITEURL` as constants on staging so live-inherited values can't bleed through; sets `WP_ENVIRONMENT_TYPE=staging`, `DISABLE_WP_CRON=true`, `DISALLOW_FILE_MODS=true`. `WP_REDIS_PREFIX` and `WP_CACHE_KEY_SALT` use `${domain//./_}_${random}_` with a random component to prevent rebuild-collisions. Search-replace covers all four URL variants (http/https × www/no-www) with `--skip-columns=guid`. At the end, reloads every installed `php*-fpm` service (drops stale opcache'd wp-config in FPM workers) and runs `wp cache flush`, `wp transient delete --all`, and `wp rocket clean --confirm` (if Rocket active) on **both** staging and live — the live flush evicts anything a prior traffic-window/opcache bug may have poisoned. Staging URL memory lives in `/root/.hestia-staging-urls/{user}_{domain}` (sidecar, root-owned 600) — **not** in wp-config — so no plugin on live can ever read it as a magic constant. On each run, any legacy `WP_STAGING_URL` constant in live wp-config is auto-deleted and live FPM is reloaded to migrate older installs. EXIT trap on failure: unmounts the setup-dir bind-mount, removes `public_html.setup`, prints exact teardown + retry commands. Teardown (`--teardown`) unmounts uploads, removes the domain and database, deletes the sidecar mapping, and (if `--src-user` and `--src-domain` are passed) also flushes live caches. SSL is skipped on domain creation — staging domains (e.g. customer.demolink.fi) are expected to handle their own certificates.
- **`v-wp-update.sh`** — runs all available WordPress updates. Flags: `--user=<hestia_user>` `--domain=<domain>` `--skip-backup` `--dry-run`. Backs up the database (gzipped, to `/home/$USER/backup/`) before touching anything unless `--skip-backup` is set. Enables maintenance mode for the duration, updates core → runs DB upgrade → updates all plugins → updates all themes → flushes cache, then disables maintenance mode. A `trap` ensures maintenance mode is always disabled even if the script errors out. `--dry-run` shows what would be updated without applying anything. Exits cleanly with no changes if everything is already up to date.
- **`v-wp-info.sh`** — prints a structured site report. Flags: `--user=<hestia_user>` `--domain=<domain>`. Sections: site identity, update status (core/plugins/themes), active theme (child vs standalone), PHP environment, database size/prefix/version, security posture (WP_DEBUG, file editing, default admin username), admin user list, object cache drop-in, active plugin list with update markers, content counts.
- **`v-wp-migrate-site.sh`** — finish importing a WordPress site rsynced from another host. Flags: `--user` `--domain` `--sql=PATH` `--old-url=URL` `--force`. Pre-conditions: user and domain already created in HestiaCP, files rsynced to public_html, DB exported as migrate.sql in public_html. Steps: fix permissions → create DB → update wp-config (credentials, salts, Redis prefix, WP_DEBUG off, WP_HOME/WP_SITEURL) → import SQL → search-replace old URL → flush caches → delete migrate.sql.
- **`v-wp-redis-install.sh`** — installs the Redis Cache plugin for a domain, sets a unique `WP_REDIS_PREFIX` in wp-config, and activates the plugin. Does not enable the object cache drop-in (that is a separate step). Flags: `--user` `--domain`.
- **`v-wp-fix-permissions.sh`** — fixes file ownership (`chown -R user:user`) and permissions (dirs 755, files 644, wp-config 640) for a domain. Flags: `--user` `--domain`.
- **`v-wp-revisions-clean.sh`** — trims post revisions via WP-CLI. Flags: `--user` `--domain` `--keep=N` `--dry-run`.
- **`v-wp-config.sh`** — views and manages wp-config.php defines (shows current values, lets you add or update common constants). Interactive after the initial listing — an SSH tool, not suited to the streamer. Flags: `--user` `--domain`.
- **`v-wp-valolink-plugin-install.sh`** — installs (or upgrades) the latest published release of `valolink/valolink-plugin` from GitHub into a chosen site. Resolves the release via `https://api.github.com/repos/valolink/valolink-plugin/releases/latest`, prefers a `.zip` release asset, falls back to the auto-generated source zipball if none is published. Installs via `wp plugin install URL --force` (idempotent / overwrite-existing) and `--activate` by default. Flags: `--user` `--domain` `--no-activate` `--print-key`. `--print-key` ensures the EngineLink API key exists in the `valolink_settings` option (generating `bin2hex(random_bytes(24))` if missing, mirroring the plugin's own `ensure_api_key()`) and prints `ENGINELINK_API_KEY=<key>` — EngineLink captures it for zero-touch enrollment; treat the output as secret. Aborts on no release found, network failure, or GitHub rate-limiting (mentions the option to authenticate if hit).
- **`v-wp-test-stream.sh`** — simulates a long-running task for testing the SSE stream.
- **`v-server-health.sh`** — one-shot server health snapshot for EngineLink's daily Layer-2 monitoring (not WordPress-specific). Prints a single-line JSON object as its **final** stdout line: `backups` (newest backup age in hours per HestiaCP user), `services` (`systemctl is-active` for nginx/hestia/mariadb-or-mysql/first php-fpm), and `mailQueue` (postfix `mailq` count, exim fallback). Backup freshness is backend-aware for the restic rollout: it uses `restic snapshots --latest 1` when the user has a per-user key (`/usr/local/hestia/data/users/<u>/restic.conf`) and a system repo is registered (`conf/restic.conf`), else falls back to the legacy tarball (`/backup/<user>.*.tar`) or `/home/<user>/backup`. The restic probes are a network round-trip each, so they run **in parallel** with a per-call `timeout` — EngineLink aborts the whole health fetch at 30s, so wall-clock must stay bounded regardless of user count, and an unreachable Storage Box falls through to the tarball logic rather than blowing the budget. Always exits 0 on a completed run; missing tools degrade to empty/null rather than failing. **Contract:** the JSON must be the last non-empty line — EngineLink parses the final `data:` frame before `event: exit`.

New v-scripts for WordPress operations (info gathering, updates, etc.) should follow the same pattern: print progress line-by-line to stdout, require root, validate inputs early.

### Installation (`install-scripts.sh`)

Symlinks `v-*` bash scripts into `/usr/local/hestia/bin/`, creates a systemd unit for `hestia-streamer`, and opens firewall port 8091 for a specific IP.

### Manual (non-streamer) scripts

Scripts **without** the `v-` prefix are deliberately unreachable by the streamer: `main.go`'s allowlist is `^v-[a-zA-Z0-9-]+$` and `install-scripts.sh` only globs `v-*`, so a non-`v-` script is never symlinked into `/usr/local/hestia/bin/` and can't be invoked over HTTP. This is the access gate for operations too destructive to sit behind a network trigger — the only way to run them is a root shell on the box (deploy is a manual `cp`, not `install-scripts.sh`).

- **`setup-restic-backup.sh`** — one-time onboarding of a box to a Hetzner Storage Box for restic incremental backups (drives Hestia's own `v-backup-user-restic`). Idempotent, guided. Two auth modes: **key** (main account — generates a dedicated per-box ed25519 key at `/root/.ssh/storagebox` and authorizes it via `install-ssh-key`) and **`--password`** (Hetzner **subaccounts**, which don't support `install-ssh-key` — it returns "Internal Error 006" — and use their own `uXXXXX-subN.your-storagebox.de` host). Pins the host key, creates an rclone sftp remote, registers a **per-box** repo with `v-add-backup-host-restic`, then flips **per-user** incremental on (the gotcha below). Storage Box specifics baked in: SSH is port **23**; the SFTP session is **chrooted to `/home`**, so repo paths are forced **relative** (an absolute `/path` lists empty — `--path /` means the home root, default is `hestia-<host>/`); rclone's Go SSH dials IPv6 and won't fall back, so on a refused dial the script pins the host's IPv4 in `/etc/hosts` and retries. **Incremental is two flags, not one:** `v-add-backup-host-restic` sets the *system* `BACKUP_INCREMENTAL=yes` (hestia.conf) but NOT the *per-user* `BACKUPS_INCREMENTAL` (note the plural — a package attribute in each `user.conf`) that `v-backup-user-restic` actually checks; without it every backup dies with "incremental backups are disabled". The script sets it in every `.pkg` (durable default) and every existing `user.conf` (immediate) via surgical `sed`, deliberately avoiding `v-change-user-package` so no other package limits get reset. Prints the must-do follow-ups: verify the DB is in the snapshot, enable Storage Box snapshots (deletion backstop), copy the restic encryption keys off-box (they live on the box being backed up — lose them and the repo is unrecoverable), keep tarball backups until a test restore. Usage (subaccount): `setup-restic-backup.sh --host uXXXXX-sub1.your-storagebox.de --user uXXXXX-sub1 --password`. Not a `v-` script for the same reason as the others — root-only setup, off the HTTP path.
- **`safe-reboot.sh`** — staged, guarded whole-box reboot. Naming it `v-…` would expose "reboot every server" to anyone with the streamer token, so it stays SSH-only by design. Flow: hard guards (root, systemd present, no `dpkg`/`apt`/`unattended-upgrades` mid-run, not already shutting down) → soft guard (refuses if a HestiaCP backup / `mysqldump` is running unless `--force`) → confirmation (type the hostname, or `--yes`) → **commit point** → pre-stop non-web services (netdata/vsftpd/fail2ban/postfix/cron/atd; nginx/php-fpm/DB/redis/named stay up) → Redis `BGSAVE` (auth-aware) → MariaDB/MySQL InnoDB dirty-page pre-flush (client + service auto-detected across `mysql`/`mariadb`/`mysqld` names) → `sync` → reboot (with `systemctl reboot` → `reboot` → `--force` fallbacks). Every bail-out is *before* the commit point, so it can never leave the box with services stopped but not rebooted. Post-confirmation log lines are stamped `+Ns` (elapsed since confirmation) and mirror to `journalctl -t safe-reboot`, so downtime is measurable live and after the fact. The pre-flush is an optimization (shorter shutdown, less crash-recovery) — skipping any stage is safe, systemd still shuts the DB down cleanly. Install: `cp safe-reboot.sh /usr/local/sbin/safe-reboot && chmod 700 /usr/local/sbin/safe-reboot`.

## Dependencies

- **Go** — compile the streamer
- **WP-CLI** — required by clone scripts for DB export/import
- **HestiaCP** — the control panel being automated
- **rsync**, **openssl** — used in clone scripts
- **systemd** — service management
