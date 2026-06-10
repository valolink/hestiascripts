#!/bin/bash

if [ "$EUID" -ne 0 ]; then
  echo "❌ ERROR: Please run as root."
  exit 1
fi

export PATH=$PATH:/usr/local/hestia/bin

check_status() {
  if [ $? -ne 0 ]; then
    echo "❌ CRITICAL ERROR: $1"
    echo "⚠️ Script aborted. You may need to manually clean up partial files or databases."
    exit 1
  fi
}

show_help() {
  cat <<'EOF'
USAGE: v-create-wp-staging [OPTIONS]

Create a staging copy of a WordPress site, or tear one down with --teardown.

Live uploads are bind-mounted read-only into the staging site — images display
normally but any write attempt fails at the OS level. The staging domain is
saved as WP_STAGING_URL in the live wp-config and reused automatically on
subsequent runs, so you can refresh staging without specifying the domain again.

OPTIONS:
  --src-user=USER      HestiaCP user who owns the live site
  --src-domain=DOMAIN  Live domain to stage
  --dest-user=USER     HestiaCP user for the staging site (default: same as src-user)
  --new-domain=DOMAIN  Staging domain (e.g. customer.demolink.fi); saved to live
                       wp-config as WP_STAGING_URL for reuse on next run
  --force              Skip overwrite confirmation
  --teardown           Remove the staging site: unmounts uploads, deletes the
                       domain and database; pass --src-user and --src-domain too
                       to also remove WP_STAGING_URL from the live wp-config
  -h, --help           Show this help

EXAMPLES:
  # Create staging — domain is remembered for next time
  v-create-wp-staging --src-user=admin --src-domain=mysite.fi --new-domain=mysite.demolink.fi

  # Refresh existing staging (domain already stored in live wp-config)
  v-create-wp-staging --src-user=admin --src-domain=mysite.fi --force

  # Tear down and clean up WP_STAGING_URL from the live site
  v-create-wp-staging --teardown --dest-user=admin --new-domain=mysite.demolink.fi \
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

  if [ ! -d "/home/$DEST_USER/web/$NEW_DOMAIN" ]; then
    echo "❌ ERROR: Domain $NEW_DOMAIN not found for user $DEST_USER."
    exit 1
  fi

  # Confirm unless --force
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

  # Unmount uploads before deleting files
  if mountpoint -q "$STAGING_UPLOADS" 2>/dev/null; then
    echo "Unmounting read-only uploads..."
    umount -l "$STAGING_UPLOADS"
    check_status "Failed to unmount $STAGING_UPLOADS — run manually: umount -l $STAGING_UPLOADS"
  fi

  # Read DB name before domain deletion removes the files
  STAGING_DB=""
  if [ -f "$STAGING_DIR/wp-config.php" ]; then
    STAGING_DB=$(sudo -u "$DEST_USER" wp --path="$STAGING_DIR" config get DB_NAME 2>/dev/null)
  fi

  # Remove domain from HestiaCP (also removes web files)
  echo "Removing HestiaCP domain..."
  v-delete-web-domain "$DEST_USER" "$NEW_DOMAIN"
  check_status "Failed to remove domain $NEW_DOMAIN."

  # Remove database
  if [ -n "$STAGING_DB" ]; then
    echo "Removing database ($STAGING_DB)..."
    v-delete-database "$DEST_USER" "$STAGING_DB"
    if [ $? -ne 0 ]; then
      echo "⚠️  Could not auto-remove database $STAGING_DB — delete it manually in HestiaCP."
    fi
  else
    echo "⚠️  Could not determine staging database name — remove it manually in HestiaCP."
  fi

  # If live site info provided, remove WP_STAGING_URL from its wp-config
  if [ -n "$SRC_USER" ] && [ -n "$OLD_WEB_DOMAIN" ]; then
    LIVE_DIR="/home/$SRC_USER/web/$OLD_WEB_DOMAIN/public_html"
    if [ -f "$LIVE_DIR/wp-config.php" ]; then
      echo "Removing WP_STAGING_URL from live wp-config..."
      sudo -u "$SRC_USER" wp --path="$LIVE_DIR" config delete WP_STAGING_URL 2>/dev/null \
        && echo "✅ WP_STAGING_URL removed." \
        || echo "⚠️  WP_STAGING_URL not found in live wp-config (already clean)."
    fi
  fi

  echo ""
  echo "✅ Staging site $NEW_DOMAIN removed."
  exit 0
fi

# ============================================================
# CREATE MODE
# ============================================================
echo ""
echo "======================================================"
echo "  WordPress Staging Setup"
echo "======================================================"

# Load user list only when needed
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

# Validate WP early — needed before reading WP_STAGING_URL
if [ ! -f "$LIVE_DIR/wp-config.php" ]; then
  echo "❌ ERROR: No wp-config.php found at $LIVE_DIR. This script is for WordPress sites only."
  exit 1
fi

if ! command -v wp &>/dev/null; then
  echo "❌ ERROR: WP-CLI is not installed or not in your PATH."
  exit 1
fi

# 3. Destination User (staging is usually same user, no create-new option)
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

# 4. Staging Domain
# Read stored WP_STAGING_URL from live wp-config, strip protocol to get domain
STORED_STAGING_URL=$(sudo -u "$SRC_USER" wp --path="$LIVE_DIR" config get WP_STAGING_URL 2>/dev/null)
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
      echo "❌ ERROR: No staging domain provided and none stored in live wp-config."
      echo "   Use --new-domain=<domain> or run interactively first to save WP_STAGING_URL."
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
NEW_DIR="/home/$DEST_USER/web/$NEW_WEB_DOMAIN/public_html"
NEW_UPLOADS="$NEW_DIR/wp-content/uploads"
RAND_STR=$(openssl rand -hex 3)
DB_DUMP="/tmp/${OLD_WEB_DOMAIN}_staging_${RAND_STR}.sql"

# Store WP_STAGING_URL in live wp-config if not set or if it changed
STAGING_URL_TO_STORE="https://$NEW_WEB_DOMAIN"
if [ "$STORED_STAGING_URL" != "$STAGING_URL_TO_STORE" ]; then
  sudo -u "$SRC_USER" wp --path="$LIVE_DIR" config set WP_STAGING_URL "$STAGING_URL_TO_STORE" --type=constant
  check_status "Failed to write WP_STAGING_URL to live wp-config."
  echo "Stored WP_STAGING_URL in live wp-config."
fi

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

# Disk space check — uploads not copied so subtract them from estimate
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

echo "Pre-flight checks passed."

# --- Overwrite Check ---
OVERWRITE_MODE=false
REUSE_DB=false
NEW_DB_NAME="${DEST_USER}_stg_${RAND_STR}"
NEW_DB_USER="${DEST_USER}_stg_${RAND_STR}"
NEW_DB_PASS=$(openssl rand -base64 18 | tr -dc 'a-zA-Z0-9' | head -c 16)
NEW_DB_HOST="localhost"

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

  # Must unmount before we can delete or overwrite files in the uploads dir
  if mountpoint -q "$NEW_UPLOADS" 2>/dev/null; then
    echo "      Unmounting existing read-only uploads..."
    umount -l "$NEW_UPLOADS"
    check_status "Failed to unmount $NEW_UPLOADS."
  fi

  if [ -f "$NEW_DIR/wp-config.php" ]; then
    echo "🔍 Found existing wp-config.php. Checking database connection..."
    if sudo -u "$DEST_USER" wp --path="$NEW_DIR" db check --quiet &>/dev/null; then
      REUSE_DB=true
      NEW_DB_NAME=$(sudo -u "$DEST_USER" wp --path="$NEW_DIR" config get DB_NAME)
      NEW_DB_USER=$(sudo -u "$DEST_USER" wp --path="$NEW_DIR" config get DB_USER)
      NEW_DB_PASS=$(sudo -u "$DEST_USER" wp --path="$NEW_DIR" config get DB_PASSWORD)
      NEW_DB_HOST=$(sudo -u "$DEST_USER" wp --path="$NEW_DIR" config get DB_HOST)
      echo "♻️ Reusing existing staging database: $NEW_DB_NAME"
      echo "[1/9] Emptying existing staging database..."
      sudo -u "$DEST_USER" wp --path="$NEW_DIR" db reset --yes --quiet
      check_status "Failed to reset staging database."
    else
      echo "⚠️ Existing database check failed. A new database will be created."
    fi
  fi
fi

echo "---------------------------------------------------"
echo "Starting staging: $OLD_DOMAIN -> $NEW_WEB_DOMAIN"
echo "Source User: $SRC_USER | Staging User: $DEST_USER"
echo "---------------------------------------------------"

# [1/9] Create domain (no SSL — staging domains manage their own certs)
if [ "$OVERWRITE_MODE" = false ]; then
  echo "[1/9] Creating staging domain ($NEW_WEB_DOMAIN) in HestiaCP..."
  v-add-web-domain "$DEST_USER" "$NEW_WEB_DOMAIN"
  check_status "Failed to create staging domain."
  echo "      SSL skipped — configure separately for your staging domain."
else
  echo "[1/9] Overwriting existing staging site."
fi

# [2/9] Export database
echo "[2/9] Exporting live database..."
sudo -u "$SRC_USER" wp --path="$LIVE_DIR" db export "$DB_DUMP" --quiet
check_status "Failed to export live database."

# [3/9] Copy files — uploads excluded, will be bind-mounted read-only instead
echo "[3/9] Copying files (excluding uploads, cache, backups)..."
if [ "$OVERWRITE_MODE" = true ]; then
  echo "      Clearing staging files..."
  find "$NEW_DIR" -mindepth 1 -delete
else
  rm -f "$NEW_DIR/index.html" "$NEW_DIR/robots.txt"
fi

rsync -a \
  --exclude 'wp-content/uploads' \
  --exclude 'wp-content/cache/*' \
  --exclude 'wp-content/updraft/*' \
  --exclude 'wp-content/debug.log' \
  "$LIVE_DIR/" "$NEW_DIR/"
check_status "Failed to sync files."

chown -R "$DEST_USER:$DEST_USER" "$NEW_DIR"
check_status "Failed to update file ownership."

# [4/9] Bind-mount live uploads as read-only
# Staging can read all live images; writes fail at OS level regardless of WP or plugin behavior
echo "[4/9] Mounting live uploads read-only..."
mkdir -p "$NEW_UPLOADS"
chown "$DEST_USER:$DEST_USER" "$NEW_UPLOADS"
mount --bind "$LIVE_UPLOADS" "$NEW_UPLOADS"
check_status "Failed to bind-mount uploads."
mount -o remount,ro,bind "$NEW_UPLOADS"
check_status "Failed to remount uploads as read-only."
echo "      ✅ $LIVE_UPLOADS mounted read-only at $NEW_UPLOADS"
echo "      ⚠️  This mount will not survive a server reboot — re-run to refresh staging if needed."

# [5/9] Create or reuse database
if [ "$REUSE_DB" = false ]; then
  echo "[5/9] Creating staging database..."
  NEW_DB_BASENAME="stg_${RAND_STR}"
  v-add-database "$DEST_USER" "$NEW_DB_BASENAME" "$NEW_DB_BASENAME" "$NEW_DB_PASS"
  check_status "Failed to create staging database."
else
  echo "[5/9] Reusing existing staging database ($NEW_DB_NAME)."
fi

# [6/9] Update wp-config.php
echo "[6/9] Updating wp-config.php..."
WP_STG="sudo -u $DEST_USER wp --path=$NEW_DIR"

$WP_STG config set DB_NAME "$NEW_DB_NAME"
$WP_STG config set DB_USER "$NEW_DB_USER"
$WP_STG config set DB_PASSWORD "$NEW_DB_PASS"
[ "$REUSE_DB" = true ] && $WP_STG config set DB_HOST "$NEW_DB_HOST"

echo "      Regenerating security salts..."
$WP_STG config shuffle-salts --quiet

echo "      Setting unique Redis cache prefix..."
REDIS_SAFE_PREFIX="${NEW_WEB_DOMAIN//./_}_"
$WP_STG config set WP_CACHE_KEY_SALT "$REDIS_SAFE_PREFIX" --type=constant --quiet
$WP_STG config set WP_REDIS_PREFIX "$REDIS_SAFE_PREFIX" --type=constant --quiet

echo "      Setting WP_ENVIRONMENT_TYPE=staging..."
$WP_STG config set WP_ENVIRONMENT_TYPE "staging" --type=constant --quiet

# [7/9] Import database
echo "[7/9] Importing database..."
$WP_STG db import "$DB_DUMP" --quiet
check_status "Failed to import database."

# [8/9] Search and replace URLs and server paths
echo "[8/9] Running search and replace..."
OLD_WP_URL=$(sudo -u "$SRC_USER" wp --path="$LIVE_DIR" option get home --quiet)
NEW_WP_URL="https://$NEW_WEB_DOMAIN"
echo "      URLs:  $OLD_WP_URL -> $NEW_WP_URL"
$WP_STG search-replace "$OLD_WP_URL" "$NEW_WP_URL" --all-tables --quiet
check_status "Failed URL search-replace."
echo "      Paths: $LIVE_DIR -> $NEW_DIR"
$WP_STG search-replace "$LIVE_DIR" "$NEW_DIR" --all-tables --quiet

# [9/9] Flush cache
echo "[9/9] Flushing object cache..."
$WP_STG cache flush --quiet

rm -f "$DB_DUMP"

echo ""
echo "=================================================================="
echo "✅ Staging Setup Complete!"
echo "   Live site:    $OLD_DOMAIN (User: $SRC_USER)"
echo "   Staging site: $NEW_WEB_DOMAIN (User: $DEST_USER)"
echo "   Database:     $NEW_DB_NAME"
echo "   Uploads:      Read-only from live (bind mount)"
echo ""
echo "   To tear down:"
echo "   v-create-wp-staging --teardown --dest-user=$DEST_USER --new-domain=$NEW_WEB_DOMAIN"
echo "=================================================================="
