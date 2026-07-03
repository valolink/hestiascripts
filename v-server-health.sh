#!/bin/bash
# v-server-health — one-shot server health snapshot for EngineLink's daily
# Layer-2 monitoring. Surfaces what Netdata doesn't know about: HestiaCP
# per-user backup freshness, core service status, and the mail queue depth.
#
# Contract with EngineLink (server/utils/serverMonitor.ts): the LAST non-empty
# stdout line MUST be a single-line JSON object. Any diagnostic chatter must go
# out BEFORE that line (or to stderr) — the parser takes the final JSON frame
# before `event: exit`. Always exits 0 on a completed run so the streamer's
# exit event reads success; partial data is fine (missing tools → empty/null).

if [ "$EUID" -ne 0 ]; then
  echo "ERROR: Please run as root."
  exit 1
fi

export PATH=$PATH:/usr/local/hestia/bin

now_epoch=$(date +%s)

# --- Backups: freshest artifact age per HestiaCP user ----------------------
# Two backup backends coexist during the restic rollout, so we report whichever
# is authoritative per user (restic first, then legacy tarball):
#   * restic (incremental, streamed to a Hetzner Storage Box): there is NO local
#     tarball — freshness lives in `restic snapshots`. Used when the user has a
#     per-user key (/usr/local/hestia/data/users/<u>/restic.conf) AND a system
#     repo is registered (/usr/local/hestia/conf/restic.conf → REPO=).
#   * legacy tarballs in /backup/<user>.*.tar (+ /home/<user>/backup DB dumps).
#
# Each restic call is a network round-trip, and EngineLink aborts the whole
# health fetch at 30s (serverMonitor.ts). So run the per-user probes in PARALLEL
# with a per-call `timeout` — wall-clock stays ~one timeout regardless of how
# many users there are, and a slow/unreachable Storage Box can't blow the budget
# (a failed restic call just falls through to the tarball logic for that user).
RESTIC_REPO=""
[ -r /usr/local/hestia/conf/restic.conf ] \
  && RESTIC_REPO=$(grep "^REPO=" /usr/local/hestia/conf/restic.conf 2>/dev/null | cut -f2 -d \')

# Emit one JSON object for a user, or nothing if no backup artifact is found.
user_backup_json() {
  local u=$1 mtime="" iso newest upass="/usr/local/hestia/data/users/$1/restic.conf"

  # 1) restic snapshot (authoritative on migrated boxes)
  if [ -n "$RESTIC_REPO" ] && [ -r "$upass" ] && command -v restic >/dev/null 2>&1; then
    iso=$(timeout 8 restic --repo "${RESTIC_REPO}${u}" --password-file "$upass" \
            --no-lock --json snapshots --latest 1 2>/dev/null \
          | grep -oE '"time":"[^"]+"' | head -n1 | sed 's/.*"time":"//; s/"$//')
    [ -n "$iso" ] && mtime=$(date -d "$iso" +%s 2>/dev/null)
  fi

  # 2) legacy tarball / DB-dump fallback
  if [ -z "$mtime" ]; then
    newest=""
    for f in /backup/${u}.*.tar; do
      [ -e "$f" ] || continue
      if [ -z "$newest" ] || [ "$f" -nt "$newest" ]; then newest="$f"; fi
    done
    if [ -z "$newest" ] && [ -d "/home/${u}/backup" ]; then
      newest=$(find "/home/${u}/backup" -type f -printf '%T@ %p\n' 2>/dev/null \
        | sort -rn | head -n1 | cut -d' ' -f2-)
    fi
    [ -n "$newest" ] && mtime=$(stat -c %Y "$newest" 2>/dev/null)
  fi

  [ -z "$mtime" ] && return 0
  local age_hours stamp
  age_hours=$(( (now_epoch - mtime) / 3600 ))
  stamp=$(date -d "@$mtime" '+%Y-%m-%d %H:%M' 2>/dev/null)
  printf '{"user":"%s","ageHours":%s,"newest":"%s"}' "$u" "$age_hours" "$stamp"
}

backups_json="[]"
if command -v v-list-users >/dev/null 2>&1; then
  users=$(v-list-users plain 2>/dev/null | awk '{print $1}')
  tmpd=$(mktemp -d)
  for u in $users; do
    user_backup_json "$u" > "$tmpd/$u" &
  done
  wait
  entries=""
  for u in $users; do
    line=$(cat "$tmpd/$u" 2>/dev/null)
    [ -n "$line" ] && entries="${entries},${line}"
  done
  rm -rf "$tmpd"
  backups_json="[${entries#,}]"
fi

# --- Services: is-active for the core stack --------------------------------
services_json="{}"
svc_entries=""
detect_services() {
  local base="nginx hestia"
  # DB flavor varies (mariadb vs mysql); include whichever exists.
  systemctl list-unit-files 2>/dev/null | grep -q '^mariadb' && base="$base mariadb"
  systemctl list-unit-files 2>/dev/null | grep -q '^mysql'   && base="$base mysql"
  # First installed php*-fpm (Hestia runs several; one active is enough signal).
  local fpm
  fpm=$(systemctl list-unit-files 2>/dev/null | grep -oE '^php[0-9.]+-fpm' | head -n1)
  [ -n "$fpm" ] && base="$base $fpm"
  echo "$base"
}
for s in $(detect_services); do
  state=$(systemctl is-active "$s" 2>/dev/null)
  [ -z "$state" ] && state="unknown"
  svc_entries="${svc_entries},\"${s}\":\"${state}\""
done
services_json="{${svc_entries#,}}"

# --- Mail queue depth ------------------------------------------------------
mail_queue=0
if command -v mailq >/dev/null 2>&1; then
  # Postfix: queued messages start with a hex queue ID at line start.
  mq=$(mailq 2>/dev/null | grep -cE '^[A-F0-9]{8,}')
  [ -n "$mq" ] && mail_queue="$mq"
elif command -v exim >/dev/null 2>&1; then
  mail_queue=$(exim -bpc 2>/dev/null || echo 0)
fi

printf '{"backups":%s,"services":%s,"mailQueue":%s}\n' \
  "$backups_json" "$services_json" "$mail_queue"

exit 0
