#!/bin/bash
# v-wp-debug-log — print (or empty) a WordPress site's WP_DEBUG_LOG
#
# EngineLink's update runs empty staging's debug log before the update and
# read it after the page checks, so the review shows the PHP errors the
# update and those page loads produced. v-wp-staging-create points staging's
# WP_DEBUG_LOG at private/wp-debug.log with display off.
#
# The log path comes from the site's own wp-config.php, which the site user
# can edit — so the file is read and truncated AS THE SITE USER, never as
# root: a hostile wp-config pointing WP_DEBUG_LOG at /etc/shadow gets a
# permission error, not the file.

if [ "$EUID" -ne 0 ]; then
  echo "ERROR: Please run as root."
  exit 1
fi

export PATH=$PATH:/usr/local/hestia/bin

show_help() {
  cat <<'EOF'
USAGE: v-wp-debug-log --user=USER --domain=DOMAIN [--truncate]

Print the tail (last 256 KB) of the site's WP_DEBUG_LOG between
DEBUG_LOG_BEGIN / DEBUG_LOG_END marker lines, or empty it with --truncate.
Exits 0 with "DEBUG_LOG_NONE" when the site has no debug log configured.

OPTIONS:
  --user=USER       HestiaCP user who owns the domain
  --domain=DOMAIN   Domain name
  --truncate        Empty the log instead of printing it
  -h, --help        Show this help
EOF
}

WEB_USER=""
DOMAIN=""
TRUNCATE=false
MAX_BYTES=262144

while [[ $# -gt 0 ]]; do
  case "$1" in
    --user=*)   WEB_USER="${1#*=}" ;;
    --domain=*) DOMAIN="${1#*=}" ;;
    --truncate) TRUNCATE=true ;;
    -h|--help)  show_help; exit 0 ;;
    *) echo "ERROR: Unknown option: $1"; show_help; exit 1 ;;
  esac
  shift
done

if [ -z "$WEB_USER" ] || [ -z "$DOMAIN" ]; then
  echo "ERROR: --user and --domain are required."
  exit 1
fi
if ! id "$WEB_USER" >/dev/null 2>&1; then
  echo "ERROR: No such user: $WEB_USER"
  exit 1
fi

DOMAIN="${DOMAIN#www.}"
WEB_DIR="/home/$WEB_USER/web/$DOMAIN/public_html"
if [ ! -f "$WEB_DIR/wp-config.php" ]; then
  echo "ERROR: $WEB_DIR does not look like a WordPress install."
  exit 1
fi

as_user() { sudo -u "$WEB_USER" "$@"; }

# WP_DEBUG_LOG is true (→ wp-content/debug.log), false/unset, or a path.
LOG_VALUE=$(as_user wp --path="$WEB_DIR" config get WP_DEBUG_LOG 2>/dev/null)
case "$LOG_VALUE" in
  ""|0|false) echo "DEBUG_LOG_NONE"; exit 0 ;;
  1|true)     LOG_FILE="$WEB_DIR/wp-content/debug.log" ;;
  *)          LOG_FILE="$LOG_VALUE" ;;
esac

if [ "$TRUNCATE" = true ]; then
  if as_user test -e "$LOG_FILE"; then
    as_user truncate -s 0 "$LOG_FILE" || { echo "ERROR: could not truncate $LOG_FILE"; exit 1; }
  fi
  echo "Debug log emptied: $LOG_FILE"
  exit 0
fi

if ! as_user test -r "$LOG_FILE"; then
  echo "DEBUG_LOG_BEGIN bytes=0 truncated=0"
  echo "DEBUG_LOG_END"
  exit 0
fi

BYTES=$(as_user stat -c %s "$LOG_FILE" 2>/dev/null || echo 0)
CUT=0
[ "$BYTES" -gt "$MAX_BYTES" ] && CUT=1
echo "DEBUG_LOG_BEGIN bytes=$BYTES truncated=$CUT"
as_user tail -c "$MAX_BYTES" "$LOG_FILE"
echo ""
echo "DEBUG_LOG_END"
