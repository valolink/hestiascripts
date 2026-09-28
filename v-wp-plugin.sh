#!/bin/bash
# v-wp-plugin — run a safe WP-CLI plugin action on a HestiaCP WordPress site
#
# The per-site worker behind EngineLink's bulk plugin actions (vulnerability
# response: a plugin CVE drops → update/deactivate it across every affected
# site from one screen). Action allowlist is deliberate — this is reachable
# over the streamer, so no arbitrary WP-CLI passthrough.

if [ "$EUID" -ne 0 ]; then
  echo "ERROR: Please run as root."
  exit 1
fi

export PATH=$PATH:/usr/local/hestia/bin

show_help() {
  cat <<'EOF'
USAGE: v-wp-plugin --user=USER --domain=DOMAIN --action=ACTION --slug=SLUG

Run a WP-CLI plugin action for one site. Actions: update, deactivate,
activate, status.
EOF
}

WEB_USER=""
DOMAIN=""
ACTION=""
SLUG=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --user=*)   WEB_USER="${1#*=}" ;;
    --domain=*) DOMAIN="${1#*=}" ;;
    --action=*) ACTION="${1#*=}" ;;
    --slug=*)   SLUG="${1#*=}" ;;
    -h|--help)  show_help; exit 0 ;;
    *) echo "ERROR: Unknown option: $1"; show_help; exit 1 ;;
  esac
  shift
done

if [ -z "$WEB_USER" ] || [ -z "$DOMAIN" ] || [ -z "$ACTION" ] || [ -z "$SLUG" ]; then
  echo "ERROR: --user, --domain, --action and --slug are all required."
  exit 1
fi

case "$ACTION" in
  update|deactivate|activate|status) ;;
  *) echo "ERROR: action must be update, deactivate, activate or status."; exit 1 ;;
esac

if ! [[ "$SLUG" =~ ^[a-z0-9][a-z0-9._-]*$ ]]; then
  echo "ERROR: invalid plugin slug."
  exit 1
fi

DOMAIN="${DOMAIN#www.}"
WEB_DIR="/home/$WEB_USER/web/$DOMAIN/public_html"

if [ ! -f "$WEB_DIR/wp-config.php" ]; then
  echo "ERROR: $WEB_DIR does not look like a WordPress install."
  exit 1
fi

wp_cmd() {
  sudo -u "$WEB_USER" wp --path="$WEB_DIR" "$@" 2>&1
}

if ! wp_cmd plugin is-installed "$SLUG"; then
  echo "SKIP: plugin $SLUG is not installed on $DOMAIN."
  exit 0
fi

echo "Running: wp plugin $ACTION $SLUG ($DOMAIN)"
wp_cmd plugin "$ACTION" "$SLUG"
RC=$?

if [ "$RC" -eq 0 ] && [ "$ACTION" != "status" ]; then
  # Stale caches after a plugin change bite hard — flush best-effort.
  wp_cmd cache flush > /dev/null 2>&1
fi

exit $RC
