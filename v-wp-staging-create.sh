#!/bin/bash

if [ "$EUID" -ne 0 ]; then
  echo "❌ ERROR: Please run as root."
  exit 1
fi

export PATH=$PATH:/usr/local/hestia/bin

# ============================================================
# Helpers
# ============================================================

check_status() {
  if [ $? -ne 0 ]; then
    echo "❌ CRITICAL ERROR: $1"
    echo "⚠️ Script aborted. You may need to manually clean up partial files or databases."
    exit 1
  fi
}

# Where we remember the staging URL for a given live site.
# Stored OUTSIDE wp-config so no WordPress plugin can read it as a magic constant.
staging_url_store() {
  echo "/root/.hestia-staging-urls/${1}_${2}"
}

# Reload every installed PHP-FPM service so workers drop their cached wp-config.php
# and pick up the new WP_REDIS_PREFIX / DB creds. Reload is graceful.
reload_php_fpm() {
  local reloaded=0
  shopt -s nullglob
  for ver_dir in /etc/php/*; do
    [ -d "$ver_dir/fpm" ] || continue
    local ver
    ver=$(basename "$ver_dir")
    if systemctl reload "php${ver}-fpm" 2>/dev/null; then
      reloaded=$((reloaded + 1))
    fi
  done
  shopt -u nullglob
  if [ "$reloaded" -gt 0 ]; then
    echo "      ↻ Reloaded $reloaded PHP-FPM service(s)"
  else
    echo "      ⚠️  Could not reload any PHP-FPM service — reload manually."
  fi
}

# Flush every cache layer we can reach for a WP site.
# Errors are swallowed per-step; missing plugins must not abort the script.
flush_site_caches() {
  local user="$1"
  local path="$2"
  local label="$3"
  echo "      Flushing caches: $label"

  sudo -u "$user" wp --path="$path" cache flush --quiet 2>/dev/null \
    && echo "        ✓ object cache" \
    || echo "        ⚠ object cache flush failed (ok if no drop-in)"

  sudo -u "$user" wp --path="$path" transient delete --all --quiet 2>/dev/null \
    && echo "        ✓ transients" \
    || echo "        ⚠ transient delete failed"

  if sudo -u "$user" wp --path="$path" plugin is-active wp-rocket --quiet 2>/dev/null; then
    sudo -u "$user" wp --path="$path" rocket clean --confirm --quiet 2>/dev/null \
      && echo "        ✓ WP Rocket" \
      || echo "        ⚠ WP Rocket clean failed"
  fi

  if sudo -u "$user" wp --path="$path" plugin is-active woo-product-feed-pro --quiet 2>/dev/null \
     || sudo -u "$user" wp --path="$path" plugin is-active woo-feed --quiet 2>/dev/null; then
    echo "        ℹ product-feed plugin active — feeds will rebuild on next scheduled run"
  fi
}

# Sanity-check a wp-config value. Aborts the script if it doesn't match.
require_wpconfig() {
  local user="$1" path="$2" key="$3" expected="$4"
  local actual
  actual=$(sudo -u "$user" wp --path="$path" config get "$key" --quiet 2>/dev/null)
  if [ "$actual" != "$expected" ]; then
    echo "❌ ERROR: wp-config sanity check failed."
    echo "   Key:      $key"
    echo "   Expected: $expected"
    echo "   Actual:   $actual"
    exit 1
  fi
}

# Confirm a wp option in the DB matches expectation. Aborts on mismatch.
require_option() {
  local user="$1" path="$2" key="$3" expected="$4"
  local actual
  actual=$(sudo -u "$user" wp --path="$path" option get "$key" --quiet 2>/dev/null)
  if [ "$actual" != "$expected" ]; then
    echo "❌ ERROR: DB option sanity check failed."
    echo "   Option:   $key"
    echo "   Expected: $expected"
    echo "   Actual:   $actual"
    exit 1
  fi
}

# Run search-replace for every common URL variant (http/https × www / no-www).
# Plugins love to store multiple shapes of the URL; replacing only the canonical
# form silently leaves stragglers behind.
search_replace_url_variants() {
  local user="$1" path="$2" old_bare="$3" new_url="$4"
  local v
  for v in \
    "https://www.${old_bare}" \
    "http://www.${old_bare}" \
    "https://${old_bare}" \
    "http://${old_bare}"; do
    sudo -u "$user" wp --path="$path" \
      search-replace "$v" "$new_url" --all-tables --quiet --skip-columns=guid 2>/dev/null
  done
}

# Two-step bind mount to RO, with a write probe to PROVE the mount is RO.
# Bind + remount,ro is the historically-portable way; a single mount -o ro,bind
# is silently ignored on older util-linux. The probe runs as root so the test
# is meaningful even when POSIX perms would already block writes for DEST_USER.
bind_mount_uploads_ro() {
  local src="$1" dst="$2"
  mount --bind "$src" "$dst"
  check_status "Failed to bind-mount $src → $dst"
  mount -o remount,ro,bind "$dst"
  check_status "Failed to remount $dst as read-only"
  local probe="$dst/.hestia-ro-probe-$$"
  if touch "$probe" 2>/dev/null; then
    rm -f "$probe"
    echo "❌ ERROR: Uploads mount at $dst is writable. Refusing to publish staging."
    umount -l "$dst" 2>/dev/null
    exit 1
  fi
}

show_help() {
  cat <<'EOF'
USAGE: v-wp-staging-create [OPTIONS]

Create a staging copy of a WordPress site, or tear one down with --teardown.

Hardening (vs naive clone):
  * Staging is built in public_html.setup; only swapped into public_html
    after wp-config, DB import, search-replace, and verification all pass.
    The staging URL therefore returns 404 until everything is correct.
  * Live uploads are bind-mounted READ-ONLY into staging; a write probe
    confirms RO before publish, so staging physically cannot rewrite live
    files (feeds, sitemaps, etc.).
  * PHP-FPM is reloaded on both staging and live after wp-config edits so
    no opcache'd worker keeps running under the old Redis prefix / DB.
  * Object cache, transients and WP Rocket are flushed on both sides at
    the end to evict anything stale from a previous run.
  * Staging URL memory lives in /root/.hestia-staging-urls/, NOT in
    wp-config — no magic constants visible to plugins on live.

OPTIONS:
  --src-user=USER      HestiaCP user who owns the live site
  --src-domain=DOMAIN  Live domain to stage
  --dest-user=USER     HestiaCP user for the staging site (default: same as src-user)
  --new-domain=DOMAIN  Staging domain (e.g. customer.demolink.fi); remembered
                       for re-runs via /root/.hestia-staging-urls/
  --force              Skip overwrite confirmation
  --teardown           Remove the staging site: unmounts uploads, deletes
                       the domain and database; pass --src-user/--src-domain
                       too to also forget the saved staging URL
  -h, --help           Show this help

EXAMPLES:
  # Create staging — domain is remembered for next time
  v-wp-staging-create --src-user=admin --src-domain=mysite.fi --new-domain=mysite.demolink.fi

  # Refresh existing staging (domain already remembered)
  v-wp-staging-create --src-user=admin --src-domain=mysite.fi --force

  # Tear down and forget the saved URL
  v-wp-staging-create --teardown --dest-user=admin --new-domain=mysite.demolink.fi \
    --src-user=admin --src-domain=mysite.fi
EOF
}

# --- Flag Parsing ---
SRC_USER=""
OLD_WEB_DOMAIN=""
DEST_USER=""
NEW_DOMAIN=""
FORCE=false
TEARDOWN=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --src-user=*)   SRC_USER="${1#*=}" ;;
    --src-domain=*) OLD_WEB_DOMAIN="${1#*=}" ;;
    --dest-user=*)  DEST_USER="${1#*=}" ;;
    --new-domain=*) NEW_DOMAIN="${1#*=}" ;;
    --force)        FORCE=true ;;
    --teardown)     TEARDOWN=true ;;
    -h|--help)      show_help; exit 0 ;;
    *) echo "❌ ERROR: Unknown option: $1"; exit 1 ;;
  esac
  shift
done

# ============================================================
# TEARDOWN MODE
# ============================================================
if [ "$TEARDOWN" = true ]; then
  echo ""
  echo "======================================================"
  echo "  WordPress Staging Teardown"
  echo "======================================================"

  if [ -z "$DEST_USER" ]; then
    mapfile -t USERS < <(v-list-users plain | cut -f1)
    PS3="Select staging site user: "
    select DEST_USER in "${USERS[@]}"; do
      [ -n "$DEST_USER" ] && break
      echo "Invalid selection."
    done
  fi

  if [ -z "$NEW_DOMAIN" ]; then
    mapfile -t DOMAINS < <(v-list-web-domains "$DEST_USER" plain | cut -f1)
    if [ ${#DOMAINS[@]} -eq 0 ]; then
      echo "❌ ERROR: No domains found for user $DEST_USER."
      exit 1
    fi
    PS3="Select staging domain to remove: "
    select NEW_DOMAIN in "${DOMAINS[@]}"; do
      [ -n "$NEW_DOMAIN" ] && break
      echo "Invalid selection."
    done
  fi

  NEW_DOMAIN="${NEW_DOMAIN#www.}"
  STAGING_DIR="/home/$DEST_USER/web/$NEW_DOMAIN/public_html"
  STAGING_UPLOADS="$STAGING_DIR/wp-content/uploads"
  SETUP_DIR="/home/$DEST_USER/web/$NEW_DOMAIN/public_html.setup"

  if [ ! -d "/home/$DEST_USER/web/$NEW_DOMAIN" ]; then
    echo "❌ ERROR: Domain $NEW_DOMAIN not found for user $DEST_USER."
    exit 1
  fi

  if [ "$FORCE" = false ]; then
    if [ ! -t 0 ]; then
      echo "❌ ERROR: Use --force to confirm teardown in non-interactive mode."
      exit 1
    fi
    echo ""
    echo "⚠️  This will permanently delete the staging site: $NEW_DOMAIN"
    read -p "Type 'yes' to confirm: " CONFIRM
    if [ "$CONFIRM" != "yes" ]; then
      echo "Aborting."
      exit 0
    fi
  fi

  echo ""

  # Unmount in both possible locations (setup dir may exist from a failed run).
  for mp in "$STAGING_UPLOADS" "$SETUP_DIR/wp-content/uploads"; do
    if mountpoint -q "$mp" 2>/dev/null; then
      echo "Unmounting $mp ..."
      umount -l "$mp"
      check_status "Failed to unmount $mp — run manually: umount -l $mp"
    fi
  done

  # Capture DB name from whichever directory has a wp-config.
  STAGING_DB=""
  for cfg in "$STAGING_DIR/wp-config.php" "$SETUP_DIR/wp-config.php"; do
    if [ -f "$cfg" ]; then
      STAGING_DB=$(sudo -u "$DEST_USER" wp --path="$(dirname "$cfg")" config get DB_NAME 2>/dev/null)
      [ -n "$STAGING_DB" ] && break
    fi
  done

  # Stale setup dir from a failed run — remove before HestiaCP teardown.
  [ -d "$SETUP_DIR" ] && rm -rf "$SETUP_DIR"

  echo "Removing HestiaCP domain..."
  v-delete-web-domain "$DEST_USER" "$NEW_DOMAIN"
  check_status "Failed to remove domain $NEW_DOMAIN."

  if [ -n "$STAGING_DB" ]; then
    echo "Removing database ($STAGING_DB)..."
    v-delete-database "$DEST_USER" "$STAGING_DB"
    if [ $? -ne 0 ]; then
      echo "⚠️  Could not auto-remove database $STAGING_DB — delete it manually in HestiaCP."
    fi
  else
    echo "⚠️  Could not determine staging database name — remove it manually in HestiaCP."
  fi

  # Forget the saved staging URL and drop any legacy WP_STAGING_URL constant.
  if [ -n "$SRC_USER" ] && [ -n "$OLD_WEB_DOMAIN" ]; then
    URL_STORE=$(staging_url_store "$SRC_USER" "$OLD_WEB_DOMAIN")
    if [ -f "$URL_STORE" ]; then
      rm -f "$URL_STORE"
      echo "✅ Forgot staged URL ($URL_STORE)."
    fi
    LIVE_DIR="/home/$SRC_USER/web/$OLD_WEB_DOMAIN/public_html"
    if [ -f "$LIVE_DIR/wp-config.php" ]; then
      if sudo -u "$SRC_USER" wp --path="$LIVE_DIR" config has WP_STAGING_URL --quiet 2>/dev/null; then
        sudo -u "$SRC_USER" wp --path="$LIVE_DIR" config delete WP_STAGING_URL --quiet \
          && echo "✅ Removed legacy WP_STAGING_URL constant from live wp-config."
      fi
      flush_site_caches "$SRC_USER" "$LIVE_DIR" "live ($OLD_WEB_DOMAIN)"
      reload_php_fpm
    fi
  fi

  echo ""
  echo "✅ Staging site $NEW_DOMAIN removed."
  exit 0
fi

# ============================================================
# CREATE / REFRESH MODE
# ============================================================
echo ""
echo "======================================================"
echo "  WordPress Staging Setup (strict mode)"
echo "======================================================"

if [ -z "$SRC_USER" ] || [ -z "$DEST_USER" ]; then
  mapfile -t USERS < <(v-list-users plain | cut -f1)
fi

# 1. Source User
if [ -z "$SRC_USER" ]; then
  PS3="Select the SOURCE user (enter number): "
  select SRC_USER in "${USERS[@]}"; do
    [ -n "$SRC_USER" ] && break
    echo "Invalid selection. Please try again."
  done
fi

# 2. Source Domain
if [ -z "$OLD_WEB_DOMAIN" ]; then
  echo ""
  PS3="Select the SOURCE domain to stage: "
  mapfile -t DOMAINS < <(v-list-web-domains "$SRC_USER" plain | cut -f1)
  if [ ${#DOMAINS[@]} -eq 0 ]; then
    echo "❌ ERROR: No web domains found for user $SRC_USER."
    exit 1
  fi
  select OLD_WEB_DOMAIN in "${DOMAINS[@]}"; do
    [ -n "$OLD_WEB_DOMAIN" ] && break
    echo "Invalid selection."
  done
fi
OLD_DOMAIN="$OLD_WEB_DOMAIN"
LIVE_DIR="/home/$SRC_USER/web/$OLD_WEB_DOMAIN/public_html"
LIVE_UPLOADS="$LIVE_DIR/wp-content/uploads"

if [ ! -f "$LIVE_DIR/wp-config.php" ]; then
  echo "❌ ERROR: No wp-config.php found at $LIVE_DIR. This script is for WordPress sites only."
  exit 1
fi

if ! command -v wp &>/dev/null; then
  echo "❌ ERROR: WP-CLI is not installed or not in your PATH."
  exit 1
fi

# 3. Destination User
if [ -z "$DEST_USER" ]; then
  echo ""
  PS3="Staging site owner (enter number): "
  select DEST_OPT in "Same user ($SRC_USER)" "Another existing user"; do
    case $REPLY in
    1)
      DEST_USER="$SRC_USER"
      break
      ;;
    2)
      echo "Select user:"
      select DEST_USER in "${USERS[@]}"; do
        if [ -n "$DEST_USER" ]; then break 2; else echo "Invalid selection."; fi
      done
      ;;
    *) echo "Invalid option." ;;
    esac
  done
fi

# 4. Recover saved staging URL.
# Priority: sidecar file → legacy WP_STAGING_URL constant in live wp-config.
URL_STORE=$(staging_url_store "$SRC_USER" "$OLD_WEB_DOMAIN")
STORED_STAGING_URL=""
if [ -f "$URL_STORE" ]; then
  STORED_STAGING_URL=$(cat "$URL_STORE" 2>/dev/null | tr -d '[:space:]')
fi
if [ -z "$STORED_STAGING_URL" ]; then
  STORED_STAGING_URL=$(sudo -u "$SRC_USER" wp --path="$LIVE_DIR" config get WP_STAGING_URL --quiet 2>/dev/null)
fi
STORED_STAGING_DOMAIN=""
if [ -n "$STORED_STAGING_URL" ]; then
  STORED_STAGING_DOMAIN="${STORED_STAGING_URL#https://}"
  STORED_STAGING_DOMAIN="${STORED_STAGING_DOMAIN#http://}"
  STORED_STAGING_DOMAIN="${STORED_STAGING_DOMAIN%%/*}"
fi

if [ -z "$NEW_DOMAIN" ]; then
  if [ -n "$STORED_STAGING_DOMAIN" ]; then
    if [ ! -t 0 ]; then
      NEW_DOMAIN="$STORED_STAGING_DOMAIN"
      echo "Using stored staging domain: $NEW_DOMAIN"
    else
      echo ""
      read -p "Staging domain [$STORED_STAGING_DOMAIN]: " NEW_DOMAIN
      NEW_DOMAIN="${NEW_DOMAIN:-$STORED_STAGING_DOMAIN}"
    fi
  else
    if [ ! -t 0 ]; then
      echo "❌ ERROR: No staging domain provided and none stored."
      echo "   Use --new-domain=<domain> or run interactively once to save it."
      exit 1
    fi
    echo ""
    read -p "Enter the staging domain (e.g., customer.demolink.fi): " NEW_DOMAIN
    if [ -z "$NEW_DOMAIN" ]; then
      echo "❌ ERROR: Staging domain cannot be empty."
      exit 1
    fi
  fi
fi

NEW_WEB_DOMAIN="${NEW_DOMAIN#www.}"
DOMAIN_ROOT="/home/$DEST_USER/web/$NEW_WEB_DOMAIN"
NEW_DIR="$DOMAIN_ROOT/public_html"
NEW_UPLOADS="$NEW_DIR/wp-content/uploads"
SETUP_DIR="$DOMAIN_ROOT/public_html.setup"
SETUP_UPLOADS="$SETUP_DIR/wp-content/uploads"
RAND_STR=$(openssl rand -hex 3)
DB_DUMP="/tmp/${OLD_WEB_DOMAIN}_staging_${RAND_STR}.sql"
NEW_WP_URL="https://$NEW_WEB_DOMAIN"

# ============================================================
# Cleanup trap. On failure, leave a clean diagnosable state and
# tell the user how to recover. Never aggressive-rollback.
# ============================================================
PUBLISHED=false
cleanup() {
  local rc=$?
  [ -n "$DB_DUMP" ] && rm -f "$DB_DUMP" 2>/dev/null

  if [ $rc -eq 0 ]; then
    return
  fi

  echo ""
  echo "─── failure cleanup (exit $rc) ─────────────────────────"

  if [ "$PUBLISHED" = false ] && [ -d "$SETUP_DIR" ]; then
    if mountpoint -q "$SETUP_UPLOADS" 2>/dev/null; then
      echo "  unmounting setup uploads"
      umount -l "$SETUP_UPLOADS" 2>/dev/null
    fi
    echo "  removing $SETUP_DIR"
    rm -rf "$SETUP_DIR" 2>/dev/null
  fi

  echo ""
  echo "Recovery options:"
  echo "  • Teardown completely:"
  echo "      v-wp-staging-create --teardown --dest-user=$DEST_USER \\"
  echo "          --new-domain=$NEW_WEB_DOMAIN --src-user=$SRC_USER \\"
  echo "          --src-domain=$OLD_WEB_DOMAIN --force"
  echo "  • Retry from clean state:"
  echo "      Re-run the same command with --force"
  echo ""
}
trap cleanup EXIT

# --- Pre-Flight Checks ---
echo "---------------------------------------------------"
echo "Running pre-flight checks..."

if [ ! -d "$LIVE_DIR" ]; then
  echo "❌ ERROR: Source directory $LIVE_DIR does not exist."
  exit 1
fi

ensure_bash() {
  local USER_TO_CHECK=$1
  local USER_CONF="/usr/local/hestia/data/users/$USER_TO_CHECK/user.conf"
  if [ -f "$USER_CONF" ]; then
    CURRENT_SHELL=$(grep "^SHELL=" "$USER_CONF" | cut -d"'" -f2)
    if [[ "$CURRENT_SHELL" != "bash" && "$CURRENT_SHELL" != "sh" ]]; then
      echo "⚠️ User $USER_TO_CHECK lacks shell access. Granting bash..."
      v-change-user-shell "$USER_TO_CHECK" "bash"
      check_status "Failed to change user shell to bash for $USER_TO_CHECK."
    fi
  fi
}

check_php_cli() {
  local USER_TO_CHECK=$1
  sudo -u "$USER_TO_CHECK" php -r "
    \$d = array_map('trim', explode(',', ini_get('disable_functions')));
    exit((in_array('exec', \$d) || in_array('proc_open', \$d)) ? 1 : 0);
  "
  if [ $? -eq 1 ]; then
    echo "❌ ERROR: 'proc_open' and/or 'exec' are disabled in PHP CLI for user $USER_TO_CHECK."
    exit 1
  fi
}

ensure_bash "$SRC_USER"
ensure_bash "$DEST_USER"
check_php_cli "$SRC_USER"
[ "$SRC_USER" != "$DEST_USER" ] && check_php_cli "$DEST_USER"

echo "      Checking disk space..."
UPLOADS_MB=$(du -sm "$LIVE_UPLOADS" 2>/dev/null | cut -f1 || echo 0)
TOTAL_MB=$(du -sm "$LIVE_DIR" | cut -f1)
OLD_SIZE_MB=$((TOTAL_MB - UPLOADS_MB))
REQUIRED_MB=$((OLD_SIZE_MB + (OLD_SIZE_MB / 5) + 50))
AVAILABLE_MB=$(df -m "/home/$DEST_USER" | awk 'NR==2 {print $4}')

if [ "$AVAILABLE_MB" -lt "$REQUIRED_MB" ]; then
  echo "❌ ERROR: Insufficient disk space."
  echo "   Estimated Requirement: ~${REQUIRED_MB}MB (uploads excluded from copy)"
  echo "   Actually Available:    ${AVAILABLE_MB}MB"
  exit 1
fi

# Stale setup directory from a previous failed run.
if [ -d "$SETUP_DIR" ]; then
  echo "⚠️  Stale $SETUP_DIR exists (previous run failed)."
  if [ "$FORCE" = true ] || [ ! -t 0 ]; then
    echo "      Removing it."
  else
    read -p "Remove it and continue? Type 'yes': " CONFIRM
    [ "$CONFIRM" = "yes" ] || { echo "Aborting."; exit 0; }
  fi
  if mountpoint -q "$SETUP_UPLOADS" 2>/dev/null; then
    umount -l "$SETUP_UPLOADS"
  fi
  rm -rf "$SETUP_DIR"
fi

echo "Pre-flight checks passed."

# --- Overwrite check ---
OVERWRITE_MODE=false
OLD_STAGING_DB=""
NEW_DB_BASENAME="stg_${RAND_STR}"
NEW_DB_NAME="${DEST_USER}_${NEW_DB_BASENAME}"
NEW_DB_USER="$NEW_DB_NAME"
NEW_DB_PASS=$(openssl rand -base64 18 | tr -dc 'a-zA-Z0-9' | head -c 16)
NEW_DB_HOST="localhost"
# Random component in the prefix so a future teardown+rebuild with the same
# domain cannot land back on the same Redis keys.
REDIS_SAFE_PREFIX="${NEW_WEB_DOMAIN//./_}_${RAND_STR}_"

if v-list-web-domain "$DEST_USER" "$NEW_WEB_DOMAIN" &>/dev/null; then
  echo "⚠️ Staging domain ($NEW_WEB_DOMAIN) already exists for user $DEST_USER."
  if [ "$FORCE" = true ]; then
    echo "      --force set, proceeding with overwrite."
    OVERWRITE_MODE=true
  elif [ ! -t 0 ]; then
    echo "❌ ERROR: Domain already exists. Re-run with --force to overwrite."
    exit 1
  else
    read -p "Overwrite existing staging site? Type 'yes' to continue: " CONFIRM
    if [ "$CONFIRM" != "yes" ]; then
      echo "Aborting."
      exit 0
    fi
    OVERWRITE_MODE=true
  fi

  # Capture old DB name so we can delete it cleanly after publish.
  if [ -f "$NEW_DIR/wp-config.php" ]; then
    OLD_STAGING_DB=$(sudo -u "$DEST_USER" wp --path="$NEW_DIR" config get DB_NAME 2>/dev/null)
  fi

  # Unmount existing uploads before we touch public_html.
  if mountpoint -q "$NEW_UPLOADS" 2>/dev/null; then
    echo "      Unmounting existing uploads..."
    umount -l "$NEW_UPLOADS"
    check_status "Failed to unmount $NEW_UPLOADS."
  fi
fi

echo "---------------------------------------------------"
echo "Staging: $OLD_DOMAIN -> $NEW_WEB_DOMAIN"
echo "Source User: $SRC_USER | Staging User: $DEST_USER"
echo "Strategy: build in $SETUP_DIR, atomic-publish to $NEW_DIR"
echo "---------------------------------------------------"

# [1/11] Create domain in HestiaCP (no SSL) and immediately disarm public_html.
# We DO NOT let HestiaCP's default page or the rsynced live wp-config sit at
# the public path. Anything served from public_html before the swap would
# inherit live's WP_REDIS_PREFIX + DB creds — the exact bug we're killing.
if [ "$OVERWRITE_MODE" = false ]; then
  echo "[1/11] Creating staging domain ($NEW_WEB_DOMAIN) in HestiaCP..."
  v-add-web-domain "$DEST_USER" "$NEW_WEB_DOMAIN"
  check_status "Failed to create staging domain."
  echo "       SSL skipped — configure separately for your staging domain."
else
  echo "[1/11] Reusing existing staging domain."
fi

echo "       Disarming public_html (any request will 404 until publish)..."
if [ -d "$NEW_DIR" ]; then
  find "$NEW_DIR" -mindepth 1 -delete 2>/dev/null
fi

# [2/11] Export live DB
echo "[2/11] Exporting live database..."
sudo -u "$SRC_USER" wp --path="$LIVE_DIR" db export "$DB_DUMP" --quiet
check_status "Failed to export live database."

# [3/11] Build setup dir
echo "[3/11] Building $SETUP_DIR (uploads/cache/backups excluded)..."
mkdir -p "$SETUP_DIR"
chown "$DEST_USER:$DEST_USER" "$SETUP_DIR"
chmod 750 "$SETUP_DIR"

rsync -a \
  --exclude 'wp-content/uploads' \
  --exclude 'wp-content/cache/*' \
  --exclude 'wp-content/updraft/*' \
  --exclude 'wp-content/wp-rocket-config/*' \
  --exclude 'wp-content/debug.log' \
  --exclude 'wp-config-backup.php' \
  "$LIVE_DIR/" "$SETUP_DIR/"
check_status "Failed to sync files."

chown -R "$DEST_USER:$DEST_USER" "$SETUP_DIR"
check_status "Failed to update file ownership on $SETUP_DIR."

# [4/11] Rewrite wp-config in the setup dir BEFORE creating the DB.
# Using $WP_STG = setup dir. Until publish, this WP can only be reached via CLI.
echo "[4/11] Rewriting wp-config in $SETUP_DIR..."
WP_STG="sudo -u $DEST_USER wp --path=$SETUP_DIR"

# Remove any constants we don't want inherited from live before we set the staging
# values. WP_HOME/WP_SITEURL must reflect staging, not live, or constant beats DB.
for stale in WP_HOME WP_SITEURL WP_STAGING_URL HESTIA_STAGING_URL; do
  $WP_STG config delete "$stale" --quiet 2>/dev/null || true
done

$WP_STG config set DB_NAME "$NEW_DB_NAME" --quiet
$WP_STG config set DB_USER "$NEW_DB_USER" --quiet
$WP_STG config set DB_PASSWORD "$NEW_DB_PASS" --quiet
$WP_STG config set DB_HOST "$NEW_DB_HOST" --quiet

echo "       Shuffling salts..."
$WP_STG config shuffle-salts --quiet

echo "       Setting unique Redis namespace ($REDIS_SAFE_PREFIX)..."
$WP_STG config set WP_CACHE_KEY_SALT "$REDIS_SAFE_PREFIX" --type=constant --quiet
$WP_STG config set WP_REDIS_PREFIX   "$REDIS_SAFE_PREFIX" --type=constant --quiet

echo "       Pinning WP_HOME/WP_SITEURL to staging..."
$WP_STG config set WP_HOME    "$NEW_WP_URL" --type=constant --quiet
$WP_STG config set WP_SITEURL "$NEW_WP_URL" --type=constant --quiet

echo "       Hardening: WP_ENVIRONMENT_TYPE=staging, DISABLE_WP_CRON, DISALLOW_FILE_MODS..."
$WP_STG config set WP_ENVIRONMENT_TYPE "staging" --type=constant --quiet
$WP_STG config set DISABLE_WP_CRON     "true"    --type=constant --raw --quiet
$WP_STG config set DISALLOW_FILE_MODS  "true"    --type=constant --raw --quiet

# [5/11] (Re)create the staging database.
echo "[5/11] Creating staging database ($NEW_DB_NAME)..."
v-add-database "$DEST_USER" "$NEW_DB_BASENAME" "$NEW_DB_BASENAME" "$NEW_DB_PASS"
check_status "Failed to create staging database."

# [6/11] Import DB into the staging DB. setup dir's wp-config now points at it.
echo "[6/11] Importing database into staging..."
$WP_STG db import "$DB_DUMP" --quiet
check_status "Failed to import database."

# [7/11] Search-replace ALL URL variants and the absolute path.
echo "[7/11] Search-replace (URL variants + absolute path)..."
OLD_WP_URL=$(sudo -u "$SRC_USER" wp --path="$LIVE_DIR" option get home --quiet)
OLD_BARE="${OLD_WP_URL#https://}"
OLD_BARE="${OLD_BARE#http://}"
OLD_BARE="${OLD_BARE#www.}"
OLD_BARE="${OLD_BARE%%/*}"
echo "       Bare host: $OLD_BARE  →  $NEW_WP_URL"
search_replace_url_variants "$DEST_USER" "$SETUP_DIR" "$OLD_BARE" "$NEW_WP_URL"
echo "       Absolute path: $LIVE_DIR  →  $NEW_DIR"
$WP_STG search-replace "$LIVE_DIR" "$NEW_DIR" --all-tables --quiet --skip-columns=guid

# [8/11] Pre-publish verification.
# Refuse to publish if any of the safety constants or URLs are wrong.
echo "[8/11] Verifying staging is safe to publish..."
require_wpconfig "$DEST_USER" "$SETUP_DIR" DB_NAME             "$NEW_DB_NAME"
require_wpconfig "$DEST_USER" "$SETUP_DIR" WP_REDIS_PREFIX     "$REDIS_SAFE_PREFIX"
require_wpconfig "$DEST_USER" "$SETUP_DIR" WP_CACHE_KEY_SALT   "$REDIS_SAFE_PREFIX"
require_wpconfig "$DEST_USER" "$SETUP_DIR" WP_HOME             "$NEW_WP_URL"
require_wpconfig "$DEST_USER" "$SETUP_DIR" WP_SITEURL          "$NEW_WP_URL"
require_wpconfig "$DEST_USER" "$SETUP_DIR" WP_ENVIRONMENT_TYPE "staging"
require_option   "$DEST_USER" "$SETUP_DIR" home    "$NEW_WP_URL"
require_option   "$DEST_USER" "$SETUP_DIR" siteurl "$NEW_WP_URL"
echo "       ✓ All checks passed."

# [9/11] Atomic publish: setup dir → public_html.
# Window between rm and mv is microseconds; nginx returns 404 during it.
# Uploads are NOT mounted yet — staging is briefly imageless after publish,
# until step [10] mounts them. That window is fully under our control because
# we control PHP-FPM reload + cache flush below.
echo "[9/11] Publishing: swap $SETUP_DIR → $NEW_DIR ..."
rm -rf "$NEW_DIR"
mv "$SETUP_DIR" "$NEW_DIR"
check_status "Failed to publish staging directory."
PUBLISHED=true

# [10/11] Bind-mount live uploads RO into the now-published path,
# and PROVE staging cannot write into it before declaring success.
echo "[10/11] Bind-mounting live uploads read-only..."
mkdir -p "$NEW_UPLOADS"
chown "$DEST_USER:$DEST_USER" "$NEW_UPLOADS"
bind_mount_uploads_ro "$LIVE_UPLOADS" "$NEW_UPLOADS"
echo "       ✓ $LIVE_UPLOADS mounted RO at $NEW_UPLOADS (write probe confirmed)"
echo "       ⚠ Bind mount does not survive a server reboot — re-run to refresh."

# [11/11] Reload PHP-FPM (so workers drop stale wp-config) and flush every cache.
# This must happen on BOTH staging and live: staging because its workers may
# have cached the empty disarmed wp-config; live because the traffic-window
# and any historical pollution may have left staging URLs in live's Redis.
echo "[11/11] Reloading PHP-FPM and flushing caches..."
reload_php_fpm
flush_site_caches "$DEST_USER" "$NEW_DIR"  "staging ($NEW_WEB_DOMAIN)"
flush_site_caches "$SRC_USER"  "$LIVE_DIR" "live ($OLD_WEB_DOMAIN)"

# Drop old staging DB if we replaced one. Best-effort; non-fatal.
if [ -n "$OLD_STAGING_DB" ] && [ "$OLD_STAGING_DB" != "$NEW_DB_NAME" ]; then
  OLD_SUFFIX="${OLD_STAGING_DB#${DEST_USER}_}"
  echo "       Removing prior staging DB ($OLD_STAGING_DB)..."
  v-delete-database "$DEST_USER" "$OLD_SUFFIX" 2>/dev/null \
    || echo "       ⚠ Could not auto-remove $OLD_STAGING_DB — delete manually."
fi

# Remember the staging URL for next run — in a sidecar, NOT in wp-config.
# Also clean up any legacy WP_STAGING_URL constant on live from older runs.
mkdir -p "$(dirname "$URL_STORE")"
chmod 700 "$(dirname "$URL_STORE")"
echo "$NEW_WP_URL" > "$URL_STORE"
chmod 600 "$URL_STORE"
if sudo -u "$SRC_USER" wp --path="$LIVE_DIR" config has WP_STAGING_URL --quiet 2>/dev/null; then
  sudo -u "$SRC_USER" wp --path="$LIVE_DIR" config delete WP_STAGING_URL --quiet \
    && echo "       ✓ Removed legacy WP_STAGING_URL constant from live wp-config."
  # Live wp-config changed → reload FPM again so workers don't keep the constant.
  reload_php_fpm
fi

rm -f "$DB_DUMP"

echo ""
echo "=================================================================="
echo "✅ Staging Setup Complete!"
echo "   Live site:    $OLD_DOMAIN (user: $SRC_USER)"
echo "   Staging site: $NEW_WEB_DOMAIN (user: $DEST_USER)"
echo "   Database:     $NEW_DB_NAME"
echo "   Redis prefix: $REDIS_SAFE_PREFIX"
echo "   Uploads:      RO bind-mount from live (write-probed)"
echo "   Stored URL:   $URL_STORE"
echo ""
echo "   To tear down:"
echo "   v-wp-staging-create --teardown --dest-user=$DEST_USER \\"
echo "       --new-domain=$NEW_WEB_DOMAIN --src-user=$SRC_USER \\"
echo "       --src-domain=$OLD_WEB_DOMAIN"
echo "=================================================================="
