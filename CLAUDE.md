# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

WordPress automation and streaming toolkit for **HestiaCP** (Hestia Control Panel). Primarily targets WordPress sites hosted on HestiaCP (Debian 12). Two main components:

1. **hestia-streamer** — Go HTTP server that streams custom `v-script` output in real time via Server-Sent Events (SSE). HestiaCP's own API only responds after a script finishes, so this streamer exists to surface progress output while the script is still running.
2. **Custom `v-scripts`** — Bash scripts placed in `/usr/local/hestia/bin/` that extend HestiaCP with WordPress-specific operations (cloning, info gathering, updates, etc.)

## Repo structure

```
hestia-setup.sh        # Server setup / maintenance entry point (run as root)
install-scripts.sh     # Deploys v-scripts + hestia-streamer (also callable from hestia-setup)
setup/                 # Modules sourced by hestia-setup.sh
  common.sh            # Shared utilities (colors, status_line, confirm, run_action, helpers)
  status.sh            # Status dashboard printed on every launch
  wpcli.sh / redis.sh / fail2ban.sh / maldet.sh / netdata.sh / security.sh
  php-fpm.sh / opcache.sh / mariadb.sh / nginx-templates.sh / maintenance.sh
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
sudo bash hestia-setup.sh

# Deploy v-scripts + streamer only
sudo bash install-scripts.sh

# Restart the service after changes
systemctl restart hestia-streamer

# Test the SSE endpoint
curl "http://localhost:8091/execute?script=v-test-stream"
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

All scripts follow the `v-<action>-<target>` naming convention and are symlinked into `/usr/local/hestia/bin/` by `install.sh`. Current scripts:

- **`v-clone-wp.sh`** — clones a WordPress site. Accepts flags for non-interactive use (e.g. via hestia-streamer) or runs fully interactively over SSH when flags are omitted:
  - `--src-user=<user>` — source HestiaCP user
  - `--src-domain=<domain>` — domain to clone
  - `--dest-user=<user>` — destination HestiaCP user (must already exist; creating new users is interactive-only)
  - `--new-domain=<domain>` — new domain name
  - `--force` — skip overwrite confirmation if the destination domain already exists (required when running non-interactively; without it the script exits with an error rather than hanging)

  Any omitted flag falls back to an interactive prompt. Steps: create domain + conditional SSL → export DB via WP-CLI → rsync files (excludes cache/backups) → create DB → update `wp-config.php` with new credentials, fresh salts, and unique Redis cache prefix → import DB → search-replace URLs and absolute paths → flush object cache. Supports cross-user cloning; handles overwrite mode (reuses existing DB if connectable); DB credentials generated with `openssl rand`.
- **`v-create-wp-staging.sh`** — creates a staging copy of a WordPress site, or tears one down with `--teardown`. Flags: `--src-user` `--src-domain` `--dest-user` `--new-domain` `--force` `--teardown`. Key differences from v-clone-wp: uploads are **not** copied — instead the live uploads directory is bind-mounted read-only into the staging site (OS-level enforcement, survives any WP/plugin write attempt, but does not survive a server reboot). Sets `WP_ENVIRONMENT_TYPE=staging` in wp-config. Reads `WP_STAGING_URL` from the live site's wp-config to pre-fill the staging domain; writes it back if not set or changed — this means the second run for the same site already knows where its staging lives. Teardown (`--teardown`) unmounts uploads, deletes the domain and database from HestiaCP, and optionally removes `WP_STAGING_URL` from the live wp-config if `--src-user` and `--src-domain` are also passed. SSL is skipped on domain creation — staging domains (e.g. customer.demolink.fi) are expected to handle their own certificates.
- **`v-update-wp.sh`** — runs all available WordPress updates. Flags: `--user=<hestia_user>` `--domain=<domain>` `--skip-backup` `--dry-run`. Backs up the database (gzipped, to `/home/$USER/backup/`) before touching anything unless `--skip-backup` is set. Enables maintenance mode for the duration, updates core → runs DB upgrade → updates all plugins → updates all themes → flushes cache, then disables maintenance mode. A `trap` ensures maintenance mode is always disabled even if the script errors out. `--dry-run` shows what would be updated without applying anything. Exits cleanly with no changes if everything is already up to date.
- **`v-get-wp-info.sh`** — prints a structured site report. Flags: `--user=<hestia_user>` `--domain=<domain>`. Sections: site identity, update status (core/plugins/themes), active theme (child vs standalone), PHP environment, database size/prefix/version, security posture (WP_DEBUG, file editing, default admin username), admin user list, object cache drop-in, active plugin list with update markers, content counts.
- **`v-test-stream.sh`** — simulates a long-running task for testing the SSE stream.

New v-scripts for WordPress operations (info gathering, updates, etc.) should follow the same pattern: print progress line-by-line to stdout, require root, validate inputs early.

### Installation (`install.sh`)

Symlinks `v-*` bash scripts into `/usr/local/hestia/bin/`, creates a systemd unit for `hestia-streamer`, and opens firewall port 8091 for a specific IP.

## Dependencies

- **Go** — compile the streamer
- **WP-CLI** — required by clone scripts for DB export/import
- **HestiaCP** — the control panel being automated
- **rsync**, **openssl** — used in clone scripts
- **systemd** — service management
