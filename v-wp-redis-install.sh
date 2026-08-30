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

Also sets two constants that keep the cache from becoming the bottleneck:
  WP_REDIS_DISABLE_GROUP_FLUSH=true  group flush uses FLUSHDB (O(1)) instead
                                     of a Lua SCAN over the whole keyspace
  WP_REDIS_MAXTTL=86400              keys expire, so the keyspace stays bounded
Existing values are never overwritten.

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

# --- Cache-behaviour constants -------------------------------------------------
#
# Both of these exist because of kuumalahde.fi on 2026-08-30. Redis went live
# there on 2026-08-27 with only WP_REDIS_PREFIX set, and by the next night the
# box was serving 504s from 21:00 to 06:00 every night.
#
# WP_REDIS_DISABLE_GROUP_FLUSH — the redis-cache plugin's flush_group() runs a
#   Lua SCAN across the ENTIRE keyspace on every call, and nothing gates that
#   except this constant. Not WP_REDIS_SELECTIVE_FLUSH, not the prefix, not the
#   database. On kuumalahde that was 392,916 calls in 46h at ~173ms each —
#   18.9 hours of Redis CPU, or 41% of a core doing nothing but flushing.
#   Redis is single-threaded, so every one of those scans blocked all other
#   cache reads until PHP timed out. With this set, flush_group() falls back to
#   flush() → FLUSHDB, which is O(1). It invalidates MORE than asked (whole DB,
#   not one group), so it is conservative: more cache misses, never staleness.
#
# WP_REDIS_MAXTTL — without it, cache keys never expire and the keyspace only
#   grows. kuumalahde reached 406,234 keys with just 4,342 carrying a TTL, which
#   is what made each scan so expensive. 86400 (one day) bounds it.
#
# Small sites never notice either problem: the keyspace stays small, so the scan
# stays cheap. It only bites at WooCommerce scale with a write-heavy import.
# Setting both at install time costs nothing and removes the trap.
set_const_if_missing() {
  local name="$1" value="$2"
  if $WP config has "$name" &>/dev/null; then
    echo "ℹ️  $name already set: $($WP config get "$name" 2>/dev/null)"
    return 0
  fi
  echo "Setting $name = $value ..."
  if $WP config set "$name" "$value" --raw --type=constant 2>&1; then
    echo "✅ $name set"
  else
    echo "⚠️  Could not set $name — set it by hand in wp-config.php"
  fi
}

echo ""
set_const_if_missing WP_REDIS_DISABLE_GROUP_FLUSH true
set_const_if_missing WP_REDIS_MAXTTL 86400

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
