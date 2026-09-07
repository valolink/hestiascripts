#!/bin/bash
# v-server-backup-guard — daily self-healing check that restic backups will
# actually run for every user on this box. Config-only: no network, no dumps.
#
# Why this exists. `v-backup-users-restic` enumerates users fresh on every run,
# so NEW USERS ARE PICKED UP AUTOMATICALLY — there is nothing to "enrol". What
# is NOT automatic is the per-user `BACKUPS_INCREMENTAL` flag (note the plural):
# it is a package attribute, upstream ships it as 'no' in default.pkg/system.pkg,
# and v-add-user copies the package file verbatim into the new user.conf. So a
# user created under a package that predates (or postdates) onboarding silently
# never backs up, and you find out when you need the backup.
#
# This re-asserts the flag everywhere, daily, and says what it changed. It is
# self-healing on purpose: a check that only reports leaves a chore, and a chore
# that has to be remembered is the thing this whole toolchain exists to remove.
#
# Also verifies the things that make a box look healthy while being broken:
#   - system BACKUP_INCREMENTAL=yes (without it v-backup-users-restic exits at once)
#   - conf/restic.conf REPO has a NON-EMPTY path. HestiaCP >=1.10.4 builds
#     "${REPO%/}/$user", so an empty path yields an ABSOLUTE 'rclone:remote:/user'
#     that a chrooted Storage Box does not resolve to the same place as the
#     relative 'user'. Every backup then fails "Unable to access restic repo" —
#     invisible, because nothing forces you to look. (hzweb1, found 2026-09-07.)
#   - the cron entries exist at all
#   - every user with a repo key still has one, and it is non-empty
#
# Flags: --dry-run (report, change nothing) --quiet (only report problems)
# Exit:  0 nothing to do · 1 fixed something or warnings · 2 broken, needs a human

set -uo pipefail

HESTIA=${HESTIA:-/usr/local/hestia}
CRON_FILE=/etc/cron.d/hestia-restic
DRY=0
QUIET=0

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY=1; shift ;;
    --quiet)   QUIET=1; shift ;;
    -h|--help) tail -n +2 "$0" | head -30 | grep '^#' | sed 's/^# \?//'; exit 0 ;;
    *)         echo "unknown argument: $1" >&2; exit 1 ;;
  esac
done

rc=0
say()  { [ "$QUIET" -eq 1 ] || echo "[backup-guard] $*"; }
warn() { echo "[backup-guard] $*"; logger -t v-server-backup-guard -- "$*" 2>/dev/null; [ "$rc" -lt 1 ] && rc=1; }
bad()  { echo "[backup-guard] BROKEN: $*" >&2; logger -t v-server-backup-guard -- "BROKEN: $*" 2>/dev/null; rc=2; }

[ "${EUID:-$(id -u)}" -eq 0 ] || { echo "must run as root" >&2; exit 2; }

# ---- 1. is restic even configured on this box? ----
if [ ! -f "$HESTIA/conf/restic.conf" ]; then
  say "no restic repo registered on this box — nothing to guard."
  exit 0
fi
# shellcheck disable=SC1090
source "$HESTIA/conf/restic.conf"

if [ -z "${REPO:-}" ]; then
  bad "REPO is empty in conf/restic.conf — no backups can run."
else
  case "$REPO" in
    rclone:*)
      body=${REPO#rclone:}; path=${body#*:}
      if [ -z "$path" ]; then
        bad "REPO='$REPO' has an EMPTY path. HestiaCP builds \"\${REPO%/}/\$user\", so this
             resolves to an ABSOLUTE 'rclone:${body%%:*}:/<user>' which a chrooted Storage Box
             does NOT treat as the relative '<user>' where the repos actually live.
             Every user backup fails 'Unable to access restic repo'. Fix: re-register with a
             per-box path, e.g. v-add-backup-host-restic 'rclone:${body%%:*}:hestia-$(hostname -s)' \\
             '${SNAPSHOTS:--1}' '${KEEP_DAILY:-14}' '${KEEP_WEEKLY:-8}' '${KEEP_MONTHLY:-6}' '${KEEP_YEARLY:-1}'
             (move the existing repo dirs on the remote first, or the keys will not match)."
      else
        say "repo: $REPO"
      fi ;;
    *) say "repo: $REPO (non-rclone backend, path check skipped)" ;;
  esac
fi

# ---- 2. system-level flag ----
sysflag=$(grep -E "^BACKUP_INCREMENTAL=" "$HESTIA/conf/hestia.conf" 2>/dev/null | cut -d"'" -f2)
if [ "$sysflag" != yes ]; then
  if [ "$DRY" -eq 1 ]; then
    warn "system BACKUP_INCREMENTAL='$sysflag' (would set to 'yes')"
  else
    "$HESTIA/bin/v-change-sys-config-value" BACKUP_INCREMENTAL yes >/dev/null 2>&1 \
      && warn "system BACKUP_INCREMENTAL was '$sysflag' — set to 'yes'." \
      || bad "could not set system BACKUP_INCREMENTAL (was '$sysflag')."
  fi
fi

# ---- 3. per-user + package flag (the actual self-healing bit) ----
set_yes() {
  local f=$1 what=$2
  [ -f "$f" ] || return 0
  local cur
  cur=$(grep -m1 "^BACKUPS_INCREMENTAL=" "$f" | cut -d"'" -f2)
  [ "$cur" = yes ] && return 0
  if [ "$DRY" -eq 1 ]; then
    warn "$what: BACKUPS_INCREMENTAL='${cur:-<missing>}' (would set 'yes')"
    return 0
  fi
  if grep -q "^BACKUPS_INCREMENTAL=" "$f"; then
    sed -i "s/^BACKUPS_INCREMENTAL=.*/BACKUPS_INCREMENTAL='yes'/" "$f"
  else
    echo "BACKUPS_INCREMENTAL='yes'" >> "$f"
  fi
  warn "$what: BACKUPS_INCREMENTAL was '${cur:-<missing>}' — set to 'yes'."
}

for pkg in "$HESTIA"/data/packages/*.pkg; do
  [ -f "$pkg" ] || continue
  set_yes "$pkg" "package $(basename "$pkg" .pkg)"
done
for uc in "$HESTIA"/data/users/*/user.conf; do
  [ -f "$uc" ] || continue
  set_yes "$uc" "user $(basename "$(dirname "$uc")")"
done

# ---- 4. keys present and non-empty ----
missing=0; empty=0
for uc in "$HESTIA"/data/users/*/user.conf; do
  [ -f "$uc" ] || continue
  u=$(basename "$(dirname "$uc")")
  grep -q "SUSPENDED='no'" "$uc" || continue
  k="$HESTIA/data/users/$u/restic.conf"
  if [ ! -f "$k" ]; then
    missing=$((missing+1))                       # normal before that user's first run
  elif [ ! -s "$k" ]; then
    bad "user $u has an EMPTY restic.conf — its repo is unopenable. Do NOT let a backup
         run recreate it blindly; the existing repo is encrypted with the lost key."
    empty=$((empty+1))
  fi
done
[ "$missing" -gt 0 ] && say "$missing user(s) have no restic key yet (created on their first backup)."

# ---- 5. is anything actually scheduled? ----
if [ ! -f "$CRON_FILE" ]; then
  warn "no $CRON_FILE — restic is configured but NOTHING SCHEDULES IT. Backups only run
        when invoked by hand. Re-run setup-restic-backup.sh to install the schedule."
else
  grep -q "v-backup-users-restic" "$CRON_FILE" || warn "$CRON_FILE has no nightly v-backup-users-restic entry."
  grep -q "v-server-backup-db-hourly" "$CRON_FILE" || say "no hourly DB entry in $CRON_FILE (optional)."
fi

case "$rc" in
  0) say "all good." ;;
  1) say "finished with fixes/warnings above." ;;
  2) echo "[backup-guard] one or more problems need a human — see BROKEN lines above." >&2 ;;
esac
exit "$rc"
