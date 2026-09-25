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

Also sets the layout the redis-cache plugin documents:
  WP_REDIS_MAXTTL=86400              keys expire, so the keyspace stays bounded
  WP_REDIS_DATABASE=<free>           a database of its own (skips any database
                                     another wp-config names or that holds keys)
  WP_REDIS_DISABLE_GROUP_FLUSH=true  only with a database of its own: group
                                     flush becomes FLUSHDB of this site alone
                                     instead of a Lua SCAN that blocks Redis
and removes WP_REDIS_SELECTIVE_FLUSH (unsupported upstream) once the site has
its own database. Existing values are never overwritten otherwise.

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

# Pick a Redis database index no other site on this box uses. Each WordPress
# site gets its own database so a flush on one site (the drop-in's flush() is
# FLUSHDB) never empties another site's cache, and key prefixes cannot collide
# however the sites are cloned. Database 0 is left to sites installed before
# this rule. Prints the index; prints nothing when redis-cli is missing or every
# database is taken, and the caller then leaves the constant unset.
redis_next_free_db() {
  local max used i
  command -v redis-cli >/dev/null 2>&1 || return 1
  max=$(redis-cli config get databases 2>/dev/null | tail -1)
  [ "$max" -gt 1 ] 2>/dev/null || max=16
  used=$(grep -hoE "WP_REDIS_DATABASE'[[:space:]]*,[[:space:]]*'?[0-9]+" \
           /home/*/web/*/public_html/wp-config.php \
           /home/*/web/*/public_html.setup/wp-config.php 2>/dev/null \
         | grep -oE '[0-9]+$' | sort -un)
  # A database holding keys that no wp-config claims belongs to something
  # else (another app, a removed site's leftovers) — do not hand it out.
  keyed=$(redis-cli info keyspace 2>/dev/null | grep -oE '^db[0-9]+' | tr -d 'db' | sort -un)
  for ((i = 1; i < max; i++)); do
    printf '%s\n' "$used" | grep -qx "$i" && continue
    printf '%s\n' "$keyed" | grep -qx "$i" && continue
    echo "$i"; return 0
  done
  return 1
}

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
# What the redis-cache drop-in (2.8.0) does, read from its source and its
# README/FAQ — kuumalahde.fi's nightly 504s (2026-08-30) and a fleet review
# (2026-09-25) are why each line is here:
#
#   flush()        `wp cache flush` = FLUSHDB of the site's database — or, with
#                  WP_REDIS_SELECTIVE_FLUSH, a Lua SCAN of the whole database
#                  deleting the prefix's keys. The README lists SELECTIVE_FLUSH
#                  as unsupported ("terribly slow"); this script no longer sets
#                  it.
#   flush_group()  a Lua SCAN of the whole database per call; Redis is
#                  single-threaded, so each scan blocks every other read
#                  (kuumalahde: 392,916 calls in 46h at ~173ms). With
#                  WP_REDIS_DISABLE_GROUP_FLUSH it calls flush() instead.
#
# So DISABLE_GROUP_FLUSH is only safe when the site has a database of its own:
# then FLUSHDB empties just this site (more misses, never stale data). In a
# shared database it would empty every neighbour on each group flush — so it
# is set only after a database of its own is confirmed below.
#
# WP_REDIS_MAXTTL — without it keys never expire and the keyspace only grows,
# which is what makes every scan slow. The FAQ's own answer to a growing Redis.
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
set_const_if_missing WP_REDIS_MAXTTL 86400

# --- WP_REDIS_DATABASE ---------------------------------------------------------
# One Redis database per site (the FAQ requires it: sharing is how one site
# ends up serving another's cached pages). The prefix keeps keys apart; only a
# database of its own keeps flushes apart. Two sites in database 0 empty each
# other's cache on every `wp cache flush` — seen between kuumalahde.fi and its
# dev clone, 2026-09-19. Only set when absent: moving a live site to another
# database is a cold cache.
OWN_DB=0
if [ "$($WP config has WP_REDIS_DATABASE 2>/dev/null; echo $?)" -ne 0 ]; then
  REDIS_DB=$(redis_next_free_db || true)
  if [ -n "$REDIS_DB" ]; then
    set_const_if_missing WP_REDIS_DATABASE "$REDIS_DB"
    OWN_DB=1
  else
    echo "⚠️  No free Redis database found — this site shares database 0; flushes are not isolated."
  fi
else
  EXISTING_DB=$($WP config get WP_REDIS_DATABASE 2>/dev/null)
  echo "ℹ️  WP_REDIS_DATABASE already set: $EXISTING_DB"
  shared=$(grep -lE "WP_REDIS_DATABASE'[[:space:]]*,[[:space:]]*'?${EXISTING_DB}[^0-9]" \
             /home/*/web/*/public_html/wp-config.php 2>/dev/null | grep -v "/$DOMAIN/" | head -1)
  [ -z "$shared" ] && [ "$EXISTING_DB" != "0" ] && OWN_DB=1
fi

if [ "$OWN_DB" -eq 1 ]; then
  set_const_if_missing WP_REDIS_DISABLE_GROUP_FLUSH true
else
  echo "ℹ️  Not setting WP_REDIS_DISABLE_GROUP_FLUSH: on a shared database it would empty every neighbour's cache."
fi
if $WP config has WP_REDIS_SELECTIVE_FLUSH &>/dev/null && [ "$OWN_DB" -eq 1 ]; then
  echo "Removing WP_REDIS_SELECTIVE_FLUSH (unsupported upstream; pointless with a database of its own) ..."
  $WP config delete WP_REDIS_SELECTIVE_FLUSH 2>&1 && echo "✅ removed"
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
