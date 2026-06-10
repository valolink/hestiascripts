#!/bin/bash
# v-wp-fix-permissions — fix file ownership and permissions for a HestiaCP WordPress domain

if [ "$EUID" -ne 0 ]; then
  echo "ERROR: Please run as root."
  exit 1
fi

export PATH=$PATH:/usr/local/hestia/bin

show_help() {
  cat <<'EOF'
USAGE: v-wp-fix-permissions [OPTIONS]

Fix file ownership and permissions for a WordPress site on HestiaCP.

  Directories : 755  (owner rwx, group/other rx)
  Files       : 644  (owner rw, group/other r)
  wp-config   : 640  (owner rw, group r, other none)
  uploads     : 755  (and all subdirectories)

OPTIONS:
  --user=USER       HestiaCP user who owns the domain
  --domain=DOMAIN   Domain name
  -h, --help        Show this help
EOF
}

WEB_USER=""
DOMAIN=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --user=*)   WEB_USER="${1#*=}" ;;
    --domain=*) DOMAIN="${1#*=}" ;;
    -h|--help)  show_help; exit 0 ;;
    *) echo "ERROR: Unknown option: $1"; show_help; exit 1 ;;
  esac
  shift
done

if [ -z "$WEB_USER" ]; then
  mapfile -t _USERS < <(v-list-users plain 2>/dev/null | cut -f1)
  echo ""
  PS3="Select user: "
  select WEB_USER in "${_USERS[@]}"; do
    [ -n "$WEB_USER" ] && break
    echo "Invalid selection."
  done
fi

if [ -z "$DOMAIN" ]; then
  echo ""
  mapfile -t _DOMAINS < <(v-list-web-domains "$WEB_USER" plain 2>/dev/null | cut -f1)
  if [ ${#_DOMAINS[@]} -eq 0 ]; then
    echo "ERROR: No web domains found for user $WEB_USER."
    exit 1
  fi
  PS3="Select domain: "
  select DOMAIN in "${_DOMAINS[@]}"; do
    [ -n "$DOMAIN" ] && break
    echo "Invalid selection."
  done
fi

DOMAIN="${DOMAIN#www.}"
WEB_DIR="/home/$WEB_USER/web/$DOMAIN/public_html"

if [ ! -d "$WEB_DIR" ]; then
  echo "ERROR: $WEB_DIR does not exist."
  exit 1
fi

echo ""
echo "Fixing permissions: $WEB_DIR"
echo "  Owner: $WEB_USER:$WEB_USER"

chown -R "$WEB_USER:$WEB_USER" "$WEB_DIR"
find "$WEB_DIR" -type d -exec chmod 755 {} \;
find "$WEB_DIR" -type f -exec chmod 644 {} \;
[ -f "$WEB_DIR/wp-config.php" ] && chmod 640 "$WEB_DIR/wp-config.php"
find "$WEB_DIR/wp-content/uploads" -type d -exec chmod 755 {} \; 2>/dev/null

echo "  Done."
