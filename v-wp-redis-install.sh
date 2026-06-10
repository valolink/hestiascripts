#!/bin/bash

if [ "$EUID" -ne 0 ]; then
  echo "❌ ERROR: Please run as root."
  exit 1
fi

export PATH=$PATH:/usr/local/hestia/bin

show_help() {
  cat <<'EOF'
USAGE: v-wp-redis-install [OPTIONS]

Install the Redis Object Cache plugin on a WordPress site, set a unique
WP_REDIS_PREFIX in wp-config.php, and activate the plugin.

Redis is NOT enabled (no object-cache.php drop-in is created). Enable it
manually in the plugin settings or with: wp redis enable --path=...

OPTIONS:
  --user=USER      HestiaCP user who owns the site
  --domain=DOMAIN  Domain to configure
  -h, --help       Show this help

EXAMPLES:
  # Interactive
  v-wp-redis-install

  # Direct (e.g. via hestia-streamer)
  v-wp-redis-install --user=admin --domain=mysite.fi
EOF
}

# --- Flag Parsing ---
HESTIA_USER=""
DOMAIN=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --user=*)   HESTIA_USER="${1#*=}" ;;
    --domain=*) DOMAIN="${1#*=}" ;;
    -h|--help)  show_help; exit 0 ;;
    *) echo "❌ ERROR: Unknown option: $1"; exit 1 ;;
  esac
  shift
done

# --- Interactive prompts for missing values ---
if [ -z "$HESTIA_USER" ]; then
  mapfile -t USERS < <(v-list-users plain | cut -f1)
  PS3="Select user: "
  select HESTIA_USER in "${USERS[@]}"; do
    [ -n "$HESTIA_USER" ] && break
    echo "Invalid selection."
  done
fi

if [ -z "$DOMAIN" ]; then
  mapfile -t DOMAINS < <(v-list-web-domains "$HESTIA_USER" plain | cut -f1)
  if [ ${#DOMAINS[@]} -eq 0 ]; then
    echo "❌ ERROR: No web domains found for user $HESTIA_USER."
    exit 1
  fi
  PS3="Select domain: "
  select DOMAIN in "${DOMAINS[@]}"; do
    [ -n "$DOMAIN" ] && break
    echo "Invalid selection."
  done
fi

DOMAIN="${DOMAIN#www.}"
WP_PATH="/home/$HESTIA_USER/web/$DOMAIN/public_html"
WP="sudo -u $HESTIA_USER wp --path=$WP_PATH"

if [ ! -f "$WP_PATH/wp-config.php" ]; then
  echo "❌ ERROR: No WordPress installation found at $WP_PATH"
  exit 1
fi

if ! command -v wp &>/dev/null; then
  echo "❌ ERROR: WP-CLI is not installed."
  exit 1
fi

echo ""
echo "======================================================"
echo "  Redis Object Cache setup: $DOMAIN"
echo "======================================================"

# --- Install plugin ---
echo ""
PLUGIN_STATUS=$($WP plugin status redis-cache 2>/dev/null | grep -oP '(?<=Status: )\S+')

if [ -n "$PLUGIN_STATUS" ]; then
  echo "ℹ️  Plugin already installed (status: $PLUGIN_STATUS)"
else
  echo "Installing redis-cache plugin..."
  $WP plugin install redis-cache 2>&1
  if [ $? -ne 0 ]; then
    echo "❌ ERROR: Plugin installation failed."
    exit 1
  fi
  echo "✅ Plugin installed"
fi

# --- Activate plugin ---
echo ""
IS_ACTIVE=$($WP plugin status redis-cache 2>/dev/null | grep -c "Status: Active")

if [ "$IS_ACTIVE" -gt 0 ]; then
  echo "ℹ️  Plugin is already active"
else
  echo "Activating redis-cache plugin..."
  $WP plugin activate redis-cache 2>&1
  if [ $? -ne 0 ]; then
    echo "❌ ERROR: Plugin activation failed."
    exit 1
  fi
  echo "✅ Plugin activated"
fi

# --- WP_REDIS_PREFIX ---
echo ""
HAS_PREFIX=$($WP config has WP_REDIS_PREFIX 2>/dev/null; echo $?)

if [ "$HAS_PREFIX" -eq 0 ]; then
  EXISTING_PREFIX=$($WP config get WP_REDIS_PREFIX 2>/dev/null)
  echo "ℹ️  WP_REDIS_PREFIX already set: $EXISTING_PREFIX"
else
  PREFIX_BASE=$(echo "$DOMAIN" | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9]/_/g' | cut -c1-20)
  PREFIX="${PREFIX_BASE}_$(openssl rand -hex 4)"
  echo "Setting WP_REDIS_PREFIX = $PREFIX ..."
  $WP config set WP_REDIS_PREFIX "$PREFIX" --type=constant 2>&1
  if [ $? -ne 0 ]; then
    echo "❌ ERROR: Failed to set WP_REDIS_PREFIX in wp-config.php"
    exit 1
  fi
  echo "✅ WP_REDIS_PREFIX set"
fi

# --- Summary ---
echo ""
echo "======================================================"
echo "  Done: $DOMAIN"
echo "======================================================"
echo "  Redis Object Cache plugin is installed and active."
echo "  The object cache drop-in is NOT enabled yet."
echo ""
echo "  To enable Redis, run:"
echo "    sudo -u $HESTIA_USER wp redis enable --path=$WP_PATH"
echo "  Or use the plugin's settings page in WP Admin."
echo "======================================================"
