# `hs` — design

Status: agreed direction, 2026-09-24. Replaces `run.sh` + `setup/` and absorbs `hestia-streamer`.

## Why

`run.sh` grew one menu item per component we installed, and its status column answers the easiest question available — *is it installed* — so a box can be all green while broken (kuumalahde 2026-08-28: fail2ban green, every ban failing, no iptables). The checks that find real faults live elsewhere, and the same facts are computed four times:

| Where | Question it answers | Consumer |
|---|---|---|
| `setup/status.sh` | installed? | the menu's ticks |
| `v-server-setup-status.sh` | installed? (again, as JSON) | EngineLink Setup tab |
| `v-server-audit.sh` (19 checks) | does it work? | menu 15, fleet sweeps |
| `v-server-health.sh` | backups / services / disk / mail queue | EngineLink daily health |

Meanwhile the work actually done on these boxes — sites, backups, restore, reboot — is not in the menu at all.

## Decisions

- **Go + Bubble Tea** (`bubbletea`, `bubbles`, `lipgloss`, `huh`). Same language as the streamer, one static binary, nothing to install on Debian 12.
- **One binary `hs`**: TUI, CLI and the streamer (`hs serve`). One version, one deploy.
- **Sites cover every Hestia user on the box**, not just ones we manage.
- **One check engine.** The TUI, the CLI, EngineLink and cron all read the same results.
- **Bash v-scripts stay** as actions until porting one earns its keep. `v-wp-staging-create`, `v-restore-restic`, `v-wp-clone-site` are production-tested; rewriting them is risk without payoff.

## Status model

Every check returns:

```go
type Result struct {
    ID        string    // "firewall.rules", "site.alavusikkunat.fi.ssl"
    Section   string    // overview grouping: backups, security, ...
    Subject   string    // "" for box-level, else user or domain
    State     State     // OK | Configured | Warn | Fail | Unknown | NA
    Summary   string    // one line: "restic snapshot 3h ago for 6/6 users"
    Evidence  []string  // what was measured, raw enough to trust
    Why       string    // why it matters (non-OK only)
    Fix       *Fix      // suggested action: an hs command or shell line
    CheckedAt time.Time
    TTL       time.Duration // evidence older than this degrades OK → Configured
}
```

| State | Means | Colour |
|---|---|---|
| **OK** | a *functional* probe passed within TTL — the thing was seen working | green |
| **Configured** | installed and configured, not proven working (or proof went stale) | neutral |
| **Warn** | works but degraded / risky | yellow |
| **Fail** | broken or dangerous | red |
| **Unknown** | the check could not run (timeout, missing tool, no network) | grey — **never counts as green** |
| **NA** | does not apply to this box | hidden |

Rules:

1. **Green requires evidence of function, not presence.** A check that can only prove presence caps at Configured.
2. **Roll-ups are the worst child**, shown as counts (`2 FAIL · 5 WARN · 31 OK · 3 ?`), never a lone tick.
3. **Staleness is a state change**: an OK whose `CheckedAt + TTL` has passed renders as Configured with "last proven 9 d ago".
4. **Every non-OK carries Why + Fix.** The Overview is literally the list of non-OK results, worst first — the audit becomes the home screen instead of menu 15.
5. **Probes are bounded** (`context.WithTimeout` per check, defaults 5 s, per-check override) and **read-only**. Checks never write; only actions do.

### What earns green (initial catalogue)

Sources noted so nothing from the existing scripts is lost.

| Area | Check | OK when | From |
|---|---|---|---|
| Firewall | rules | `iptables` present, ≥ 5 rules, `hestia-iptables` active | audit |
| Fail2ban | bans | daemon active, WP jail present, no ban-action errors since daemon start, ≥ 1 successful ban since start (else Configured) | audit, status |
| SSH | auth | `PasswordAuthentication no` (effective `sshd -T`, not the file) | status |
| Updates | security | unattended-upgrades security-only, no pending security updates > 7 d, no reboot-required > 7 d | audit, status |
| Hestia | version | installed == latest (network Unknown ≠ OK) | status |
| Hestia | conf drift | no `*_SYSTEM` naming a purged package | setup-status, audit |
| Services | units | no failed units; core services active | audit, health |
| Backups | restic nightly | snapshot < 26 h for **every** user with data | health, audit |
| Backups | hourly DB | `db-hourly` snapshot < 2 h per DB user | new |
| Backups | restorable | `make-restore-script --check` (or repo open + key present) within 30 d | new |
| Backups | guard | guard ran < 26 h, exit 0 | new |
| Disk | space + inodes | every mounted fs < 75 % (Warn) / 90 % (Fail) | audit (root-only in status) |
| Memory | FPM ceiling | Σ `pm.max_children` × avg worker RSS < RAM | memory |
| Redis | health | PING, maxmemory + policy set, flush-scan share < threshold, keyspace bounded | audit, status |
| Redis | per-site | every WP site with the drop-in has a unique prefix and `WP_REDIS_DISABLE_GROUP_FLUSH` | audit |
| PHP | versions | no EOL PHP in use; OpCache ≥ 256 MB on used versions | audit, status |
| MariaDB | buffer | buffer pool hit rate ≥ 99 % | memory |
| Web | templates | repo templates installed **byte-identical**, `nginx -t` OK, intended domains assigned | status, audit |
| Web | cache headers | drop-in present and identical to repo | new |
| Mail | delivery | a delivery (`status=sent`) in the postfix log within 7 d, queue below threshold | health, audit |
| Maldet | scans | installed, last scan < 8 d, no unquarantined hits | status |
| Netdata | posture | running, tuned profile, :19999 **not** reachable externally | status, CLAUDE.md |
| hs | self | binary version == repo HEAD, streamer answering locally with the token | status |
| **Site** | reachable | HTTP 200 (or expected redirect) on the site's own URL | new |
| **Site** | SSL | cert valid > 14 d | audit |
| **Site** | core integrity | `wp core verify-checksums` clean (would have caught the alavus webshell) | new |
| **Site** | hardening | no PHP executes in `uploads/`, no `.sql`/`.zip`/`debug.log` reachable in docroot | audit (partial), new |
| **Site** | updates | no plugin with a pending security update > 7 d; core current | new |
| **Site** | backup | covered by the user's restic check | derived |
| **Site** | debug | `WP_DEBUG` off on production sites; `debug.log` < 50 MB | new (alavus: 11.8 GB) |

Slow per-site checks (WP-CLI ≈ 1–3 s/site) run in the background and are cached — see *State*.

## Information architecture

```
hs — hzweb1                                   2 FAIL · 5 WARN · 31 OK · 3 ?
───────────────────────────────────────────────────────────────────────────
  o  Overview      every non-OK result, worst first, each with Why + Fix
  s  Sites         all domains, all users → per-site screen
  b  Backups       restic per user, hourly DB, guard, restore, DR script
  x  Security      firewall, fail2ban, SSH, maldet, updates, hardening
  p  Performance   PHP-FPM profiles + ceiling, OpCache, MariaDB, Redis, memory
  w  Web server    nginx/apache templates + assignments, cache headers
  m  Mail          relay, recipients, test send, queue
  n  Monitoring    netdata, streamer, EngineLink link
  y  System        packages, Hestia update, service cleanup, disk cleanup, reboot
  /  search actions and sites     ?  help     r  refresh     q  quit
```

- **Keyboard-first.** Single-letter sections, `/` fuzzy search over every action and site, `j/k`/arrows, `enter`, `esc` back. Mirrors EngineLink's `g`-prefix jumps; mouse is never required.
- **Every screen = its checks on top, its actions below.** A section's actions are the fixes for its checks plus its routine operations; the Overview's `Fix` jumps straight to the action.
- **Sites** — table of every web domain across every user: user, WP version, updates, cache template, Redis, last backup, SSL days, hardening. Filterable (`/`), sortable. Enter → site screen: info, update, cache flush, clone, staging create/teardown, fix permissions, restore (guided), template switch, Redis install, block PHP in uploads. Non-WP domains show the non-WP checks only.
- **Action flow** keeps what `run_action` got right: show the exact commands (or the v-script + args) → confirm → stream output in a viewport → exit code → re-run the affected checks. Destructive actions keep **typed confirmation** (domain name or hostname).
- **Dropped / merged from the old menu:** Netdata "open :19999 to an IP" (contradicts the streamer proxy design — replaced by a check that it's closed); duplicate Deploy (1 and 13→1); single-item menus folded into their section; SMTP stops being Resend-only (relay host/port/credentials generic, Resend a preset).

## CLI

Everything the TUI does is a subcommand, so EngineLink, cron and fleet sweeps call the same code:

```
hs                              TUI
hs check [--section S] [--site D] [--json] [--brief] [--refresh]
hs site list [--json]
hs site <domain> info|update|cache-flush|harden|...
hs backup status|run|restore|dr-script
hs serve                        the streamer (systemd unit)
hs self update                  git pull + install (replaces v-hestiascripts-update)
hs version
```

Exit codes follow the audit: `2` any Fail, `1` any Warn, `0` otherwise (Unknown counts as Warn).

## Compatibility — what must not change

EngineLink depends on these; they stay as **thin wrappers** until EngineLink moves to `hs check --json`:

| Name | Contract | Wrapper |
|---|---|---|
| `v-server-health` | final non-empty stdout line = `{"backups":…,"services":…,"mailQueue":N,"disk":{"usedPct":N,"info":S}}`; exit 0 | `hs compat health` |
| `v-server-setup-status` | final line = the current JSON shape (streamer, wpcli, redis, fail2ban, maldet, netdata, security, smtp, phpFpmProfiles, opcache, mariadb, nginxTemplates, hestia, serviceDrift, restic, disk); exit 0 | `hs compat setup-status` |
| `v-server-audit` | text report, `--brief`, exit 2/1/0 | `hs check` |
| `v-hestiascripts-update` | detached install, exit 0 before restart | `hs self update` |
| `v-wp-*`, `v-server-backup-*` | unchanged — still bash, still symlinked | — |
| streamer HTTP | `/execute` (SSE, `event: exit`, allowlist), `/download`, `/upload`, `/netdata/alarms`, `/netdata/data`, `X-Streamer-Token`, port 8091 | `hs serve` (port of `main.go`, same handlers) |

Wrappers are golden-tested: the old script and the wrapper run on the same box and their JSON is diffed (field order aside) before the old script is deleted. A later EngineLink change can switch to the richer `hs check --json` (states + evidence + fixes) and retire the compat layer.

## State

`/var/lib/hs/`, root 700:

- `results.json` — last result per check with `CheckedAt`. The TUI opens instantly on it and refreshes in the background; stale entries render with their age.
- `/etc/cron.d/hs` — `hs check --refresh --quiet` every 15 min (box checks) and hourly (per-site WP-CLI checks), so evidence is fresh when someone looks and EngineLink reads cached results within its 30 s budget.
- Evidence counters that need history (e.g. "a ban succeeded since daemon start") are derived from logs at check time, not stored — no second source of truth.

## Repo layout

```
cmd/hs/main.go
internal/check/        registry, Result, runner (bounded, parallel), state cache
internal/check/box/    firewall, fail2ban, backups, redis, …   (one file per area)
internal/check/site/   reachable, ssl, integrity, hardening, …
internal/hestia/       typed wrappers over v-list-* JSON (users, domains, dbs, conf)
internal/wp/           WP-CLI runner (per-user, timeouts, --skip-plugins where safe)
internal/action/       action registry: plan → confirm → exec (stream) → recheck
internal/serve/        streamer handlers (port of main.go)
internal/tui/          bubbletea models: overview, sites, site, section, runner, search
internal/compat/       health + setup-status JSON shapes
v-*.sh, setup-*.sh     unchanged bash actions
templates/             unchanged
```

`go.mod` at the repo root; `hs` built static (`CGO_ENABLED=0`) and committed like `hestia-streamer` is today, so boxes still deploy with `git pull` and no toolchain.

## Phases

Each phase is shippable on its own and leaves the old tooling working.

1. **Check engine, read-only.** Registry + state model + box checks ported from audit / status / setup-status / health / memory. `hs check`, `hs compat …`. Run beside the old scripts on every box and diff. *Done when* the compat JSON matches on all boxes and `hs check` finds everything `v-server-audit` finds.
2. **`hs serve`.** Port `main.go` handlers verbatim, same unit name/port/env file; swap the systemd `ExecStart`. *Done when* EngineLink's Operate/Setup/health flows pass against it on one box, then fleet.
3. **TUI, read-only.** Overview, Sites (with per-site checks + cache), Backups, section screens showing checks. *Done when* it replaces `run.sh` for looking.
4. **Actions.** Registry with plan/confirm/stream/recheck. Wrap the v-scripts and `setup/*` functions first (exec, not rewrite); port a function to Go only when it needs rollback, validation or structured output (template install, php.ini edits, relay config). *Done when* every `run.sh` path and every root-shell script (`setup-restic-backup`, `v-restore-restic`, `make-restore-script`, `safe-reboot`) is reachable from `hs`.
5. **Retire** `run.sh`, `setup/`, `main.go`, `hestia-streamer`, the old status scripts. Update CLAUDE.md and `docs/submodules.md` in EngineLink.

## Open questions

- **Security-sensitive actions in `hs serve`**: the streamer allowlist today is name-based (`v-*`). Under one binary, keep the rule that root-shell-only operations (restore, reboot, DR script) are **not** reachable over HTTP — enforce it with an explicit per-action `Remote: false` flag, checked in `serve`, rather than the name prefix.
- **Per-site integrity checks** call `wp core verify-checksums`, which fetches checksums from api.wordpress.org — cache per WP version to keep the hourly run cheap.
- **Exemptions**: some sites legitimately fail a check (no Redis on a tiny site). Exemptions live in `/etc/hs/exemptions.yaml` with a reason and date, and render as NA with the reason — never silently green.
