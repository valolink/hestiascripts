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

The streamer runs on port **8091**. The `/execute` endpoint accepts `?script=SCRIPT_NAME&arg=ARG1&arg=ARG2`.

## Architecture

### hestia-streamer (`main.go`)

Single-endpoint Go HTTP server (`/execute`). Execution flow:
- Validates script name with `^v-[a-zA-Z0-9-]+$` regex — must start with `v-`
- Resolves script path under `/usr/local/hestia/bin/` using `filepath.Join()` (prevents directory traversal)
- Runs script directly via `exec.Command()` — no shell invocation (prevents injection)
- Streams stdout line-by-line as SSE events using `http.Flusher`
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
- **`v-wp-test-stream.sh`** — simulates a long-running task for testing the SSE stream.

New v-scripts for WordPress operations (info gathering, updates, etc.) should follow the same pattern: print progress line-by-line to stdout, require root, validate inputs early.

### Installation (`install-scripts.sh`)

Symlinks `v-*` bash scripts into `/usr/local/hestia/bin/`, creates a systemd unit for `hestia-streamer`, and opens firewall port 8091 for a specific IP.

## Dependencies

- **Go** — compile the streamer
- **WP-CLI** — required by clone scripts for DB export/import
- **HestiaCP** — the control panel being automated
- **rsync**, **openssl** — used in clone scripts
- **systemd** — service management
