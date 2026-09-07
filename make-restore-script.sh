#!/bin/bash
# make-restore-script — emit a SELF-CONTAINED disaster-recovery script for THIS box.
#
# The output is a single bash file you keep in a password vault. Run it on a fresh
# HestiaCP install and it will: connect that box to the same Hetzner Storage Box,
# register the same restic repo, then CREATE EVERY USER and restore them from
# their latest snapshot. No pre-created users, no notes to follow.
#
# DELIBERATELY NOT a v-script (no v- prefix): its output contains every restic
# encryption key on the box, so it must never be reachable from hestia-streamer.
# Same access gate as setup-restic-backup.sh / safe-reboot.sh — root shell only.
#
# Why users don't need creating first: upstream v-restore-user-full-restic takes
# USER SNAPSHOT KEY, and when the user does not exist it runs v-add-user itself,
# writes the key to the new user's restic.conf, verifies the repo opens, and then
# restores user.conf/ssl from the snapshot over the top. That is the whole reason
# this can be turnkey — see the "user absent" branch in that script.
#
# The generated script EMBEDS setup-restic-backup.sh verbatim, read from disk at
# generation time. That is deliberate: a hand-maintained second copy of the
# connection logic would drift (this repo has shipped a drifted template before),
# and the embedded copy inherits every fix — IPv4 pinning, the subaccount-SSH
# preflight, the keyscan verification.
#
# Usage:
#   make-restore-script.sh                 # write ./restore-<host>-<date>.sh
#   make-restore-script.sh -o /path/out.sh # write somewhere specific
#   make-restore-script.sh --stdout        # print to stdout (pipe to your vault CLI)
#
# Re-run it whenever you add a user, change the Storage Box password, or move the
# repo — the artifact is a point-in-time snapshot of all three.

set -uo pipefail

log() { echo "[make-restore] $*" >&2; }
die() { echo "[make-restore] ERROR: $*" >&2; exit 1; }

HESTIA=/usr/local/hestia
OUT=""
TO_STDOUT=0
SELF_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SETUP_SRC="$SELF_DIR/setup-restic-backup.sh"

while [ $# -gt 0 ]; do
  case "$1" in
    -o|--out)   OUT=${2:-}; shift 2 ;;
    --stdout)   TO_STDOUT=1; shift ;;
    --setup-src) SETUP_SRC=${2:-}; shift 2 ;;
    -h|--help)  tail -n +2 "$0" | grep '^#' | sed 's/^# \?//'; exit 0 ;;
    *)          die "unknown argument: $1 (see --help)" ;;
  esac
done

# ---- guards ----
[ "${EUID:-$(id -u)}" -eq 0 ] || die "must run as root (needs to read user restic keys)."
[ -f "$HESTIA/conf/restic.conf" ] || die "$HESTIA/conf/restic.conf missing — this box has no restic repo registered. Run setup-restic-backup.sh first."
[ -f "$SETUP_SRC" ] || die "can't find setup-restic-backup.sh at $SETUP_SRC (pass --setup-src PATH)."
command -v rclone >/dev/null 2>&1 || die "rclone not found — needed to read the remote's stored password."

# ---- gather: repo ----
# shellcheck disable=SC1090
source "$HESTIA/conf/restic.conf"
[ -n "${REPO:-}" ] || die "REPO empty in $HESTIA/conf/restic.conf"
case "$REPO" in
  rclone:*) ;;
  *) die "REPO is '$REPO' — this generator only understands rclone: repos." ;;
esac

repo_body=${REPO#rclone:}          # e.g. storagebox:hestia-hzhestia
REMOTE=${repo_body%%:*}            # storagebox
REPO_PATH=${repo_body#*:}          # hestia-hzhestia

[ -n "$REMOTE" ] || die "couldn't parse an rclone remote name out of REPO='$REPO'"
if [ -z "$REPO_PATH" ]; then
  die "REPO='$REPO' has an EMPTY path. HestiaCP >=1.10.4 builds \"\${REPO%/}/\$user\",
     so an empty path yields an ABSOLUTE 'rclone:$REMOTE:/<user>' that a chrooted
     Storage Box does not resolve to the same place as the relative '<user>'.
     Fix the box first (re-register with a per-box --path), then re-run me."
fi

# ---- gather: rclone remote ----
RC_CONF=$(rclone config file 2>/dev/null | tail -1)
[ -f "$RC_CONF" ] || die "can't locate rclone.conf"
sect=$(sed -n "/^\[$REMOTE\]/,/^\[/p" "$RC_CONF")
[ -n "$sect" ] || die "no [$REMOTE] section in $RC_CONF"
rc_val() { sed -n "s/^$1 *= *//p" <<< "$sect" | head -1; }

SB_HOST=$(rc_val host)
SB_USER=$(rc_val user)
SB_PORT=$(rc_val port); SB_PORT=${SB_PORT:-23}
SB_OBSC=$(rc_val pass)

[ -n "$SB_HOST" ] && [ -n "$SB_USER" ] || die "[$REMOTE] is missing host/user."
[ -n "$SB_OBSC" ] || die "[$REMOTE] has no stored password. This generator only supports
     password auth (Hetzner subaccounts). A key-auth box would need its private key
     carried across too, which is not something to put in a vault note by default."

SB_PASS=$(rclone reveal "$SB_OBSC" 2>/dev/null) \
  || die "rclone reveal failed on the stored password."
[ -n "$SB_PASS" ] || die "revealed password is empty."

# ---- gather: per-user keys ----
declare -a PAIRS=()
for f in "$HESTIA"/data/users/*/restic.conf; do
  [ -f "$f" ] || continue
  u=$(basename "$(dirname "$f")")
  k=$(tr -d '\r\n' < "$f")
  [ -n "$k" ] || { log "WARN: $u has an EMPTY restic.conf — skipping (its repo is unrecoverable)."; continue; }
  PAIRS+=("$u:$k")
done
[ ${#PAIRS[@]} -gt 0 ] || die "no user restic keys found under $HESTIA/data/users/*/restic.conf — nothing to restore."

# Warn about repos on the remote with no local key: they cannot be restored and
# their absence from this artifact is silent otherwise.
remote_dirs=$(timeout 45 rclone lsd "$REMOTE:$REPO_PATH" 2>/dev/null | awk '{print $NF}')
for d in $remote_dirs; do
  grep -q "^$d:" <<< "$(printf '%s\n' "${PAIRS[@]}")" || \
    log "WARN: repo '$d' exists on the Storage Box but has NO key on this box — it is already unrecoverable and is NOT in this artifact."
done

FQDN=$(hostname -f 2>/dev/null || hostname)
SHORT=$(hostname -s 2>/dev/null || echo host)
STAMP=$(date +%F)

if [ -z "$OUT" ] && [ "$TO_STDOUT" -eq 0 ]; then
  OUT="./restore-${SHORT}-${STAMP}.sh"
fi

# ---- emit ----
emit() {
cat <<HEADER
#!/bin/bash
# =============================================================================
#  DISASTER RECOVERY — $FQDN
#  generated $STAMP by make-restore-script.sh
#
#  *** THIS FILE CONTAINS EVERY RESTIC ENCRYPTION KEY FOR THAT BOX ***
#  *** PLUS THE STORAGE BOX PASSWORD. Treat it as a root credential.  ***
#
#  What it does, on a FRESH HestiaCP install:
#    1. connects this box to the same Hetzner Storage Box (rclone sftp remote)
#    2. registers the same restic repo + retention with Hestia
#    3. for each user below: CREATES the user and restores its latest snapshot
#       (v-restore-user-full-restic creates absent users itself — you do NOT
#        need to v-add-user anything first)
#
#  Prerequisites on the target box:
#    - HestiaCP already installed (same major version ideally), running as root
#    - outbound TCP :$SB_PORT to the Storage Box
#    - the web/db/mail stacks the users need (nginx/php/mariadb) present
#
#  Usage:
#    bash $(basename "${OUT:-restore.sh}") --check        # verify every repo opens + list snapshots. CHANGES NOTHING.
#    bash $(basename "${OUT:-restore.sh}") --connect-only # set up the Storage Box connection, no restore
#    bash $(basename "${OUT:-restore.sh}")                # full restore of all users
#    bash $(basename "${OUT:-restore.sh}") --only a,b     # restore a subset
#
#  ALWAYS run --check first. It proves the keys in this file still open the
#  repos before you start writing to a new box.
#
#  If the Storage Box password was rotated since $STAMP, either edit SB_PASS
#  below or run with:  SB_PASS='newpassword' bash $(basename "${OUT:-restore.sh}") --check
# =============================================================================

set -uo pipefail

# ---- connection (point-in-time as of $STAMP) ----
SB_HOST=$(printf %q "$SB_HOST")
SB_USER=$(printf %q "$SB_USER")
SB_PORT=$(printf %q "$SB_PORT")
SB_PASS="\${SB_PASS:-$(printf %q "$SB_PASS")}"
REMOTE=$(printf %q "$REMOTE")
REPO_PATH=$(printf %q "$REPO_PATH")
REPO="rclone:\$REMOTE:\$REPO_PATH"

# ---- retention, mirrored from the source box ----
R_SNAPSHOTS=$(printf %q "${SNAPSHOTS:--1}")
R_DAILY=$(printf %q "${KEEP_DAILY:-14}")
R_WEEKLY=$(printf %q "${KEEP_WEEKLY:-8}")
R_MONTHLY=$(printf %q "${KEEP_MONTHLY:-6}")
R_YEARLY=$(printf %q "${KEEP_YEARLY:-1}")

# ---- users and their restic encryption keys ----
# Format: user:key — the key is BOTH the repo password and (transiently) the
# initial Hestia password v-add-user is given; user.conf from the snapshot
# overwrites it during the restore.
USERS=(
HEADER

for p in "${PAIRS[@]}"; do
  printf '  %s\n' "$(printf %q "$p")"
done

cat <<'BODY'
)

MODE=full
ONLY=""
while [ $# -gt 0 ]; do
  case "$1" in
    --check)        MODE=check; shift ;;
    --connect-only) MODE=connect; shift ;;
    --only)         ONLY=${2:-}; shift 2 ;;
    -h|--help)      head -45 "$0" | grep '^#' | sed 's/^# \?//'; exit 0 ;;
    *)              echo "unknown argument: $1" >&2; exit 1 ;;
  esac
done

log() { echo "[restore] $*"; }
die() { echo "[restore] ERROR: $*" >&2; exit 1; }

[ "${EUID:-$(id -u)}" -eq 0 ] || die "must run as root."
[ -x /usr/local/hestia/bin/v-restore-user-full-restic ] \
  || die "v-restore-user-full-restic missing — install HestiaCP (>=1.9) on this box first."

selected() {
  [ -z "$ONLY" ] && return 0
  tr ',' '\n' <<< "$ONLY" | grep -qx "$1"
}

# ---------------------------------------------------------------- connection
if ! command -v rclone >/dev/null 2>&1; then
  log "rclone not present — installing from rclone.org (needs outbound HTTPS)."
  curl -fsSL https://rclone.org/install.sh | bash \
    || die "rclone install failed. Install it manually, then re-run."
fi

SETUP=$(mktemp /tmp/setup-restic-backup.XXXXXX.sh)
trap 'rm -f "$SETUP"' EXIT
cat > "$SETUP" <<'SETUP_SCRIPT_VERBATIM_EOF'
BODY

# The embedded copy: read from disk so it can never drift from the real thing.
cat "$SETUP_SRC"

cat <<'BODY2'
SETUP_SCRIPT_VERBATIM_EOF

log "Connecting this box to the Storage Box (embedded setup-restic-backup.sh)..."
SB_PASS="$SB_PASS" bash "$SETUP" \
  --host "$SB_HOST" --user "$SB_USER" --port "$SB_PORT" \
  --remote "$REMOTE" --path "$REPO_PATH" --password \
  --snapshots "$R_SNAPSHOTS" --daily "$R_DAILY" --weekly "$R_WEEKLY" \
  --monthly "$R_MONTHLY" --yearly "$R_YEARLY" \
  || die "Storage Box connection failed — nothing has been restored. Fix the connection and re-run."

if [ "$MODE" = connect ]; then
  log "--connect-only: Storage Box wired up, no users touched."
  exit 0
fi

# ---------------------------------------------------------------- check mode
fail=0
if [ "$MODE" = check ]; then
  log "Verifying every repo opens with the key in this file. Nothing will be written."
  for pair in "${USERS[@]}"; do
    u=${pair%%:*}; k=${pair#*:}
    selected "$u" || continue
    kf=$(mktemp); printf '%s\n' "$k" > "$kf"
    out=$(restic -r "$REPO/$u" --password-file "$kf" snapshots --latest 1 2>&1)
    rc=$?
    rm -f "$kf"
    if [ $rc -eq 0 ]; then
      line=$(grep -E '^[0-9a-f]{8} ' <<< "$out" | tail -1)
      printf '  OK    %-20s %s\n' "$u" "${line:-(no snapshots yet)}"
    else
      printf '  FAIL  %-20s %s\n' "$u" "$(tail -1 <<< "$out")"
      fail=1
    fi
  done
  [ $fail -eq 0 ] && log "All repos readable. This artifact is good." \
                  || log "SOME REPOS FAILED — do not rely on this artifact until resolved."
  exit $fail
fi

# ---------------------------------------------------------------- full restore
log "FULL RESTORE onto $(hostname -f). This creates users and writes data."
log "Users: $(for p in "${USERS[@]}"; do u=${p%%:*}; selected "$u" && printf '%s ' "$u"; done)"
printf '[restore] Type YES to proceed: '
read -r confirm
[ "$confirm" = "YES" ] || die "aborted."

ok=0; bad=0
for pair in "${USERS[@]}"; do
  u=${pair%%:*}; k=${pair#*:}
  selected "$u" || continue
  log "=== $u ==="
  if /usr/local/hestia/bin/v-restore-user-full-restic "$u" latest "$k"; then
    log "$u restored."; ok=$((ok+1))
  else
    log "$u FAILED — continuing with the rest."; bad=$((bad+1))
  fi
done

log "Done. restored=$ok failed=$bad"
cat <<'NEXT'

Post-restore checks — the restore does not do these for you:
  1. IPs: users were restored onto THIS box's IP. Check v-list-web-domains per
     user and re-point DNS, or the sites answer on the old address.
  2. SSL: Let's Encrypt certs restore as files but may need re-issuing once DNS
     points here (v-add-letsencrypt-domain).
  3. Mail: DKIM keys restore, but SPF/DMARC/MX records live in DNS.
  4. Cron: restored per user; confirm nothing double-fires if the old box is
     still alive.
  5. Passwords: each user's Hestia login was transiently set to its restic key,
     then overwritten by the restored user.conf. Verify a login before assuming.
  6. Turn the backup schedule back on — restoring does not schedule anything.
NEXT
[ $bad -eq 0 ]
BODY2
}

if [ "$TO_STDOUT" -eq 1 ]; then
  emit
else
  emit > "$OUT" || die "failed writing $OUT"
  chmod 600 "$OUT"
  log "Wrote $OUT (mode 600)."
  log "Users included: ${#PAIRS[@]}  |  repo: $REPO"
  log "This file contains all restic keys + the Storage Box password — put it in the vault and delete the local copy."
fi
