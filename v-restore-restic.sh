#!/bin/bash
# v-restore-restic — guided, interactive restore from this box's restic backups.
#
# Run it, answer the questions, type the confirmation word. Nothing to remember.
#
#   1. which Hestia user
#   2. what to restore:
#        whole user · user settings only · one site (files + database) ·
#        one site, files only · one site, database only
#   3. which site (when the scope is a site) — the database is read from the
#      site's wp-config.php, with a pick-list fallback for non-WordPress sites
#   4. which backup time — nightly snapshots, plus the hourly database
#      snapshots when only a database is being restored
#   Then a summary of exactly what will be replaced, and a typed confirmation
#   (the domain, or the user name) before anything runs.
#
# Restores are delegated to upstream HestiaCP wherever it has a command
# (v-restore-user-full-restic, v-restore-web-domain-restic,
# v-restore-database-restic, v-restore-cron-job-restic). The hourly database
# snapshots are made by our own v-server-backup-db-hourly, and upstream cannot
# read them (it expects Hestia's /home/<user>/backup layout), so that path is
# implemented here: dump → drop → create → import.
#
# Safety copies. Before anything is replaced, the current state is put aside
# under /root/restic-restore-safety/: a zstd dump of the database, or the
# previous public_html moved whole (same filesystem, so it is instant). Copies
# older than 14 days are deleted at the next run. A whole-user restore is too
# big to copy aside, so it offers a v-backup-user tarball instead.
#
# Streamer: the v- prefix puts this on PATH via install-scripts.sh, but it is
# NOT in hestia-streamer's allowlist (v-wp-*, v-server-*, curated names) and it
# refuses to run without a terminal. Needs python3 for restic's JSON (present
# on every Debian 12 Hestia box — fail2ban is Python).
#
# Flags — optional, the point is not needing any:
#   --dry-run   walk the whole dialogue, then print what would run instead of running it
#   -h, --help

set -uo pipefail

HESTIA=${HESTIA:-/usr/local/hestia}
BIN="$HESTIA/bin"
SAFETY=/root/restic-restore-safety
SAFETY_KEEP_DAYS=14
DRY=0
TS=$(date +%Y%m%d-%H%M%S)

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY=1 ;;
    -h|--help) tail -n +2 "$0" | sed -n '/^#/!q;p' | sed 's/^# \?//'; exit 0 ;;
    *) echo "unknown argument: $1 (this script is interactive — just run it)" >&2; exit 1 ;;
  esac
  shift
done

# ---------------------------------------------------------------- ui ----
if [ -t 1 ]; then
  BOLD=$'\e[1m' DIM=$'\e[2m' RED=$'\e[31m' GRN=$'\e[32m' YEL=$'\e[33m' CYN=$'\e[36m' NC=$'\e[0m'
else
  BOLD='' DIM='' RED='' GRN='' YEL='' CYN='' NC=''
fi
say()  { printf '%s\n' "$*"; }
info() { printf '%s[restore]%s %s\n' "$CYN" "$NC" "$*"; }
warn() { printf '%s[restore] WARN:%s %s\n' "$YEL" "$NC" "$*"; }
hr()   { printf '%s%s%s\n' "$DIM" "------------------------------------------------------------------" "$NC"; }

SAFETY_NOTES=()
print_safety_notes() {
  [ ${#SAFETY_NOTES[@]} -gt 0 ] || return 0
  say ""
  say "  ${BOLD}Safety copies${NC} (delete when satisfied; auto-deleted after $SAFETY_KEEP_DAYS days):"
  local n; for n in "${SAFETY_NOTES[@]}"; do say "    - $n"; done
}
die() {
  printf '%s[restore] ERROR:%s %s\n' "$RED" "$NC" "$*" >&2
  print_safety_notes >&2
  logger -t v-restore-restic -- "FAILED: $*" 2>/dev/null
  exit 1
}
abort() { say "${DIM}aborted — nothing was changed.${NC}"; exit 0; }

# Why the repo would not open — one bounded probe, then a plain-language verdict.
# Deliberately a SINGLE rclone attempt: the Storage Box bans an IP after repeated
# failed logins, and rclone's default pacer would turn one bad probe into ten.
repo_diagnose() {
  local msg=$1 remote probe
  printf '%s[restore] ERROR:%s cannot open the backup repo for %s.\n' "$RED" "$NC" "$user" >&2
  say "  restic said: ${msg:-<no message>}"
  if [[ "$REPO" == rclone:* ]]; then
    remote=${REPO#rclone:}; remote=${remote%%:*}
    if ! command -v rclone >/dev/null 2>&1; then
      say "  rclone is not installed on this box — the repo cannot be reached. Run setup-restic-backup.sh."
    elif ! rclone listremotes 2>/dev/null | grep -qx "$remote:"; then
      say "  rclone has no remote named '$remote' in root's config, so this box was never onboarded"
      say "  to the Storage Box (or the config was lost). Run setup-restic-backup.sh."
    else
      say "  probing the Storage Box once (rclone lsd $remote:) …"
      probe=$(timeout 25 rclone lsd "$remote:" --retries 1 --low-level-retries 1 2>&1 >/dev/null | grep -v '^\s*$' | tail -1)
      if [ -z "$probe" ]; then
        say "  rclone reaches the Storage Box fine, so the fault is the repo path or the user's key:"
        say "    repo: $REPO/$user    key: $KEYFILE"
        say "  check the config side with: v-server-backup-guard --dry-run"
      else
        say "  rclone: $probe"
        case "$probe" in
          *refused*|*"i/o timeout"*|*"no route"*)
            say "  a refused/timed-out connection to port 23 FROM THIS BOX ONLY means the Storage Box has banned"
            say "  this IP after repeated failed logins (it happened to hzdemolink on 2026-09-07). Test from a"
            say "  second box or the workstation before touching any credentials — a ban lifts by itself or via Hetzner." ;;
          *"ermission denied"*|*"unable to authenticate"*|*handshake*|*"no supported methods"*)
            say "  authentication failed: wrong Storage Box password/key, or SSH is not enabled on the subaccount"
            say "  (a per-subaccount toggle in the Hetzner console). See setup-restic-backup.sh --help." ;;
          *)
            say "  see setup-restic-backup.sh --help for the known failure modes." ;;
        esac
      fi
    fi
  fi
  local last
  last=$(grep -h 'v-backup-user-restic' "$HESTIA/log/system.log" 2>/dev/null | tail -1)
  [ -n "$last" ] && say "  last successful restic backup on this box: $last" || say "  no restic backup has ever succeeded on this box (nothing in $HESTIA/log/system.log)."
  last=$(grep -h 'restic' "$HESTIA/log/error.log" 2>/dev/null | tail -1)
  [ -n "$last" ] && say "  last restic error in Hestia's log:            $last"
  logger -t v-restore-restic -- "FAILED: cannot open repo for $user: $msg" 2>/dev/null
  exit 1
}

# Echo a command (dim) and run it unless --dry-run.
run() {
  printf '%s$ %s%s\n' "$DIM" "$*" "$NC"
  [ "$DRY" -eq 1 ] || "$@"
}

# choose "Question" "label|description" ...  → CHOICE (0-based), CHOICE_LABEL
choose() {
  local prompt=$1; shift
  local opts=("$@") i ans label desc
  say ""; say "${BOLD}$prompt${NC}"
  for i in "${!opts[@]}"; do
    label=${opts[$i]%%|*}; desc=${opts[$i]#*|}
    [ "$desc" = "${opts[$i]}" ] && desc=""
    printf '  %s%2d)%s %-28s %s%s%s\n' "$BOLD" $((i + 1)) "$NC" "$label" "$DIM" "$desc" "$NC"
  done
  say "   ${DIM}q) quit${NC}"
  while :; do
    read -r -p "> " ans || abort
    case "$ans" in
      q|Q) abort ;;
      ''|*[!0-9]*) ;;
      *) if [ "$ans" -ge 1 ] && [ "$ans" -le ${#opts[@]} ]; then
           CHOICE=$((ans - 1)); CHOICE_LABEL=${opts[$CHOICE]%%|*}; return
         fi ;;
    esac
    say "  ${DIM}pick a number 1-${#opts[@]}, or q${NC}"
  done
}

# yesno "Question" default(y|n) → 0 for yes
yesno() {
  local ans hint="[y/N]"; [ "$2" = y ] && hint="[Y/n]"
  read -r -p "$1 $hint " ans || abort
  [ -z "$ans" ] && ans=$2
  [[ "$ans" =~ ^[Yy] ]]
}

# ---------------------------------------------------------- preflight ----
[ "${EUID:-$(id -u)}" -eq 0 ] || die "must run as root."
{ [ -t 0 ] && [ -t 1 ]; } || die "interactive only — run it in a terminal."
[ -f "$HESTIA/conf/restic.conf" ] || die "no restic repo registered on this box ($HESTIA/conf/restic.conf missing)."
# shellcheck disable=SC1091
source "$HESTIA/conf/restic.conf"
[ -n "${REPO:-}" ] || die "REPO is empty in $HESTIA/conf/restic.conf."
command -v restic  >/dev/null 2>&1 || die "restic not installed."
command -v python3 >/dev/null 2>&1 || die "python3 not installed (needed to read restic's snapshot list)."
MYSQL=$(command -v mariadb 2>/dev/null || command -v mysql 2>/dev/null || true)
DUMP=$(command -v mariadb-dump 2>/dev/null || command -v mysqldump 2>/dev/null || true)
ZSTD=$(command -v zstd 2>/dev/null || true)
HOSTFQDN=$(hostname -f 2>/dev/null || hostname)

# Tidy old safety copies so the directory does not grow forever.
if [ -d "$SAFETY" ] && [ "$DRY" -eq 0 ]; then
  while IFS= read -r old; do
    rm -rf -- "$old" && info "removed safety copy older than $SAFETY_KEEP_DAYS days: $old"
  done < <(find "$SAFETY" -mindepth 1 -maxdepth 1 -mtime +"$SAFETY_KEEP_DAYS" 2>/dev/null)
fi

say ""
say "${BOLD}Restore from restic backups${NC} — $HOSTFQDN"
[ "$DRY" -eq 1 ] && say "${YEL}DRY RUN: the dialogue is real, the restore is only printed.${NC}"

# ---------------------------------------------------------- 1. user ----
users=()
for d in "$HESTIA/data/users"/*/; do
  u=$(basename "$d")
  [ -s "$d/restic.conf" ] && [ -f "$d/user.conf" ] && users+=("$u")
done
[ ${#users[@]} -gt 0 ] || die "no user on this box has a restic key yet (nothing has been backed up)."

if [ ${#users[@]} -eq 1 ]; then
  user=${users[0]}
  say ""; say "${BOLD}User:${NC} $user ${DIM}(the only user with backups)${NC}"
else
  choose "Which user?" "${users[@]}"
  user=$CHOICE_LABEL
fi
USER_DATA="$HESTIA/data/users/$user"
KEYFILE="$USER_DATA/restic.conf"
USER_REPO="${REPO%/}/$user"
rs() { restic --repo "$USER_REPO" --password-file "$KEYFILE" "$@"; }

# -------------------------------- 2. read the repo once (fail fast) ----
say ""
info "reading the backup list from the Storage Box …"
errf=$(mktemp)
if ! snaps_json=$(rs snapshots --json 2>"$errf"); then
  # In --json mode restic reports the error as a JSON line; pull the message out.
  msg=$({ cat "$errf"; printf '%s\n' "$snaps_json"; } | python3 -c '
import sys, json
for line in sys.stdin:
    line = line.strip()
    if not line.startswith("{"):
        continue
    try:
        m = json.loads(line).get("message")
    except Exception:
        continue
    if m:
        print(m)
        break' 2>/dev/null)
  [ -n "$msg" ] || msg=$(grep -v '^\s*$' "$errf" | tail -1)
  rm -f "$errf"
  repo_diagnose "$msg"
fi
rm -f "$errf"

# TSV: short_id, epoch, kind, when (box-local time), age. Newest first.
# kind: nightly (whole /home/<user>) · hourly (db-hourly tag) · manual (hand-made DB dump) · other
PY_SNAPS='
import sys, json, re
from datetime import datetime, timezone
user = sys.argv[1]
try:
    snaps = json.load(sys.stdin)
except Exception:
    sys.exit(1)
now = datetime.now(timezone.utc)
rows = []
for s in snaps:
    t = re.sub(r"(\.\d{6})\d+", r"\1", s.get("time", ""))
    try:
        dt = datetime.fromisoformat(t.replace("Z", "+00:00"))
    except Exception:
        continue
    paths = s.get("paths") or []
    tags = s.get("tags") or []
    home = "/home/" + user
    if any(p.rstrip("/") == home or p.startswith(home + "/") for p in paths):
        kind = "nightly"
    elif "db-hourly" in tags or any("hestia-hourly-db" in p for p in paths):
        kind = "hourly"
    elif "manual" in tags or any("hestia-manual-db" in p for p in paths):
        kind = "manual"
    else:
        kind = "other"
    secs = int((now - dt.astimezone(timezone.utc)).total_seconds())
    if secs < 3600:
        age = "%d min ago" % max(secs // 60, 0)
    elif secs < 172800:
        age = "%d h ago" % (secs // 3600)
    else:
        age = "%d d ago" % (secs // 86400)
    sid = s.get("short_id") or s.get("id", "")[:8]
    when = dt.astimezone().strftime("%a %Y-%m-%d %H:%M %Z")
    rows.append((int(dt.timestamp()), sid, kind, when, age))
rows.sort(reverse=True)
for ep, sid, kind, when, age in rows:
    print("%s\t%d\t%s\t%s\t%s" % (sid, ep, kind, when, age))
'
SNAPS_TSV=$(python3 -c "$PY_SNAPS" "$user" <<< "$snaps_json") || die "could not parse the snapshot list."
[ -n "$SNAPS_TSV" ] || die "the repo for $user has no snapshots."

# --------------------------------------------------------- 3. scope ----
choose "What do you want to restore?" \
  "Whole user|every web domain, DNS zone, mail domain, database, cron job and home file of $user" \
  "User settings only|user.conf (package, limits, panel password), user SSL certs, cron jobs — no domains, databases or files" \
  "One site, files + database|/home/$user/web/DOMAIN and the site's database" \
  "One site, files only|/home/$user/web/DOMAIN — public_html is replaced, database untouched" \
  "One site, database only|the site's database — files untouched; hourly snapshots available"
case $CHOICE in
  0) SCOPE=user ;;
  1) SCOPE=settings ;;
  2) SCOPE=site ;;
  3) SCOPE=files ;;
  4) SCOPE=db ;;
esac
SCOPE_LABEL=$CHOICE_LABEL

kind_label() {
  case "$1" in
    nightly) echo "nightly, full backup" ;;
    hourly)  echo "hourly, database only" ;;
    manual)  echo "manual, database only" ;;
    *)       echo "$1" ;;
  esac
}

newest_nightly=$(awk -F'\t' '$3=="nightly"{print $1; exit}' <<< "$SNAPS_TSV")

# choose_snapshot kind... → SNAP_ID SNAP_KIND SNAP_WHEN SNAP_AGE
choose_snapshot() {
  local kinds=" $* " rows=() id ep kind when age
  while IFS=$'\t' read -r id ep kind when age; do
    [[ "$kinds" == *" $kind "* ]] && rows+=("$id"$'\t'"$kind"$'\t'"$when"$'\t'"$age")
  done <<< "$SNAPS_TSV"
  [ ${#rows[@]} -gt 0 ] || die "no usable snapshots for this kind of restore in the repo for $user."
  local total=${#rows[@]} per=15 page=0 start end i ans
  say ""; say "${BOLD}Restore from which backup?${NC} ${DIM}(newest first)${NC}"
  while :; do
    start=$((page * per)); end=$(((page + 1) * per)); [ $end -gt "$total" ] && end=$total
    for ((i = start; i < end; i++)); do
      IFS=$'\t' read -r id kind when age <<< "${rows[$i]}"
      printf '  %s%2d)%s %s  %s%-10s %s%s\n' "$BOLD" $((i + 1)) "$NC" "$when" "$DIM" "$age" "$(kind_label "$kind")" "$NC"
    done
    [ $end -lt "$total" ] && say "   ${DIM}m) more ($((total - end)) older)${NC}"
    say "   ${DIM}q) quit${NC}"
    read -r -p "> " ans || abort
    case "$ans" in
      q|Q) abort ;;
      m|M) [ $end -lt "$total" ] && page=$((page + 1)) ;;
      ''|*[!0-9]*) ;;
      *) if [ "$ans" -ge 1 ] && [ "$ans" -le "$total" ]; then
           IFS=$'\t' read -r SNAP_ID SNAP_KIND SNAP_WHEN SNAP_AGE <<< "${rows[$((ans - 1))]}"; return
         fi ;;
    esac
  done
}

# Hestia's backup.conf inside a nightly snapshot — lists WEB='a,b' DB='x,y' …
backup_conf_of() { rs dump "$1" "/home/$user/backup/backup.conf" 2>/dev/null; }
conf_list() { grep -oE "(^|[[:space:]])$1='[^']*'" <<< "$2" | head -1 | cut -d"'" -f2 | tr ',' '\n' | sed '/^$/d'; }
wp_db_name() { grep -oP "define\(\s*['\"]DB_NAME['\"]\s*,\s*['\"]\K[^'\"]+" | head -1; }

# ------------------------------------------------------- 4. site ----
domain="" DB="" DB_SOURCE="" SQL_PATH=""
if [ "$SCOPE" = site ] || [ "$SCOPE" = files ] || [ "$SCOPE" = db ]; then
  # Union of the live web.conf and the newest nightly's backup.conf, so a
  # domain deleted since the backup can still be picked.
  live_domains=$(grep -o "^DOMAIN='[^']*'" "$USER_DATA/web.conf" 2>/dev/null | cut -d"'" -f2)
  snap_domains=""
  if [ -n "$newest_nightly" ]; then
    bconf=$(backup_conf_of "$newest_nightly")
    snap_domains=$(conf_list WEB "$bconf")
  fi
  domain_opts=()
  while IFS= read -r d; do
    [ -n "$d" ] || continue
    if grep -qxF "$d" <<< "$live_domains"; then domain_opts+=("$d|")
    else domain_opts+=("$d|not on this server right now — in the backup only"); fi
  done < <(printf '%s\n%s\n' "$live_domains" "$snap_domains" | sed '/^$/d' | sort -u)
  [ ${#domain_opts[@]} -gt 0 ] || die "$user has no web domains, live or in the newest backup."
  choose "Which site?" "${domain_opts[@]}"
  domain=$CHOICE_LABEL
fi

# ------------------------------------------------- 5. backup time ----
if [ "$SCOPE" = db ]; then
  choose_snapshot nightly hourly manual
else
  choose_snapshot nightly
fi

# ------------------------------------- verify the pick, find the DB ----
if [ "$SNAP_KIND" = nightly ]; then
  bconf=$(backup_conf_of "$SNAP_ID")
  [ -n "$bconf" ] || die "snapshot $SNAP_ID has no Hestia backup.conf — it is not a nightly Hestia backup."
  snap_web=$(conf_list WEB "$bconf")
  snap_dbs=$(conf_list DB "$bconf")
  if [ -n "$domain" ] && [ "$SCOPE" != db ] && ! grep -qxF "$domain" <<< "$snap_web"; then
    die "$domain is not in the $SNAP_WHEN backup (it holds: $(tr '\n' ' ' <<< "$snap_web")). Pick another time."
  fi
fi

if [ "$SCOPE" = site ] || [ "$SCOPE" = db ]; then
  # 1. live wp-config, 2. wp-config inside a nightly snapshot, 3. ask.
  for wp in "/home/$user/web/$domain/public_html/wp-config.php" "/home/$user/web/$domain/wp-config.php"; do
    [ -f "$wp" ] || continue
    DB=$(wp_db_name < "$wp"); [ -n "$DB" ] && { DB_SOURCE="wp-config.php on this server"; break; }
  done
  if [ -z "$DB" ] && [ "$SNAP_KIND" = nightly ]; then
    DB=$(rs dump "$SNAP_ID" "/home/$user/web/$domain/public_html/wp-config.php" 2>/dev/null | wp_db_name)
    [ -n "$DB" ] && DB_SOURCE="wp-config.php inside the backup"
  fi

  # What the chosen snapshot can actually give us.
  if [ "$SNAP_KIND" = nightly ]; then
    available=$snap_dbs
  else
    available=$(rs ls "$SNAP_ID" 2>/dev/null | grep -E '\.sql$' | sed 's#.*/##; s#\.sql$##')
  fi
  [ -n "$available" ] || die "the $SNAP_WHEN backup contains no databases."

  if [ -n "$DB" ] && ! grep -qxF "$DB" <<< "$available"; then
    warn "$domain uses database $DB ($DB_SOURCE), but the $SNAP_WHEN backup does not contain it."
    warn "it holds: $(tr '\n' ' ' <<< "$available")"
    DB=""
  fi
  if [ -z "$DB" ]; then
    db_opts=()
    while IFS= read -r d; do db_opts+=("$d|"); done <<< "$available"
    if [ "$SCOPE" = site ]; then db_opts+=("(skip the database)|restore files only"); fi
    choose "Which database belongs to $domain?" "${db_opts[@]}"
    if [ "$CHOICE_LABEL" = "(skip the database)" ]; then
      SCOPE=files; SCOPE_LABEL="One site, files only"
    else
      DB=$CHOICE_LABEL; DB_SOURCE="chosen by you"
    fi
  fi
  if [ -n "$DB" ] && [ "$SNAP_KIND" != nightly ]; then
    SQL_PATH=$(rs ls "$SNAP_ID" 2>/dev/null | grep -E "/$DB\.sql$" | head -1)
    [ -n "$SQL_PATH" ] || die "cannot find $DB.sql inside snapshot $SNAP_ID."
  fi
  if [ -n "$DB" ] && ! grep -q "DB='$DB'" "$USER_DATA/db.conf" 2>/dev/null; then
    warn "$DB is not registered to $user in Hestia right now (deleted since the backup?). A nightly restore re-registers it; an hourly one only imports data."
  fi
fi

TARBALL=no
if [ "$SCOPE" = user ]; then
  say ""
  say "A whole-user restore replaces too much to put aside as a safety copy."
  if yesno "Take a local tarball backup of $user first (v-backup-user, a few minutes)?" y; then TARBALL=yes; fi
fi

# ------------------------------------------------------- 6. confirm ----
say ""; hr
say "${BOLD}You are about to restore${NC}"
printf '  %-10s %s\n' "Server:"   "$HOSTFQDN"
printf '  %-10s %s\n' "User:"     "$user"
printf '  %-10s %s\n' "Scope:"    "$SCOPE_LABEL"
[ -n "$domain" ] && printf '  %-10s %s\n' "Site:" "$domain"
[ -n "$DB" ]     && printf '  %-10s %s  %s(%s)%s\n' "Database:" "$DB" "$DIM" "$DB_SOURCE" "$NC"
printf '  %-10s %s  %s(%s, %s, snapshot %s)%s\n' "From:" "$SNAP_WHEN" "$DIM" "$(kind_label "$SNAP_KIND")" "$SNAP_AGE" "$SNAP_ID" "$NC"
say ""
say "  ${BOLD}What changes${NC}"
case $SCOPE in
  user)
    say "  - every web domain of $user: public_html wiped and replaced, domain config and SSL rebuilt"
    say "  - every database, DNS zone, mail domain and cron job of $user replaced"
    say "  - other files under /home/$user overlaid from the backup (files added since are kept)"
    say "  - user.conf replaced (package, limits, panel password as they were)"
    if [ $TARBALL = yes ]; then say "  - safety: a v-backup-user tarball is written to /backup first"; else say "  - ${YEL}no safety copy${NC}"; fi ;;
  settings)
    say "  - user.conf replaced (package, limits, contact, panel password as they were)"
    say "  - user-level SSL certificates and cron jobs replaced, then the user is rebuilt"
    say "  - no web domain, database or file is touched"
    say "  - safety: the current user.conf, ssl/ and cron.conf are copied to $SAFETY/" ;;
  site|files)
    say "  - /home/$user/web/$domain is replaced from the backup: public_html is emptied first,"
    say "    then everything comes back as it was at $SNAP_WHEN; domain config and SSL are rebuilt"
    say "  - safety: the current public_html is moved aside whole (kept $SAFETY_KEEP_DAYS days)"
    if [ "$SCOPE" = site ]; then
      say "  - database $DB is dropped and re-imported from the backup"
      say "  - safety: a zstd dump of the current $DB is written first"
    else
      say "  - the database is not touched"
    fi ;;
  db)
    say "  - database $DB is dropped and re-imported from the backup; nothing else is touched"
    say "  - safety: a zstd dump of the current $DB is written first" ;;
esac
[ -n "$domain" ] && say "  - the WordPress object cache of $domain is flushed afterwards (if it is a WordPress site)"
hr
if [ -n "$domain" ]; then word=$domain; else word=$user; fi
say ""
read -r -p "Type ${BOLD}$word${NC} to proceed (anything else aborts): " ans || abort
[ "$ans" = "$word" ] || abort

# ------------------------------------------------------- 7. execute ----
logger -t v-restore-restic -- "START user=$user scope=$SCOPE domain=${domain:-} db=${DB:-} snapshot=$SNAP_ID ($SNAP_WHEN) dry=$DRY" 2>/dev/null
say ""
[ "$DRY" -eq 1 ] && say "${YEL}DRY RUN — printing, not running.${NC}"

safety_dir() { mkdir -p "$SAFETY" && chmod 700 "$SAFETY"; }

db_exists() { [ -n "$MYSQL" ] && "$MYSQL" -e "USE \`$1\`" >/dev/null 2>&1; }

safety_db_dump() {
  [ -n "$DUMP" ] || die "no mysqldump/mariadb-dump on this box — cannot take a safety copy."
  if ! db_exists "$1"; then info "database $1 does not exist right now — nothing to put aside."; return 0; fi
  safety_dir
  local out="$SAFETY/${user}_${1}_${TS}.sql"; local comp="cat"
  if [ -n "$ZSTD" ]; then out="$out.zst"; comp="$ZSTD -q -T0"; else out="$out.gz"; comp="gzip"; fi
  info "safety copy: dumping the current database $1 → $out"
  if [ "$DRY" -eq 1 ]; then say "${DIM}\$ $DUMP --single-transaction --routines --quick $1 | $comp > $out${NC}"; return 0; fi
  if ! "$DUMP" --single-transaction --routines --quick "$1" 2>"$SAFETY/.dump.err" | $comp > "$out"; then
    rm -f "$out"; die "safety dump of $1 failed: $(tail -1 "$SAFETY/.dump.err" 2>/dev/null) — nothing has been changed."
  fi
  rm -f "$SAFETY/.dump.err"
  SAFETY_NOTES+=("database $1 as it was: $out")
}

safety_move_public_html() {
  local src="/home/$user/web/$domain/public_html" dest
  [ -d "$src" ] || { info "no public_html to put aside."; return 0; }
  safety_dir
  if [ "$(stat -c %d "$src")" = "$(stat -c %d "$SAFETY")" ]; then
    dest="$SAFETY/${user}_${domain}_${TS}.public_html"
  else
    dest="/home/$user/web/$domain/public_html.pre-restore-$TS"
    warn "$SAFETY is on a different filesystem — keeping the copy next to the site instead (delete it soon, the nightly backup will otherwise pick it up)"
  fi
  info "safety copy: moving the current public_html aside → $dest"
  run mv "$src" "$dest" || die "could not move public_html aside — nothing has been changed."
  SAFETY_NOTES+=("previous public_html: $dest")
}

flush_wp_cache() {
  local docroot="/home/$user/web/$domain/public_html"
  [ -f "$docroot/wp-config.php" ] || return 0
  command -v wp >/dev/null 2>&1 || return 0
  info "flushing the WordPress object cache …"
  run timeout 60 sudo -u "$user" wp --path="$docroot" cache flush --quiet || warn "cache flush failed — run: sudo -u $user wp --path=$docroot cache flush"
}

do_user_full() {
  if [ $TARBALL = yes ]; then
    info "local tarball first …"
    run "$BIN/v-backup-user" "$user" || die "v-backup-user failed — stopping before the restore."
    SAFETY_NOTES+=("tarball in /backup: $(ls -t /backup/"$user".*.tar 2>/dev/null | head -1)")
  fi
  info "restoring the whole user (upstream v-restore-user-full-restic) …"
  printf '%s$ %s%s\n' "$DIM" "$BIN/v-restore-user-full-restic $user $SNAP_ID <restic key>" "$NC"
  if [ "$DRY" -eq 0 ]; then
    "$BIN/v-restore-user-full-restic" "$user" "$SNAP_ID" "$(cat "$KEYFILE")" || die "v-restore-user-full-restic reported an error (see above)."
  fi
}

do_user_settings() {
  safety_dir
  local keep="$SAFETY/${user}_settings_${TS}"
  info "safety copy: current user.conf, ssl/, cron.conf → $keep"
  if [ "$DRY" -eq 0 ]; then
    mkdir -p "$keep" && cp -a "$USER_DATA/user.conf" "$keep/" && cp -a "$USER_DATA/ssl" "$keep/" 2>/dev/null
    [ -f "$USER_DATA/cron.conf" ] && cp -a "$USER_DATA/cron.conf" "$keep/"
  fi
  SAFETY_NOTES+=("previous user settings: $keep")
  local tmp; tmp=$(mktemp -d /root/restic-restore.XXXXXX); chmod 700 "$tmp"
  info "downloading the user's Hestia config from the backup …"
  run rs restore "$SNAP_ID" --include "/home/$user/backup/hestia" --target "$tmp" || die "download failed — nothing has been changed."
  local src="$tmp/home/$user/backup/hestia"
  if [ "$DRY" -eq 0 ]; then
    [ -f "$src/user.conf" ] || die "backup has no hestia/user.conf — nothing has been changed."
    cp -f "$src/user.conf" "$USER_DATA/user.conf"
    [ -d "$src/ssl" ] && cp -rf "$src/ssl/." "$USER_DATA/ssl/"
    [ -f "$src/backup-excludes.conf" ] && cp -f "$src/backup-excludes.conf" "$USER_DATA/"
  fi
  run "$BIN/v-restore-cron-job-restic" "$user" "$SNAP_ID" || warn "cron jobs not restored (the backup may hold none)."
  run "$BIN/v-rebuild-user" "$user" || warn "v-rebuild-user reported an error — check the panel."
  rm -rf "$tmp"
}

do_site_files() {
  safety_move_public_html
  info "restoring /home/$user/web/$domain (upstream v-restore-web-domain-restic) …"
  run "$BIN/v-restore-web-domain-restic" "$user" "$SNAP_ID" "$domain" || die "v-restore-web-domain-restic reported an error (see above)."
}

do_site_db() {
  [ -n "$MYSQL" ] || die "no mysql/mariadb client on this box."
  if [ "$SNAP_KIND" = nightly ]; then
    safety_db_dump "$DB"
    info "restoring database $DB (upstream v-restore-database-restic) …"
    run "$BIN/v-restore-database-restic" "$user" "$SNAP_ID" "$DB" || die "v-restore-database-restic reported an error (see above)."
    return
  fi
  # Hourly / manual dump: plain SQL under /var/lib/hestia-*-db/<user>/<db>.sql
  local tmp; tmp=$(mktemp -d /root/restic-restore.XXXXXX); chmod 700 "$tmp"
  info "downloading $SQL_PATH from snapshot $SNAP_ID …"
  if [ "$DRY" -eq 1 ]; then
    say "${DIM}\$ restic dump $SNAP_ID $SQL_PATH > $tmp/$DB.sql${NC}"
  else
    rs dump "$SNAP_ID" "$SQL_PATH" > "$tmp/$DB.sql" || die "download failed — nothing has been changed."
    [ -s "$tmp/$DB.sql" ] || die "the downloaded dump is empty — nothing has been changed."
    info "downloaded $(du -h "$tmp/$DB.sql" | cut -f1)"
  fi
  safety_db_dump "$DB"
  local charset
  charset=$(grep "DB='$DB'" "$USER_DATA/db.conf" 2>/dev/null | grep -o "CHARSET='[^']*'" | cut -d"'" -f2)
  : "${charset:=utf8mb4}"
  info "recreating database $DB ($charset) and importing …"
  if [ "$DRY" -eq 1 ]; then
    say "${DIM}\$ $MYSQL -e 'DROP DATABASE IF EXISTS \`$DB\`; CREATE DATABASE \`$DB\` CHARACTER SET $charset'${NC}"
    say "${DIM}\$ $MYSQL $DB < $tmp/$DB.sql${NC}"
  else
    "$MYSQL" -e "DROP DATABASE IF EXISTS \`$DB\`; CREATE DATABASE \`$DB\` CHARACTER SET $charset;" || die "could not recreate $DB."
    "$MYSQL" "$DB" < "$tmp/$DB.sql" || die "import into $DB failed — the previous contents are in the safety copy."
  fi
  rm -rf "$tmp"
}

case $SCOPE in
  user)     do_user_full ;;
  settings) do_user_settings ;;
  site)     do_site_files; do_site_db ;;
  files)    do_site_files ;;
  db)       do_site_db ;;
esac
[ -n "$domain" ] && flush_wp_cache

# ------------------------------------------------------- 8. report ----
say ""; hr
if [ "$DRY" -eq 1 ]; then
  say "${YEL}${BOLD}Dry run finished.${NC} Nothing was changed."
else
  say "${GRN}${BOLD}Done.${NC} $SCOPE_LABEL for $user restored from $SNAP_WHEN."
  logger -t v-restore-restic -- "OK user=$user scope=$SCOPE domain=${domain:-} db=${DB:-} snapshot=$SNAP_ID" 2>/dev/null
fi
if [ -n "$domain" ] && command -v curl >/dev/null 2>&1; then
  code=$(curl -s -o /dev/null -m 10 -w '%{http_code}' "https://$domain/" 2>/dev/null || echo "no answer")
  say "  https://$domain/ answers: HTTP $code"
fi
print_safety_notes
say "  ${DIM}log: journalctl -t v-restore-restic${NC}"
exit 0
