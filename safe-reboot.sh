#!/bin/bash
# safe-reboot — staged, guarded reboot for HestiaCP web boxes (Debian 12).
#
# DELIBERATELY NOT a v-script: no "v-" prefix, so the hestia-streamer allowlist
# (^v-[a-zA-Z0-9-]+$) can never invoke it and install-scripts.sh (globs v-*)
# never symlinks it into /usr/local/hestia/bin/. The ONLY way to run this is a
# root shell on the box. A whole-box reboot is too destructive to sit behind an
# HTTP trigger, so the access gate is SSH itself.
#
# Flow (nothing stateful happens until AFTER you confirm):
#   1. Hard guards   — root, systemd present, no package op in progress
#   2. Soft guard    — refuse if a HestiaCP backup / mysqldump is running (--force overrides)
#   3. Confirm       — type the hostname (or pass --yes); LAST chance to bail
#   --- commit: from here we always reach the reboot ---
#   4. Pre-stop non-web services      (best-effort)
#   5. Redis BGSAVE + wait            (best-effort, auth-aware)
#   6. MariaDB InnoDB dirty-page flush (best-effort)
#   7. sync + reboot                  (with fallbacks)
#
# Install: cp safe-reboot.sh /usr/local/sbin/safe-reboot && chmod 700 /usr/local/sbin/safe-reboot
# Usage:   safe-reboot            # interactive: prompts for hostname confirmation
#          safe-reboot --yes      # non-interactive: skip the prompt (for scripted use)
#          safe-reboot --force    # proceed even if a backup appears to be running

set -uo pipefail

ASSUME_YES=0
FORCE=0
for arg in "${@:-}"; do
    case "$arg" in
        -y|--yes)   ASSUME_YES=1 ;;
        -f|--force) FORCE=1 ;;
        "")         ;;
        *) echo "[safe-reboot] Unknown argument: $arg" >&2; exit 2 ;;
    esac
done

HOST_SHORT=$(hostname -s 2>/dev/null || hostname 2>/dev/null || echo "this-host")

# log() prints to stdout AND to syslog (journald), so there's a persistent
# audit trail across the reboot: `journalctl -t safe-reboot` after it comes back
# tells you it was a clean staged reboot, not a crash.
#
# Once TIMING flips to 1 (at the commit point), each line is stamped with the
# elapsed seconds since confirmation — so you can watch how long the box stays
# reachable before the connection drops.
TIMING=0
log() {
    local tag="[safe-reboot]"
    [ "${TIMING:-0}" = "1" ] && tag="[safe-reboot +${SECONDS}s]"
    echo "$tag $*"
    command -v logger >/dev/null 2>&1 && logger -t safe-reboot -- "$*" || true
}
die() { log "ABORT: $*"; exit 1; }

# ===========================================================================
# Stage 1: hard guards — refuse outright, never overridable
# ===========================================================================
[ "${EUID:-$(id -u)}" -eq 0 ] || die "must run as root."

command -v systemctl >/dev/null 2>&1 || die "systemctl not found — this script assumes systemd."

# Already shutting down / starting up? Don't stack a reboot on top.
state=$(systemctl is-system-running 2>/dev/null || true)
if [ "$state" = "stopping" ]; then
    die "system is already shutting down."
fi

log "Checking for running package operations..."
if pgrep -x dpkg >/dev/null 2>&1 || pgrep -x apt >/dev/null 2>&1 || pgrep -x apt-get >/dev/null 2>&1; then
    die "dpkg/apt is running — rebooting now could corrupt the package database. Retry in a few minutes."
fi
# unattended-upgrades actively installing (trailing space excludes the idle shutdown hook)
if pgrep -f "unattended-upgrade " >/dev/null 2>&1; then
    die "unattended-upgrades is installing packages. Watch: tail -f /var/log/unattended-upgrades/unattended-upgrades.log"
fi
log "OK — no package operations in progress."

# ===========================================================================
# Stage 2: soft guard — HestiaCP backup / DB dump in progress (--force overrides)
# ===========================================================================
backup_busy=""
pgrep -f 'v-backup-user' >/dev/null 2>&1 && backup_busy="HestiaCP backup"
pgrep -x mysqldump      >/dev/null 2>&1 && backup_busy="${backup_busy:+$backup_busy + }mysqldump"
if [ -n "$backup_busy" ]; then
    if [ "$FORCE" -eq 1 ]; then
        log "WARN: $backup_busy running, but --force given — proceeding."
    else
        die "$backup_busy appears to be running. Rebooting now wastes it. Wait, or pass --force."
    fi
fi

# ===========================================================================
# Stage 3: confirmation — the LAST bail-out point, before anything is touched
# ===========================================================================
if [ "$ASSUME_YES" -ne 1 ]; then
    if [ ! -t 0 ]; then
        die "not a terminal and --yes not given. Refusing to reboot without confirmation."
    fi
    echo "[safe-reboot] About to REBOOT '$HOST_SHORT'. All websites on this box go down for the reboot."
    read -r -p "[safe-reboot] Type the hostname ('$HOST_SHORT') to confirm: " reply
    if [ "$reply" != "$HOST_SHORT" ] && [ "$reply" != "$(hostname 2>/dev/null)" ]; then
        die "confirmation did not match — nothing changed."
    fi
fi

# ===========================================================================
# --- COMMIT POINT: past here we always proceed to the reboot. No aborts, so we
#     can never leave the box with services stopped but not rebooted. ---
# ===========================================================================
SECONDS=0   # reset the elapsed clock; every log line below is now stamped +Ns
TIMING=1
log "Confirmed. Beginning pre-flight for '$HOST_SHORT'."

# Stage 4: pre-stop services that don't affect website availability.
# Web stack stays UP (nginx/apache2/php-fpm/mariadb/redis). 'named' stays up too
# in case this box is authoritative DNS — keep it serving until the kernel goes.
PRESTOP_SERVICES=(netdata vsftpd fail2ban postfix cron atd)
log "Pre-stopping non-essential services: ${PRESTOP_SERVICES[*]}"
for svc in "${PRESTOP_SERVICES[@]}"; do
    if systemctl is-active --quiet "$svc" 2>/dev/null; then
        if systemctl stop "$svc" 2>/dev/null; then
            log "  stopped $svc"
        else
            log "  WARN: failed to stop $svc (continuing)"
        fi
    fi
done

# Stage 5: Redis background save (auth-aware, best-effort).
REDIS_SVC=""
for s in redis-server redis; do
    systemctl is-active --quiet "$s" 2>/dev/null && { REDIS_SVC="$s"; break; }
done
if ! command -v redis-cli >/dev/null 2>&1; then
    log "Redis pre-save skipped (redis-cli not installed)."
elif [ -z "$REDIS_SVC" ]; then
    log "Redis pre-save skipped (no active redis service)."
else
    # If requirepass is set, redis-cli needs it — otherwise 'config get save' would
    # silently NOAUTH-fail and we'd wrongly think persistence is off.
    REDIS_CONF=$(find /etc/redis -maxdepth 2 -name 'redis.conf' 2>/dev/null | head -1)
    REDIS_PASS=""
    if [ -n "$REDIS_CONF" ]; then
        REDIS_PASS=$(grep -E '^[[:space:]]*requirepass[[:space:]]+' "$REDIS_CONF" 2>/dev/null | tail -1 | awk '{print $2}' | tr -d '"')
    fi
    rcli() {
        if [ -n "$REDIS_PASS" ]; then
            redis-cli -a "$REDIS_PASS" --no-auth-warning "$@" 2>/dev/null
        else
            redis-cli "$@" 2>/dev/null
        fi
    }

    SAVE_CFG=$(rcli config get save | tail -1)
    if [ -n "$SAVE_CFG" ]; then
        log "Redis persistence enabled — triggering BGSAVE..."
        rcli BGSAVE >/dev/null
        for _ in $(seq 1 30); do            # wait up to ~60s
            INPROG=$(rcli info persistence | grep -oP 'rdb_bgsave_in_progress:\K[0-9]' | head -1)
            [ "${INPROG:-0}" = "0" ] && break
            sleep 2
        done
        log "Redis snapshot done."
    else
        log "Redis persistence disabled (pure cache) — nothing to save."
    fi
fi

# Stage 6: MariaDB/MySQL InnoDB pre-flush (best-effort; uses root socket auth).
# Detect both the client binary (mysql OR mariadb — newer MariaDB drops the
# mysql symlink) and the service name (mariadb OR mysql OR mysqld), so this
# stage isn't skipped just because of naming variants.
DB_CLI=""
for c in mysql mariadb; do
    command -v "$c" >/dev/null 2>&1 && { DB_CLI="$c"; break; }
done
DB_SVC=""
for s in mariadb mysql mysqld; do
    systemctl is-active --quiet "$s" 2>/dev/null && { DB_SVC="$s"; break; }
done

if [ -n "$DB_CLI" ] && [ -n "$DB_SVC" ]; then
    log "InnoDB pre-flush via '$DB_CLI' (service '$DB_SVC') — telling it to flush dirty pages..."
    "$DB_CLI" -e "SET GLOBAL innodb_max_dirty_pages_pct = 0;" 2>/dev/null \
        || log "WARN: could not set flush target (continuing anyway)"

    log "Waiting for dirty pages to drain (max ~2 min)..."
    for _ in $(seq 1 24); do
        DIRTY=$("$DB_CLI" -Nse "SHOW GLOBAL STATUS LIKE 'Innodb_buffer_pool_pages_dirty';" 2>/dev/null | awk '{print $2}')
        # Empty (query failed / server gone) or non-numeric → stop waiting, don't error under set -u.
        [[ "$DIRTY" =~ ^[0-9]+$ ]] || { log "  (no dirty-page count available — stopping wait)"; break; }
        log "  dirty pages: $DIRTY"
        if [ "$DIRTY" -le 10 ]; then
            log "Buffer pool clean enough."
            break
        fi
        sleep 5
    done
else
    log "MariaDB/MySQL pre-flush skipped (client='${DB_CLI:-none}', active service='${DB_SVC:-none}')."
fi

# Stage 7: flush filesystem buffers and reboot.
log "Syncing filesystems..."
sync

log "All pre-flight done in ${SECONDS}s. Rebooting '$HOST_SHORT' NOW."
sleep 1   # give stdout/journald a moment to flush the line above

if ! systemctl reboot 2>/dev/null; then
    log "WARN: 'systemctl reboot' failed — trying 'reboot'."
    if ! reboot 2>/dev/null; then
        log "WARN: 'reboot' failed — forcing via 'systemctl reboot --force'."
        systemctl reboot --force
    fi
fi
