#!/bin/bash
# setup-restic-backup — onboard THIS HestiaCP box to a shared Hetzner Storage Box
# for restic incremental backups (drives Hestia's own v-backup-user-restic).
#
# DELIBERATELY NOT a v-script (no v- prefix): it's a one-time root setup task, so
# it stays off the streamer allowlist and off install-scripts.sh's v-* glob — the
# only way to run it is a root shell on the box. Idempotent: safe to re-run, and
# you're MEANT to re-run it after authorizing the key on the Storage Box.
#
# What it does, in order:
#   1. Generates a DEDICATED ed25519 key for this box (/root/.ssh/storagebox) —
#      per-box key, not root's general key, so it's independently revocable.
#      (Key auth only; --password mode skips straight to step 4.)
#   2. Pins the Storage Box host key into /root/.ssh/known_hosts, and verifies
#      the key actually landed.
#   3. Preflights which auth methods the Storage Box offers this user, so a
#      subaccount with SSH disabled is named as such instead of surfacing later
#      as a generic "can't connect". Then, in key mode, verifies the key is
#      authorized (installing it interactively if not).
#   4. Creates an rclone sftp remote to the Storage Box and proves it connects.
#   5. Registers a PER-BOX restic repo with Hestia (v-add-backup-host-restic),
#      which also flips the SYSTEM BACKUP_INCREMENTAL=yes.
#   6. Flips the PER-USER BACKUPS_INCREMENTAL=yes (note the plural) in every
#      package and every existing user.conf — without it every per-user backup
#      dies with "incremental backups are disabled". See the block at the end.
#
# NOTE on the repo path: HestiaCP >= 1.10.4 strips any trailing slash in
# v-add-backup-host-restic and builds the per-user repo as "${REPO%/}/$user"
# (upstream #5100), so conf/restic.conf will show your --path WITHOUT the
# trailing slash this script passes. That is correct and expected. It also means
# an EMPTY path ('--path /') yields "rclone:<remote>:/<user>" — an ABSOLUTE path
# that a chrooted Storage Box does NOT resolve to the same place as the relative
# "<user>". Always give a real per-box --path (the default does).
#
# Usage:
#   Main account (key auth):
#     setup-restic-backup.sh --host uXXXXX.your-storagebox.de --user uXXXXX
#   Subaccount (password auth — subaccounts don't support install-ssh-key, and
#   use their OWN hostname uXXXXX-subN.your-storagebox.de):
#     setup-restic-backup.sh --host uXXXXX-sub1.your-storagebox.de \
#                            --user uXXXXX-sub1 --path / --password
#
# Options (defaults in brackets):
#   --password      use the Storage Box password (prompted, or SB_PASS env)
#                   instead of an SSH key — REQUIRED for subaccounts
#   --port N        Storage Box SSH port [23 — Hetzner default]
#   --key PATH      dedicated key path [/root/.ssh/storagebox]
#   --remote NAME   rclone remote name [storagebox]
#   --path P        repo dir on the box, RELATIVE to its /home [hestia-<shorthostname>/]
#                   — keep it per-box so multiple boxes don't collide on one
#                   Storage Box. Leading slashes are stripped (the SFTP chroot
#                   only addresses /home-relative paths); '--path /' = home root.
#   --snapshots N --daily N --weekly N --monthly N --yearly N
#                   restic retention [-1 14 8 6 1]  (-1 = disable that tier)

set -uo pipefail

log() { echo "[setup-restic] $*"; }
die() { echo "[setup-restic] ERROR: $*" >&2; exit 1; }

# ---- defaults ----
HOST="" USER_SB="" PORT=23
KEY=/root/.ssh/storagebox
REMOTE=storagebox
SHORTHOST=$(hostname -s 2>/dev/null || hostname 2>/dev/null || echo host)
REPO_PATH="hestia-${SHORTHOST}/"   # RELATIVE to the Storage Box home (/home) — see normalisation below
SNAPSHOTS=-1 DAILY=14 WEEKLY=8 MONTHLY=6 YEARLY=1
PASSWORD_AUTH=0

# ---- args ----
while [ $# -gt 0 ]; do
  case "$1" in
    --host)      HOST=${2:-};      shift 2 ;;
    --user)      USER_SB=${2:-};   shift 2 ;;
    --port)      PORT=${2:-};      shift 2 ;;
    --key)       KEY=${2:-};       shift 2 ;;
    --remote)    REMOTE=${2:-};    shift 2 ;;
    --path)      REPO_PATH=${2:-}; shift 2 ;;
    --snapshots) SNAPSHOTS=${2:-}; shift 2 ;;
    --daily)     DAILY=${2:-};     shift 2 ;;
    --weekly)    WEEKLY=${2:-};    shift 2 ;;
    --monthly)   MONTHLY=${2:-};   shift 2 ;;
    --yearly)    YEARLY=${2:-};    shift 2 ;;
    --password)  PASSWORD_AUTH=1;  shift ;;
    -h|--help)   tail -n +2 "$0" | grep '^#' | sed 's/^# \?//'; exit 0 ;;
    *)           die "unknown argument: $1 (see --help)" ;;
  esac
done

# ---- guards ----
[ "${EUID:-$(id -u)}" -eq 0 ] || die "must run as root."
[ -n "$HOST" ] && [ -n "$USER_SB" ] || die "need --host and --user (your Storage Box host + username). See --help."
command -v rclone >/dev/null 2>&1 || die "rclone not found. Install: curl https://rclone.org/install.sh | bash"
command -v ssh-keyscan >/dev/null 2>&1 || die "ssh-keyscan not found (openssh-client)."
[ -x /usr/local/hestia/bin/v-add-backup-host-restic ] || die "v-add-backup-host-restic missing — update HestiaCP."

# Normalise the repo path RELATIVE to the Storage Box home. A Hetzner Storage
# Box SFTP session is chrooted and addresses everything relative to /home; an
# absolute '/path' escapes the writable area and lists empty (which reads as
# "can't connect"). So strip any leading slash the operator passed (e.g.
# '--path /' → home root) and keep a single trailing slash.
while case "$REPO_PATH" in /*) true ;; *) false ;; esac; do REPO_PATH="${REPO_PATH#/}"; done
case "$REPO_PATH" in ""|*/) ;; *) REPO_PATH="$REPO_PATH/" ;; esac

mkdir -p /root/.ssh && chmod 700 /root/.ssh

# ---- pin Storage Box host key (both auth modes) ----
# ssh-keyscan exits 0 even when it gets nothing back, so confirm the key actually
# landed rather than logging "Pinned" and failing confusingly later: rclone is
# given known_hosts_file, so an empty known_hosts turns into an unrelated-looking
# connection error several steps further down.
if ! ssh-keygen -F "[$HOST]:$PORT" >/dev/null 2>&1; then
  ssh-keyscan -p "$PORT" "$HOST" 2>/dev/null >> /root/.ssh/known_hosts
  if ssh-keygen -F "[$HOST]:$PORT" >/dev/null 2>&1; then
    log "Pinned Storage Box host key ([$HOST]:$PORT) into known_hosts."
  else
    die "ssh-keyscan returned no host key for [$HOST]:$PORT.
     Check the hostname is right and port $PORT is reachable from this box."
  fi
fi

# ---- preflight: does the Storage Box offer this user ANY auth method? ----
# A Hetzner SUBACCOUNT with SSH not enabled in the console still exists and still
# accepts TCP on :23 — it just advertises an EMPTY auth-method list, so every
# credential you hand it fails identically. Without this probe that surfaces as
# rclone's "couldn't connect", which reads like a network or path fault and sends
# you debugging the wrong layer entirely (cost us a full round trip on
# u626683-sub3, 2026-09-07). openssh names the cause in one line; ask it first.
methods=""
ssh_v=$(ssh -p "$PORT" -o BatchMode=yes -o StrictHostKeyChecking=accept-new \
            -o ConnectTimeout=10 -v "$USER_SB@$HOST" exit 2>&1)
if grep -q 'Authentications that can continue' <<< "$ssh_v"; then
  methods=$(sed -n 's/.*Authentications that can continue: //p' <<< "$ssh_v" | head -1 | tr -d '[:space:]')
  if [ -z "$methods" ]; then
    die "The Storage Box offers NO authentication methods for '$USER_SB'.
     That is exactly what a Hetzner subaccount looks like when SSH is not enabled on it.
     Fix: Hetzner console -> Storage Box -> Subaccounts -> $USER_SB -> enable SSH
     (leave 'external reachability' on too), give it a minute, then re-run this script."
  fi
  log "Storage Box offers auth methods for $USER_SB: $methods"
  if [ "$PASSWORD_AUTH" -eq 1 ] && ! grep -q password <<< "$methods"; then
    die "Storage Box offers [$methods] for '$USER_SB' but not 'password' — --password cannot work here."
  fi
fi
# No "Authentications that can continue" line at all means we never reached the
# auth stage (DNS/route/port). Say nothing here and let the rclone probe below
# diagnose it, since that path has the IPv4 fallback.

# ---- authentication (rclone secret args differ by mode) ----
rclone_secret=()

if [ "$PASSWORD_AUTH" -eq 1 ]; then
  # Password auth — REQUIRED for Hetzner SUBACCOUNTS: they don't support
  # install-ssh-key (it returns "Internal Error 006"), and they use their OWN
  # hostname (uXXXXXX-subN.your-storagebox.de) + the subaccount user. rclone
  # stores the password obscured in root-only rclone.conf; the backup data is
  # restic-encrypted regardless, and the subaccount is scoped to its own dir.
  if [ -z "${SB_PASS:-}" ]; then
    [ -t 0 ] || die "--password needs a terminal to prompt (or pass it via the SB_PASS env var)."
    read -rs -p "[setup-restic] Storage Box password for $USER_SB@$HOST: " SB_PASS; echo
  fi
  [ -n "$SB_PASS" ] || die "empty password."
  rclone_secret=(pass="$(rclone obscure "$SB_PASS")")
  log "Using password auth (subaccount mode)."
else
  # Key auth — for the MAIN account (supports install-ssh-key).
  if [ ! -f "$KEY" ]; then
    ssh-keygen -t ed25519 -N '' -f "$KEY" -C "hestia-${SHORTHOST}-backup" >/dev/null \
      || die "ssh-keygen failed."
    log "Generated dedicated backup key: $KEY"
  else
    log "Using existing key: $KEY"
  fi

  sb_key_works() {
    # Run 'pwd', not 'exit' — a Storage Box has no shell, only a fixed command
    # allowlist (pwd/ls/mkdir/…). 'exit' isn't on it, so it'd return non-zero
    # even when the key IS authorized, sending us into a needless reinstall.
    ssh -p "$PORT" -i "$KEY" -o BatchMode=yes -o ConnectTimeout=10 \
        -o StrictHostKeyChecking=accept-new "$USER_SB@$HOST" pwd >/dev/null 2>&1
  }
  if sb_key_works; then
    log "Key already authorized on the Storage Box."
  elif [ -t 0 ]; then
    log "Key not yet authorized. Installing it now — you'll be asked for the Storage Box password ONCE."
    if cat "$KEY.pub" | ssh -p "$PORT" -o StrictHostKeyChecking=accept-new "$USER_SB@$HOST" install-ssh-key; then
      sb_key_works || die "key installed but auth still fails — double-check --user/--host."
      log "Key installed and verified."
    else
      die "install-ssh-key failed. If this is a SUBACCOUNT it isn't supported there — re-run with --password (and the subaccount's own uXXXXXX-subN host). Otherwise enable SSH support on the Storage Box."
    fi
  else
    log "Key not authorized, and not a terminal to prompt. Run this once then re-run me:"
    log "    cat $KEY.pub | ssh -p $PORT $USER_SB@$HOST install-ssh-key"
    exit 1
  fi
  rclone_secret=(key_file="$KEY")
fi

# ---- rclone remote (idempotent; delete+recreate) ----
rclone config delete "$REMOTE" >/dev/null 2>&1 || true
if ! rclone config create "$REMOTE" sftp \
        host="$HOST" user="$USER_SB" port="$PORT" \
        "${rclone_secret[@]}" known_hosts_file=/root/.ssh/known_hosts >/dev/null 2>&1; then
  log "WARN: rclone rejected known_hosts_file (older rclone?) — recreating without it."
  rclone config delete "$REMOTE" >/dev/null 2>&1 || true
  rclone config create "$REMOTE" sftp \
        host="$HOST" user="$USER_SB" port="$PORT" "${rclone_secret[@]}" >/dev/null \
    || die "rclone remote creation failed."
fi
log "rclone remote '$REMOTE:' configured."

# Connectivity check with an IPv4 fallback. Test against the remote's HOME
# (bare "$REMOTE:", which resolves to /home) — NOT the repo subdir — because
# that's the one path always addressable through the chroot. Hetzner Storage Box
# SSH (:23) is refused over IPv6 on many boxes, and rclone's Go SSH dials the
# AAAA record without falling back to IPv4 the way openssh does; if the first
# attempt fails, pin the host to its IPv4 in /etc/hosts (host key stays valid —
# still keyed on the hostname) and retry.
probe_err=""
# --retries/--low-level-retries 1 are NOT tuning, they are brute-force-ban
# avoidance. rclone's default pacer retries an auth failure ~10 times, so ONE
# bad-credential `rclone lsd` looks like ten failed logins to the Storage Box.
# Two of those in a debugging session is enough for Hetzner to block the box's
# IP outright (connection refused, not auth denied) — which is exactly what we
# did to hzdemolink on 2026-09-07 while diagnosing the sub3 SSH toggle. One
# attempt is all a probe ever needs.
probe() { probe_err=$(rclone lsd "$REMOTE:" --retries 1 --low-level-retries 1 --contimeout 15s 2>&1 >/dev/null); }

if ! probe; then
  # Only a genuine DIAL failure is the IPv6 symptom. An auth rejection means the
  # credential or the subaccount's SSH setting is wrong — pinning an IP then
  # changes nothing and leaves misleading litter in /etc/hosts, which is what
  # happened while debugging u626683-sub3 (2026-09-07).
  case "$probe_err" in
    *"unable to authenticate"*|*"handshake failed"*|*"ermission denied"*)
      die "Storage Box refused authentication for '$USER_SB'.
     rclone said: $(tail -1 <<< "$probe_err")
     If this is a subaccount, confirm SSH is enabled for it in the Hetzner console
     and that the password is current. Debug: rclone lsd $REMOTE: -vv" ;;
  esac
  ipv4=$(getent ahostsv4 "$HOST" 2>/dev/null | awk '{print $1; exit}')
  if [ -n "$ipv4" ] && ! grep -qF " $HOST" /etc/hosts; then
    echo "$ipv4 $HOST" >> /etc/hosts
    log "rclone couldn't connect (likely IPv6) — pinned $HOST -> $ipv4 in /etc/hosts, retrying."
  fi
  probe || die "rclone can't reach $REMOTE: (home dir).
     rclone said: $(tail -1 <<< "$probe_err")
     Debug: rclone lsd $REMOTE: -vv"
fi

# Home reachable — now ensure the per-box repo dir exists (relative to /home).
if [ -n "$REPO_PATH" ]; then
  rclone mkdir "$REMOTE:$REPO_PATH" 2>/dev/null || true
  rclone lsd "$REMOTE:$REPO_PATH" >/dev/null 2>&1 \
    || die "connected to $REMOTE:, but can't create/list '$REPO_PATH'. Check the subaccount can write under /home."
fi
log "Verified rclone can reach $REMOTE:$REPO_PATH"

# ---- 5. register the per-box repo with Hestia ----
REPO="rclone:$REMOTE:$REPO_PATH"
log "Registering restic repo with Hestia: $REPO"
/usr/local/hestia/bin/v-add-backup-host-restic "$REPO" "$SNAPSHOTS" "$DAILY" "$WEEKLY" "$MONTHLY" "$YEARLY" \
  || die "v-add-backup-host-restic failed."

# ---- 6. enable PER-USER incremental backups ----
# v-add-backup-host-restic sets the SYSTEM flag (BACKUP_INCREMENTAL in
# hestia.conf) but NOT the per-user one. v-backup-user-restic checks
# BACKUPS_INCREMENTAL (note the plural) in each user's user.conf and refuses with
# "incremental backups are disabled" when it's 'no' — the Hestia default. It's a
# package attribute with no standalone setter, so flip it directly: in every
# package (.pkg) so future/rebuilt users inherit 'yes', and in every existing
# user.conf so it takes effect now (the check greps user.conf live). Surgical —
# only this one key changes, so no other package limits are disturbed (which a
# v-change-user-package reapply WOULD do). Add the key if a file lacks it.
set_incremental_yes() {
  local file=$1
  [ -f "$file" ] || return 0
  if grep -q "^BACKUPS_INCREMENTAL=" "$file"; then
    sed -i "s/^BACKUPS_INCREMENTAL=.*/BACKUPS_INCREMENTAL='yes'/" "$file"
  else
    echo "BACKUPS_INCREMENTAL='yes'" >> "$file"
  fi
}
log "Enabling per-user incremental backups (BACKUPS_INCREMENTAL=yes) — packages + existing users..."
for pkg in /usr/local/hestia/data/packages/*.pkg; do set_incremental_yes "$pkg"; done
for uc  in /usr/local/hestia/data/users/*/user.conf; do set_incremental_yes "$uc";  done

FIRST_USER=$(v-list-users plain 2>/dev/null | awk 'NR==1{print $1}')
FIRST_USER=${FIRST_USER:-<user>}

log "Done. Incremental backups now target ${REPO}<user>."
cat <<EOF

Next steps — do these; they're the difference between a backup and a false sense of one:

  1. Test one user (streams to the box, no 2x-space error):
       v-backup-user-restic $FIRST_USER
     Then CONFIRM THE DATABASE IS IN THE SNAPSHOT (a WP backup without the DB is useless):
       restic -r ${REPO}$FIRST_USER --password-file /usr/local/hestia/data/users/$FIRST_USER/restic.conf snapshots
       restic -r ${REPO}$FIRST_USER --password-file /usr/local/hestia/data/users/$FIRST_USER/restic.conf ls latest | grep -i '\.sql'

  2. Enable Storage Box SNAPSHOTS in the Hetzner console — deletion backstop. A
     compromised box can delete its own repo; the Storage Box snapshot survives it.

  3. Copy the restic ENCRYPTION KEYS off this box — without them the repo is
     unrecoverable, and they live on the very box being backed up:
       tar czf /root/restic-keys-${SHORTHOST}.tgz /usr/local/hestia/data/users/*/restic.conf
     then move that archive somewhere independent (NOT onto this same Storage Box).

  4. Keep the classic tarball backups running until you've done a real test restore.
EOF
