#!/bin/bash
# v-server-backup-db-hourly — hourly DATABASE-ONLY restic snapshot for every user.
#
# Complements the nightly `v-backup-users-restic` (whole /home/$user). This one
# exists because a WooCommerce site takes orders all day: losing up to 24h of
# them is a real business loss, and the nightly is the only other recovery point.
#
# It is deliberately NOT "run v-backup-user-restic hourly". Four reasons, each of
# which would otherwise make hourly backups useless or expensive:
#
#   1. RETENTION. Hestia's forget policy is keep-daily/weekly/monthly, which keeps
#      only the MOST RECENT snapshot of each day. Hourly snapshots under that
#      policy are deleted at the very next run — you would pay the whole cost and
#      still hold one point per day. This needs `--keep-last N`, which is why
#      setup-restic-backup.sh now defaults SNAPSHOTS=48.
#   2. PRUNE. v-backup-user-restic ends every run with `forget --prune`, and prune
#      (rewriting pack files) is the expensive operation. Standard restic practice
#      is forget often, prune rarely — so this script forgets WITHOUT pruning and
#      leaves pruning to the nightly run.
#   3. STAGING COLLISION. v-backup-user-config opens with `rm -fr /home/$user/backup/`.
#      An hourly job staging there would race the nightly and corrupt whichever
#      was mid-flight. We stage outside /home entirely.
#   4. COMPRESSION. Hestia writes .sql.zst, and compressed dumps dedupe terribly —
#      one changed row rewrites the whole compressed stream, so each hourly
#      snapshot would upload nearly the full database. The repos are format v2,
#      so restic compresses natively; feeding it PLAIN SQL makes hourly deltas
#      small. This is the difference between hourly being cheap and being absurd.
#
# Snapshots are tagged `db-hourly` and live under a distinct path, so restic's
# forget (which groups by host+paths) treats them as their own retention group
# and they never compete with the nightly whole-home snapshots.
#
# Flags:
#   --user U     only this user
#   --keep N     hourly snapshots to retain [48]
#   --dry-run    dump and report sizes, back up nothing
#
# Exit: 0 if every eligible user succeeded, 1 if any failed (cron stays quiet
# either way; detail goes to journald under the tag v-server-backup-db-hourly).

set -uo pipefail

HESTIA=${HESTIA:-/usr/local/hestia}
BIN="$HESTIA/bin"
STAGE_ROOT=/var/lib/hestia-hourly-db
LOCK=/var/lock/hestia-hourly-db.lock
EXCLUDE_CONF="$HESTIA/conf/restic-hourly-exclude.conf"
TAG=db-hourly
KEEP=48
ONLY_USER=""
DRY=0

while [ $# -gt 0 ]; do
  case "$1" in
    --user)    ONLY_USER=${2:-}; shift 2 ;;
    --keep)    KEEP=${2:-48};    shift 2 ;;
    --dry-run) DRY=1;            shift ;;
    -h|--help) tail -n +2 "$0" | head -40 | grep '^#' | sed 's/^# \?//'; exit 0 ;;
    *)         echo "unknown argument: $1" >&2; exit 1 ;;
  esac
done

log() { echo "[hourly-db] $*"; logger -t v-server-backup-db-hourly -- "$*" 2>/dev/null; }

[ "${EUID:-$(id -u)}" -eq 0 ] || { echo "must run as root" >&2; exit 1; }

# Never overlap with ourselves. An hourly job that runs long (big DB, slow link)
# must not have the next hour's copy start dumping on top of it.
exec 9>"$LOCK"
if ! flock -n 9; then
  log "another run is still going — skipping this hour."
  exit 0
fi

[ -f "$HESTIA/conf/restic.conf" ] || { log "no restic repo registered — nothing to do."; exit 0; }
# shellcheck disable=SC1090
source "$HESTIA/conf/restic.conf"
[ -n "${REPO:-}" ] || { log "REPO empty in conf/restic.conf"; exit 1; }
command -v restic >/dev/null 2>&1 || { log "restic not installed"; exit 1; }

DUMP=mysqldump
[ -x /usr/bin/mariadb-dump ] && DUMP=/usr/bin/mariadb-dump

HOSTFQDN=$(hostname -f 2>/dev/null || hostname)
failed=0; done_users=0; skipped=0

excluded() {
  [ -f "$EXCLUDE_CONF" ] || return 1
  grep -qxF "$1" "$EXCLUDE_CONF" 2>/dev/null
}

for user in $("$BIN/v-list-sys-users" plain 2>/dev/null | awk '{print $1}'); do
  [ -n "$ONLY_USER" ] && [ "$user" != "$ONLY_USER" ] && continue
  uc="$HESTIA/data/users/$user/user.conf"
  [ -f "$uc" ] || continue

  grep -q "SUSPENDED='no'" "$uc" || { skipped=$((skipped+1)); continue; }
  # Respect the same gate the nightly uses, so enabling/disabling is one switch.
  grep -q "^BACKUPS_INCREMENTAL='yes'" "$uc" || { skipped=$((skipped+1)); continue; }
  excluded "$user" && { log "$user: excluded by $EXCLUDE_CONF"; skipped=$((skipped+1)); continue; }

  keyfile="$HESTIA/data/users/$user/restic.conf"
  if [ ! -s "$keyfile" ]; then
    # The nightly creates the key and inits the repo on its first run. Don't
    # duplicate that logic here — just wait for it rather than risk two
    # different code paths initialising the same repo.
    log "$user: no restic key yet (nightly will create it) — skipping."
    skipped=$((skipped+1)); continue
  fi

  dbconf="$HESTIA/data/users/$user/db.conf"
  [ -s "$dbconf" ] || { skipped=$((skipped+1)); continue; }

  # Collect this user's live mysql databases.
  dbs=()
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    d=$(grep -o "^DB='[^']*'"        <<< "$line" | cut -d"'" -f2)
    t=$(grep -o "TYPE='[^']*'"       <<< "$line" | cut -d"'" -f2)
    s=$(grep -o "SUSPENDED='[^']*'"  <<< "$line" | cut -d"'" -f2)
    [ -n "$d" ] || continue
    [ "$t" = mysql ] || { log "$user/$d: TYPE=$t not supported here — skipping."; continue; }
    [ "$s" = yes ] && continue
    dbs+=("$d")
  done < "$dbconf"
  [ ${#dbs[@]} -gt 0 ] || { skipped=$((skipped+1)); continue; }

  stage="$STAGE_ROOT/$user"
  rm -rf "$stage"
  mkdir -p "$stage" || { log "$user: cannot create $stage"; failed=1; continue; }
  chmod 700 "$stage"

  # Space guard: uncompressed dumps are the point (see header note 4), so make
  # sure we can actually land them before starting.
  avail=$(df -Pk "$STAGE_ROOT" | awk 'NR==2{print $4}')
  if [ "${avail:-0}" -lt 1048576 ]; then
    log "$user: less than 1GB free at $STAGE_ROOT — skipping to avoid filling the disk."
    rm -rf "$stage"; failed=1; continue
  fi

  dumped=0
  for d in "${dbs[@]}"; do
    # --single-transaction matches Hestia's own dump: non-blocking on InnoDB, so
    # a live storefront keeps taking orders while this runs.
    if timeout 900 "$DUMP" --single-transaction --routines --quick "$d" > "$stage/$d.sql" 2>"$stage/$d.err"; then
      rm -f "$stage/$d.err"; dumped=$((dumped+1))
    else
      log "$user/$d: dump FAILED — $(tail -1 "$stage/$d.err" 2>/dev/null)"
      rm -f "$stage/$d.sql" "$stage/$d.err"; failed=1
    fi
  done

  if [ "$dumped" -eq 0 ]; then
    log "$user: no database dumped — nothing to snapshot."
    rm -rf "$stage"; continue
  fi

  size=$(du -sh "$stage" 2>/dev/null | cut -f1)
  if [ "$DRY" -eq 1 ]; then
    log "$user: DRY-RUN — $dumped db(s), $size staged at $stage (not backed up, not removed)."
    done_users=$((done_users+1)); continue
  fi

  repo="${REPO%/}/$user"
  if timeout 3600 restic --repo "$repo" --password-file "$keyfile" \
        backup "$stage" --tag "$TAG" --host "$HOSTFQDN" \
        --limit-upload 10240 >/dev/null 2>&1; then
    log "$user: snapshot ok ($dumped db(s), $size)."
    done_users=$((done_users+1))
  else
    log "$user: restic backup FAILED."
    failed=1; rm -rf "$stage"; continue
  fi

  # forget WITHOUT --prune: cheap (drops references only). The nightly
  # v-backup-user-restic run does the prune. An exclusive-lock clash with a
  # nightly run in progress is harmless — the next hour tidies up.
  if ! timeout 600 restic --repo "$repo" --password-file "$keyfile" \
        forget --tag "$TAG" --keep-last "$KEEP" >/dev/null 2>&1; then
    log "$user: forget skipped (repo busy or locked) — next run will retry."
  fi

  # Never leave plaintext SQL on disk. restic dedupes against the repo index,
  # not against local state, so keeping it would buy nothing.
  rm -rf "$stage"
done

rmdir "$STAGE_ROOT" 2>/dev/null
verb="snapshotted"; [ "$DRY" -eq 1 ] && verb="dumped (dry-run, nothing sent)"
log "done: $done_users user(s) $verb, $skipped skipped, failures=$failed"
exit "$failed"
