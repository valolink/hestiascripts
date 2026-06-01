#!/bin/bash

# Ensure the script is run as root
if [ "$EUID" -ne 0 ]; then
  echo "❌ ERROR: Please run this script as root."
  exit 1
fi

# Assign input arguments to variables
HESTIA_USER=$1
OLD_DOMAIN=$2
NEW_DOMAIN=$3

# Basic validation to ensure all arguments are provided
if [ -z "$HESTIA_USER" ] || [ -z "$OLD_DOMAIN" ] || [ -z "$NEW_DOMAIN" ]; then
  echo "Usage: $0 <hestia_user> <old_domain> <new_domain>"
  echo "Example: $0 admin oldsite.com newsite.com"
  exit 1
fi

# --- Error Handling Function ---
check_status() {
  if [ $? -ne 0 ]; then
    echo "❌ CRITICAL ERROR: $1"
    echo "⚠️ Script aborted to prevent damage. You may need to manually clean up partial files or databases."
    exit 1
  fi
}

# Add Hestia binaries to path
export PATH=$PATH:/usr/local/hestia/bin

# Strip 'www.' to get the actual HestiaCP base domain names for directories
OLD_WEB_DOMAIN="${OLD_DOMAIN#www.}"
NEW_WEB_DOMAIN="${NEW_DOMAIN#www.}"

# Define Paths & Random Generation using the stripped web domains
OLD_DIR="/home/$HESTIA_USER/web/$OLD_WEB_DOMAIN/public_html"
NEW_DIR="/home/$HESTIA_USER/web/$NEW_WEB_DOMAIN/public_html"
RAND_STR=$(openssl rand -hex 3) # Generates 6 random characters safely
DB_DUMP="/tmp/${OLD_WEB_DOMAIN}_clone_${RAND_STR}.sql"

# --- Pre-Flight Checks ---
echo "Running pre-flight checks..."

if [ ! -d "/home/$HESTIA_USER" ]; then
  echo "❌ ERROR: Hestia user '$HESTIA_USER' does not exist."
  exit 1
fi

if [ ! -d "$OLD_DIR" ]; then
  echo "❌ ERROR: Source directory $OLD_DIR does not exist. Did you type the old domain correctly?"
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

# 1st: Check and enable bash shell for the user if necessary
USER_CONF="/usr/local/hestia/data/users/$HESTIA_USER/user.conf"
if [ -f "$USER_CONF" ]; then
  CURRENT_SHELL=$(grep "^SHELL=" "$USER_CONF" | cut -d"'" -f2)
  if [[ "$CURRENT_SHELL" != "bash" && "$CURRENT_SHELL" != "sh" ]]; then
    echo "⚠️ User $HESTIA_USER lacks shell access (Current: $CURRENT_SHELL)."
    echo "   Granting bash access via v-change-user-shell..."
    v-change-user-shell "$HESTIA_USER" "bash"
    check_status "Failed to change user shell to bash."
  fi
fi

# 2nd: NOW check if proc_open or exec are disabled in PHP CLI (Using exact array matching)
sudo -u "$HESTIA_USER" php -r "
  \$d = array_map('trim', explode(',', ini_get('disable_functions')));
  exit((in_array('exec', \$d) || in_array('proc_open', \$d)) ? 1 : 0);
"
if [ $? -eq 1 ]; then
  echo "❌ ERROR: 'proc_open' and/or 'exec' are still disabled in the PHP CLI configuration."
  echo "WP-CLI requires these functions to run database exports and imports."
  echo "Note: You must remove them from the CLI php.ini (e.g., /etc/php/X.Y/cli/php.ini), not just the FPM config."
  exit 1
fi

echo "Pre-flight checks passed."

# --- Destination & Overwrite Check ---
OVERWRITE_MODE=false
REUSE_DB=false
NEW_DB_NAME="${HESTIA_USER}_cln_${RAND_STR}"
NEW_DB_USER="${HESTIA_USER}_cln_${RAND_STR}"
NEW_DB_PASS=$(openssl rand -base64 18 | tr -dc 'a-zA-Z0-9' | head -c 16)
NEW_DB_HOST="localhost"

# Use NEW_WEB_DOMAIN here so Hestia checks for the base domain correctly
if v-list-web-domain "$HESTIA_USER" "$NEW_WEB_DOMAIN" &>/dev/null; then
  echo "⚠️ Target domain ($NEW_WEB_DOMAIN) already exists in HestiaCP."
  read -p "Do you want to OVERWRITE the existing site? Type 'yes' to continue: " CONFIRM
  if [ "$CONFIRM" != "yes" ]; then
    echo "Aborting clone process."
    exit 0
  fi
  OVERWRITE_MODE=true

  # Check if we can reuse the existing database
  if [ -f "$NEW_DIR/wp-config.php" ]; then
    echo "🔍 Found existing wp-config.php. Checking database connection..."
    if sudo -u "$HESTIA_USER" wp --path="$NEW_DIR" db check --quiet &>/dev/null; then
      REUSE_DB=true

      # Extract existing credentials
      NEW_DB_NAME=$(sudo -u "$HESTIA_USER" wp --path="$NEW_DIR" config get DB_NAME)
      NEW_DB_USER=$(sudo -u "$HESTIA_USER" wp --path="$NEW_DIR" config get DB_USER)
      NEW_DB_PASS=$(sudo -u "$HESTIA_USER" wp --path="$NEW_DIR" config get DB_PASSWORD)
      NEW_DB_HOST=$(sudo -u "$HESTIA_USER" wp --path="$NEW_DIR" config get DB_HOST)

      echo "♻️ Preparing to reuse existing database: $NEW_DB_NAME"
      echo "[1/8] Emptying existing target database..."
      sudo -u "$HESTIA_USER" wp --path="$NEW_DIR" db reset --yes --quiet
      check_status "Failed to reset existing destination database."
    else
      echo "⚠️ Existing database check failed. A new database will be created."
    fi
  fi
fi

echo "---------------------------------------------------"
echo "Starting clone process: $OLD_DOMAIN -> $NEW_DOMAIN"
echo "---------------------------------------------------"

# 1. Create the new web domain (if not overwriting)
if [ "$OVERWRITE_MODE" = false ]; then
  echo "[1/8] Creating new web domain ($NEW_WEB_DOMAIN) in HestiaCP..."
  v-add-web-domain "$HESTIA_USER" "$NEW_WEB_DOMAIN"
  check_status "Failed to create web domain in Hestia. Check if the domain already exists."
fi

# 2. Export the database using WP-CLI
echo "[2/8] Exporting the old database..."
sudo -u "$HESTIA_USER" wp --path="$OLD_DIR" db export "$DB_DUMP" --quiet
check_status "Failed to export the old database. Check if the old site's DB credentials are valid."

# 3. Copy files to the new domain
echo "[3/8] Copying files..."
if [ "$OVERWRITE_MODE" = true ]; then
  echo "      Clearing out old files in target directory..."
  # Safely clear target directory without deleting the directory itself
  find "$NEW_DIR" -mindepth 1 -delete
else
  # NEW: Remove HestiaCP default files from fresh domain creations
  echo "      Removing HestiaCP default index.html..."
  rm -f "$NEW_DIR/index.html" "$NEW_DIR/robots.txt"
fi

cp -a "$OLD_DIR/." "$NEW_DIR/"
check_status "Failed to copy files. Check disk space and permissions."
chown -R "$HESTIA_USER:$HESTIA_USER" "$NEW_DIR"
check_status "Failed to update ownership of the copied files."

# 4. Create or reuse the database
if [ "$REUSE_DB" = false ]; then
  echo "[4/8] Creating new database ($NEW_DB_NAME)..."
  NEW_DB_BASENAME="cln_${RAND_STR}"
  v-add-database "$HESTIA_USER" "$NEW_DB_BASENAME" "$NEW_DB_BASENAME" "$NEW_DB_PASS"
  check_status "Failed to create the database in HestiaCP. Check your database limits."
else
  echo "[4/8] Reusing existing database ($NEW_DB_NAME) - already emptied."
fi

# 5. Update credentials in wp-config.php
echo "[5/8] Updating wp-config.php credentials, salts, and cache..."

# Database Credentials
sudo -u "$HESTIA_USER" wp --path="$NEW_DIR" config set DB_NAME "$NEW_DB_NAME"
check_status "Failed to set DB_NAME in wp-config.php."
sudo -u "$HESTIA_USER" wp --path="$NEW_DIR" config set DB_USER "$NEW_DB_USER"
check_status "Failed to set DB_USER in wp-config.php."
sudo -u "$HESTIA_USER" wp --path="$NEW_DIR" config set DB_PASSWORD "$NEW_DB_PASS"
check_status "Failed to set DB_PASSWORD in wp-config.php."
if [ "$REUSE_DB" = true ]; then
  sudo -u "$HESTIA_USER" wp --path="$NEW_DIR" config set DB_HOST "$NEW_DB_HOST"
  check_status "Failed to set DB_HOST in wp-config.php."
fi

# Shuffle WordPress Security Salts
echo "      Regenerating WordPress security salts..."
sudo -u "$HESTIA_USER" wp --path="$NEW_DIR" config shuffle-salts --quiet
check_status "Failed to shuffle WordPress salts."

# Update Redis Cache Prefixes to prevent cache collisions between old and new site
echo "      Setting unique Redis cache prefixes..."
# Replace dots with underscores in the domain for a clean prefix format
REDIS_SAFE_PREFIX="${NEW_WEB_DOMAIN//./_}_"

sudo -u "$HESTIA_USER" wp --path="$NEW_DIR" config set WP_CACHE_KEY_SALT "$REDIS_SAFE_PREFIX" --type=constant --quiet
check_status "Failed to set WP_CACHE_KEY_SALT."

sudo -u "$HESTIA_USER" wp --path="$NEW_DIR" config set WP_REDIS_PREFIX "$REDIS_SAFE_PREFIX" --type=constant --quiet
check_status "Failed to set WP_REDIS_PREFIX."

# 6. Import the database to the new site
echo "[6/8] Importing the database..."
sudo -u "$HESTIA_USER" wp --path="$NEW_DIR" db import "$DB_DUMP" --quiet
check_status "Failed to import the database into the new site."

# 7. Search and Replace old domain with new domain
echo "[7/8] Running search and replace in the database..."

# Fetch the exact Site URL from the OLD WordPress installation
OLD_WP_URL=$(sudo -u "$HESTIA_USER" wp --path="$OLD_DIR" option get home --quiet)

# Define the NEW WordPress URL (Assuming https is used)
# This retains the 'www.' because it uses the original $NEW_DOMAIN variable
NEW_WP_URL="https://$NEW_DOMAIN"

# Output what is actually being replaced for clarity in the terminal
echo "    Replacing: $OLD_WP_URL -> $NEW_WP_URL"

# Run the search-replace using the full URLs
sudo -u "$HESTIA_USER" wp --path="$NEW_DIR" search-replace "$OLD_WP_URL" "$NEW_WP_URL" --all-tables --quiet
check_status "Failed to run WP-CLI search and replace for URLs."

# Optional: Run a secondary pass for the raw domain name to update emails/hardcoded paths.
# Prevent double-replacement if the new domain is a subdomain of the old domain (e.g., dev.domain.com)
# if [[ "$NEW_WEB_DOMAIN" != *"$OLD_WEB_DOMAIN"* ]]; then
#   sudo -u "$HESTIA_USER" wp --path="$NEW_DIR" search-replace "$OLD_WEB_DOMAIN" "$NEW_WEB_DOMAIN" --all-tables --quiet
#   check_status "Failed to run secondary WP-CLI search and replace for raw domain."
# else
#   echo "    Skipping raw domain replace to prevent double-subdomain nesting."
# fi

# 8. Flush Object Cache
echo "[8/8] Flushing object cache to prevent stale data..."
sudo -u "$HESTIA_USER" wp --path="$NEW_DIR" cache flush --quiet

# Cleanup
echo "Cleaning up temporary files..."
rm -f "$DB_DUMP"

echo "=================================================================="
echo "✅ Clone Successful!"
echo "User:         $HESTIA_USER"
echo "New Domain:   $NEW_DOMAIN"
echo "Database:     $NEW_DB_NAME"
echo "=================================================================="
