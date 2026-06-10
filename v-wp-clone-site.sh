#!/bin/bash

if [ "$EUID" -ne 0 ]; then
  echo "❌ ERROR: Please run this script as root."
  exit 1
fi

export PATH=$PATH:/usr/local/hestia/bin

check_status() {
  if [ $? -ne 0 ]; then
    echo "❌ CRITICAL ERROR: $1"
    echo "⚠️ Script aborted to prevent damage. You may need to manually clean up partial files or databases."
    exit 1
  fi
}

show_help() {
  cat <<'EOF'
USAGE: v-wp-clone-site [OPTIONS]

Clone a WordPress site to a new domain within HestiaCP.
Omit any flag to be prompted interactively.

OPTIONS:
  --src-user=USER      HestiaCP user who owns the source site
  --src-domain=DOMAIN  Domain to clone
  --dest-user=USER     HestiaCP user for the new site (must already exist)
  --new-domain=DOMAIN  Domain name for the cloned site
  --force              Skip overwrite confirmation if destination domain exists
  -h, --help           Show this help

EXAMPLES:
  # Run interactively over SSH
  v-wp-clone-site

  # Clone in one command (e.g. via hestia-streamer)
  v-wp-clone-site --src-user=admin --src-domain=mysite.fi --dest-user=admin --new-domain=newsite.fi
EOF
}

# --- Flag Parsing ---
SRC_USER=""
OLD_WEB_DOMAIN=""
DEST_USER=""
NEW_DOMAIN=""
FORCE=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --src-user=*)   SRC_USER="${1#*=}" ;;
    --src-domain=*) OLD_WEB_DOMAIN="${1#*=}" ;;
    --dest-user=*)  DEST_USER="${1#*=}" ;;
    --new-domain=*) NEW_DOMAIN="${1#*=}" ;;
    --force)        FORCE=true ;;
    -h|--help)      show_help; exit 0 ;;
    *) echo "❌ ERROR: Unknown option: $1"; exit 1 ;;
  esac
  shift
done

echo "==================================================="
echo "  HestiaCP WordPress Cloner"
echo "==================================================="

# Load user list only when needed for interactive prompts
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
  PS3="Select the SOURCE domain to clone: "
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

# 3. Destination User
if [ -z "$DEST_USER" ]; then
  echo ""
  PS3="Where should the new site be cloned? (enter number): "
  select DEST_OPT in "Same user ($SRC_USER)" "Another existing user" "Create a new user"; do
    case $REPLY in
    1)
      DEST_USER="$SRC_USER"
      break
      ;;
    2)
      echo "Select TARGET user:"
      select DEST_USER in "${USERS[@]}"; do
        if [ -n "$DEST_USER" ]; then break 2; else echo "Invalid selection."; fi
      done
      ;;
    3)
      read -p "Enter new username: " NEW_USERNAME
      read -p "Enter new email: " NEW_EMAIL
      read -s -p "Enter new password: " NEW_PASS
      echo
      echo "Creating new user '$NEW_USERNAME'..."
      v-add-user "$NEW_USERNAME" "$NEW_PASS" "$NEW_EMAIL"
      check_status "Failed to create new user."
      DEST_USER="$NEW_USERNAME"
      break
      ;;
    *)
      echo "Invalid option."
      ;;
    esac
  done
fi

# 4. New Domain
if [ -z "$NEW_DOMAIN" ]; then
  echo ""
  read -p "Enter the NEW domain (e.g., newsite.com): " NEW_DOMAIN
  if [ -z "$NEW_DOMAIN" ]; then
    echo "❌ ERROR: Domain cannot be empty."
    exit 1
  fi
fi
NEW_WEB_DOMAIN="${NEW_DOMAIN#www.}"

# Define Paths & Variables
OLD_DIR="/home/$SRC_USER/web/$OLD_WEB_DOMAIN/public_html"
NEW_DIR="/home/$DEST_USER/web/$NEW_WEB_DOMAIN/public_html"
RAND_STR=$(openssl rand -hex 3)
DB_DUMP="/tmp/${OLD_WEB_DOMAIN}_clone_${RAND_STR}.sql"

# --- Pre-Flight Checks ---
echo "---------------------------------------------------"
echo "Running pre-flight checks..."

if [ ! -d "$OLD_DIR" ]; then
  echo "❌ ERROR: Source directory $OLD_DIR does not exist."
  exit 1
fi

if [ ! -f "$OLD_DIR/wp-config.php" ]; then
  echo "❌ ERROR: No wp-config.php found in $OLD_DIR. This script is strictly for WordPress sites."
  exit 1
fi

if ! command -v wp &>/dev/null; then
  echo "❌ ERROR: WP-CLI is not installed or not in your PATH."
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
ensure_bash "$SRC_USER"
ensure_bash "$DEST_USER"

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
check_php_cli "$SRC_USER"
if [ "$SRC_USER" != "$DEST_USER" ]; then
  check_php_cli "$DEST_USER"
fi

echo "      Checking disk space..."
OLD_SIZE_MB=$(du -sm "$OLD_DIR" | cut -f1)
REQUIRED_MB=$((OLD_SIZE_MB + (OLD_SIZE_MB / 5) + 50))
AVAILABLE_MB=$(df -m "/home/$DEST_USER" | awk 'NR==2 {print $4}')

if [ "$AVAILABLE_MB" -lt "$REQUIRED_MB" ]; then
  echo "❌ ERROR: Insufficient disk space to safely clone."
  echo "   Estimated Requirement: ~${REQUIRED_MB}MB"
  echo "   Actually Available:    ${AVAILABLE_MB}MB"
  exit 1
fi

echo "Pre-flight checks passed."

# --- Destination & Overwrite Check ---
OVERWRITE_MODE=false
REUSE_DB=false
NEW_DB_NAME="${DEST_USER}_cln_${RAND_STR}"
NEW_DB_USER="${DEST_USER}_cln_${RAND_STR}"
NEW_DB_PASS=$(openssl rand -base64 18 | tr -dc 'a-zA-Z0-9' | head -c 16)
NEW_DB_HOST="localhost"

if v-list-web-domain "$DEST_USER" "$NEW_WEB_DOMAIN" &>/dev/null; then
  echo "⚠️ Target domain ($NEW_WEB_DOMAIN) already exists for user $DEST_USER."
  if [ "$FORCE" = true ]; then
    echo "      --force flag set, proceeding with overwrite."
    OVERWRITE_MODE=true
  elif [ ! -t 0 ]; then
    echo "❌ ERROR: Domain already exists. Re-run with --force to overwrite."
    exit 1
  else
    read -p "Do you want to OVERWRITE the existing site? Type 'yes' to continue: " CONFIRM
    if [ "$CONFIRM" != "yes" ]; then
      echo "Aborting clone process."
      exit 0
    fi
    OVERWRITE_MODE=true
  fi

  if [ -f "$NEW_DIR/wp-config.php" ]; then
    echo "🔍 Found existing wp-config.php. Checking database connection..."
    if sudo -u "$DEST_USER" wp --path="$NEW_DIR" db check --quiet &>/dev/null; then
      REUSE_DB=true
      NEW_DB_NAME=$(sudo -u "$DEST_USER" wp --path="$NEW_DIR" config get DB_NAME)
      NEW_DB_USER=$(sudo -u "$DEST_USER" wp --path="$NEW_DIR" config get DB_USER)
      NEW_DB_PASS=$(sudo -u "$DEST_USER" wp --path="$NEW_DIR" config get DB_PASSWORD)
      NEW_DB_HOST=$(sudo -u "$DEST_USER" wp --path="$NEW_DIR" config get DB_HOST)

      echo "♻️ Preparing to reuse existing database: $NEW_DB_NAME"
      echo "[1/8] Emptying existing target database..."
      sudo -u "$DEST_USER" wp --path="$NEW_DIR" db reset --yes --quiet
      check_status "Failed to reset existing destination database."
    else
      echo "⚠️ Existing database check failed. A new database will be created."
    fi
  fi
fi

echo "---------------------------------------------------"
echo "Starting clone: $OLD_DOMAIN -> $NEW_DOMAIN"
echo "Source User: $SRC_USER | Target User: $DEST_USER"
echo "---------------------------------------------------"

# 1. Create the new web domain (if not overwriting)
if [ "$OVERWRITE_MODE" = false ]; then
  echo "[1/8] Creating new web domain ($NEW_WEB_DOMAIN) in HestiaCP..."
  v-add-web-domain "$DEST_USER" "$NEW_WEB_DOMAIN"
  check_status "Failed to create web domain."

  echo "      Checking DNS records before requesting SSL..."
  OLD_IP=$(getent hosts "$OLD_WEB_DOMAIN" | awk '{ print $1 }' | head -n 1)
  NEW_IP=$(getent hosts "$NEW_WEB_DOMAIN" | awk '{ print $1 }' | head -n 1)

  if [ -n "$NEW_IP" ] && [ "$OLD_IP" == "$NEW_IP" ]; then
    echo "      ✅ DNS matches ($NEW_IP). Provisioning SSL..."
    v-add-web-domain-ssl "$DEST_USER" "$NEW_WEB_DOMAIN"
  else
    echo "      ⚠️ DNS for $NEW_WEB_DOMAIN does not match $OLD_WEB_DOMAIN."
    echo "      Skipping SSL provisioning."
  fi
fi

# 2. Export the database using WP-CLI (Run as SRC_USER)
echo "[2/8] Exporting the old database..."
sudo -u "$SRC_USER" wp --path="$OLD_DIR" db export "$DB_DUMP" --quiet
check_status "Failed to export the old database."

# 3. Copy files to the new domain
echo "[3/8] Copying files (excluding cache and backups)..."
if [ "$OVERWRITE_MODE" = true ]; then
  echo "      Clearing out old files in target directory..."
  find "$NEW_DIR" -mindepth 1 -delete
else
  echo "      Removing HestiaCP default index.html..."
  rm -f "$NEW_DIR/index.html" "$NEW_DIR/robots.txt"
fi

rsync -a --exclude 'wp-content/cache/*' \
  --exclude 'wp-content/updraft/*' \
  --exclude 'wp-content/debug.log' \
  "$OLD_DIR/" "$NEW_DIR/"
check_status "Failed to sync files."

chown -R "$DEST_USER:$DEST_USER" "$NEW_DIR"
check_status "Failed to update ownership of the copied files."

# 4. Create or reuse the database
if [ "$REUSE_DB" = false ]; then
  echo "[4/8] Creating new database..."
  NEW_DB_BASENAME="cln_${RAND_STR}"
  v-add-database "$DEST_USER" "$NEW_DB_BASENAME" "$NEW_DB_BASENAME" "$NEW_DB_PASS"
  check_status "Failed to create the database in HestiaCP."
else
  echo "[4/8] Reusing existing database ($NEW_DB_NAME)."
fi

# 5. Update credentials in wp-config.php (Run as DEST_USER)
echo "[5/8] Updating wp-config.php credentials, salts, and cache..."

sudo -u "$DEST_USER" wp --path="$NEW_DIR" config set DB_NAME "$NEW_DB_NAME"
sudo -u "$DEST_USER" wp --path="$NEW_DIR" config set DB_USER "$NEW_DB_USER"
sudo -u "$DEST_USER" wp --path="$NEW_DIR" config set DB_PASSWORD "$NEW_DB_PASS"
if [ "$REUSE_DB" = true ]; then
  sudo -u "$DEST_USER" wp --path="$NEW_DIR" config set DB_HOST "$NEW_DB_HOST"
fi

echo "      Regenerating WordPress security salts..."
sudo -u "$DEST_USER" wp --path="$NEW_DIR" config shuffle-salts --quiet

echo "      Setting unique Redis cache prefixes..."
REDIS_SAFE_PREFIX="${NEW_WEB_DOMAIN//./_}_"
sudo -u "$DEST_USER" wp --path="$NEW_DIR" config set WP_CACHE_KEY_SALT "$REDIS_SAFE_PREFIX" --type=constant --quiet
sudo -u "$DEST_USER" wp --path="$NEW_DIR" config set WP_REDIS_PREFIX "$REDIS_SAFE_PREFIX" --type=constant --quiet

# 6. Import the database to the new site (Run as DEST_USER)
echo "[6/8] Importing the database..."
sudo -u "$DEST_USER" wp --path="$NEW_DIR" db import "$DB_DUMP" --quiet
check_status "Failed to import the database."

# 7. Search and Replace old domain with new domain
echo "[7/8] Running search and replace in the database..."
OLD_WP_URL=$(sudo -u "$SRC_USER" wp --path="$OLD_DIR" option get home --quiet)
NEW_WP_URL="https://$NEW_DOMAIN"

echo "    Replacing URLs: $OLD_WP_URL -> $NEW_WP_URL"
sudo -u "$DEST_USER" wp --path="$NEW_DIR" search-replace "$OLD_WP_URL" "$NEW_WP_URL" --all-tables --quiet
check_status "Failed WP-CLI URL search and replace."

echo "    Replacing absolute server paths: $OLD_DIR -> $NEW_DIR"
sudo -u "$DEST_USER" wp --path="$NEW_DIR" search-replace "$OLD_DIR" "$NEW_DIR" --all-tables --quiet

# 8. Flush Object Cache
echo "[8/8] Flushing object cache..."
sudo -u "$DEST_USER" wp --path="$NEW_DIR" cache flush --quiet

echo "Cleaning up temporary files..."
rm -f "$DB_DUMP"

echo "=================================================================="
echo "✅ Clone Successful!"
echo "Source Site:  $OLD_DOMAIN (User: $SRC_USER)"
echo "New Site:     $NEW_DOMAIN (User: $DEST_USER)"
echo "Database:     $NEW_DB_NAME"
echo "=================================================================="
