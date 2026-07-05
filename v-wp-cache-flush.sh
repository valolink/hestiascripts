#!/bin/bash
# v-wp-cache-flush — flush every cache layer for a HestiaCP WordPress domain
#
# Layers, in order:
#   1. WordPress object cache (Redis via the drop-in) — wp cache flush
#   2. Transients                                     — wp transient delete --expired
#   3. WP Rocket page cache (if active)               — wp rocket clean --confirm
#   4. nginx FastCGI/proxy cache                      — v-purge-nginx-cache
#
# Safe to run any time; each layer is best-effort so a missing plugin or
# disabled cache never fails the whole flush.

if [ "$EUID" -ne 0 ]; then
  echo "ERROR: Please run as root."
  exit 1
fi

export PATH=$PATH:/usr/local/hestia/bin

show_help() {
  cat <<'EOF'
USAGE: v-wp-cache-flush [OPTIONS]

Flush object cache (Redis), expired transients, WP Rocket page cache and
the nginx cache for a WordPress site on HestiaCP.

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

if [ ! -f "$WEB_DIR/wp-config.php" ]; then
  echo "ERROR: $WEB_DIR does not look like a WordPress install."
  exit 1
fi

wp_cmd() {
  sudo -u "$WEB_USER" wp --path="$WEB_DIR" "$@" 2>&1
}

echo "Flushing caches for $DOMAIN ($WEB_USER)"

echo "→ Object cache (Redis)..."
wp_cmd cache flush || echo "  (object cache flush failed — no drop-in?)"

echo "→ Expired transients..."
wp_cmd transient delete --expired || echo "  (transient cleanup failed)"

if wp_cmd plugin is-active wp-rocket > /dev/null; then
  echo "→ WP Rocket page cache..."
  wp_cmd rocket clean --confirm || echo "  (rocket clean failed)"
else
  echo "→ WP Rocket not active — skipping."
fi

echo "→ nginx cache..."
v-purge-nginx-cache "$WEB_USER" "$DOMAIN" 2>&1 || echo "  (nginx purge failed — cache not enabled?)"

echo "Done — all cache layers flushed."
