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

# --- Backups: newest backup file age per HestiaCP user ---------------------
# HestiaCP writes user backups to /backup as <user>.<timestamp>.tar (plus a
# per-user /home/<user>/backup for DB dumps). We report the freshest artifact
# we can find per user, in whole hours.
backups_json="[]"
if command -v v-list-users >/dev/null 2>&1; then
  users=$(v-list-users plain 2>/dev/null | awk '{print $1}')
  entries=""
  for u in $users; do
    newest=""
    # Hestia system backups live in /backup as ${user}.*.tar
    for f in /backup/${u}.*.tar; do
      [ -e "$f" ] || continue
      if [ -z "$newest" ] || [ "$f" -nt "$newest" ]; then newest="$f"; fi
    done
    # Fall back to the user's own backup dir (DB/site dumps)
    if [ -z "$newest" ] && [ -d "/home/${u}/backup" ]; then
      newest=$(find "/home/${u}/backup" -type f -printf '%T@ %p\n' 2>/dev/null \
        | sort -rn | head -n1 | cut -d' ' -f2-)
    fi
    [ -z "$newest" ] && continue
    mtime=$(stat -c %Y "$newest" 2>/dev/null) || continue
    age_hours=$(( (now_epoch - mtime) / 3600 ))
    stamp=$(date -d "@$mtime" '+%Y-%m-%d %H:%M' 2>/dev/null)
    entries="${entries},{\"user\":\"${u}\",\"ageHours\":${age_hours},\"newest\":\"${stamp}\"}"
  done
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
