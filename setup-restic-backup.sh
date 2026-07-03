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
#   2. Pins the Storage Box host key into /root/.ssh/known_hosts.
#   3. Verifies the key is authorized on the Storage Box. If not, installs it
#      (interactive — prompts once for the Storage Box password) or prints how.
#   4. Creates an rclone sftp remote to the Storage Box.
#   5. Registers a PER-BOX restic repo with Hestia (v-add-backup-host-restic),
#      which also flips BACKUP_INCREMENTAL=yes.
#
# Usage:
#   setup-restic-backup.sh --host uXXXXX.your-storagebox.de --user uXXXXX [opts]
#
# Options (defaults in brackets):
#   --port N        Storage Box SSH port [23 — Hetzner default]
#   --key PATH      dedicated key path [/root/.ssh/storagebox]
#   --remote NAME   rclone remote name [storagebox]
#   --path P        repo path prefix on the box [/hestia-<shorthostname>/] — keep
#                   it per-box so multiple boxes don't collide on one Storage Box
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
REPO_PATH="/hestia-${SHORTHOST}/"
SNAPSHOTS=-1 DAILY=14 WEEKLY=8 MONTHLY=6 YEARLY=1

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

# normalise repo path to /.../ form
case "$REPO_PATH" in /*) ;; *) REPO_PATH="/$REPO_PATH" ;; esac
case "$REPO_PATH" in */) ;; *) REPO_PATH="$REPO_PATH/" ;; esac

mkdir -p /root/.ssh && chmod 700 /root/.ssh

# ---- 1. dedicated key ----
if [ ! -f "$KEY" ]; then
  ssh-keygen -t ed25519 -N '' -f "$KEY" -C "hestia-${SHORTHOST}-backup" >/dev/null \
    || die "ssh-keygen failed."
  log "Generated dedicated backup key: $KEY"
else
  log "Using existing key: $KEY"
fi

# ---- 2. pin Storage Box host key ----
if ! ssh-keygen -F "[$HOST]:$PORT" >/dev/null 2>&1; then
  ssh-keyscan -p "$PORT" "$HOST" 2>/dev/null >> /root/.ssh/known_hosts
  log "Pinned Storage Box host key ([$HOST]:$PORT) into known_hosts."
fi

# ---- 3. verify / install the authorized key ----
sb_key_works() {
  ssh -p "$PORT" -i "$KEY" -o BatchMode=yes -o ConnectTimeout=10 \
      -o StrictHostKeyChecking=accept-new "$USER_SB@$HOST" exit 2>/dev/null
}

if sb_key_works; then
  log "Key already authorized on the Storage Box."
elif [ -t 0 ]; then
  log "Key not yet authorized. Installing it now — you'll be asked for the Storage Box password ONCE."
  if cat "$KEY.pub" | ssh -p "$PORT" -o StrictHostKeyChecking=accept-new "$USER_SB@$HOST" install-ssh-key; then
    sb_key_works || die "key installed but auth still fails — double-check --user/--host."
    log "Key installed and verified."
  else
    die "install-ssh-key failed. Enable SSH support on the Storage Box (Hetzner console) and retry."
  fi
else
  log "Key not authorized, and not a terminal (can't prompt for the Storage Box password)."
  log "Run this once, then re-run me:"
  log "    cat $KEY.pub | ssh -p $PORT $USER_SB@$HOST install-ssh-key"
  exit 1
fi

# ---- 4. rclone remote (idempotent; delete+recreate) ----
rclone config delete "$REMOTE" >/dev/null 2>&1 || true
if ! rclone config create "$REMOTE" sftp \
        host="$HOST" user="$USER_SB" port="$PORT" \
        key_file="$KEY" known_hosts_file=/root/.ssh/known_hosts >/dev/null 2>&1; then
  log "WARN: rclone rejected known_hosts_file (older rclone?) — recreating without it."
  rclone config delete "$REMOTE" >/dev/null 2>&1 || true
  rclone config create "$REMOTE" sftp \
        host="$HOST" user="$USER_SB" port="$PORT" key_file="$KEY" >/dev/null \
    || die "rclone remote creation failed."
fi
log "rclone remote '$REMOTE:' configured."

rclone mkdir "$REMOTE:$REPO_PATH" 2>/dev/null || true
rclone lsd "$REMOTE:$REPO_PATH" >/dev/null 2>&1 \
  || die "rclone can't reach $REMOTE:$REPO_PATH — check Storage Box path/permissions."
log "Verified rclone can write to $REMOTE:$REPO_PATH"

# ---- 5. register the per-box repo with Hestia ----
REPO="rclone:$REMOTE:$REPO_PATH"
log "Registering restic repo with Hestia: $REPO"
/usr/local/hestia/bin/v-add-backup-host-restic "$REPO" "$SNAPSHOTS" "$DAILY" "$WEEKLY" "$MONTHLY" "$YEARLY" \
  || die "v-add-backup-host-restic failed."

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
