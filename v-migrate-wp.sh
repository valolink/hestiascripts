#!/bin/bash
# v-migrate-wp — finish importing a WordPress site rsynced to this server
#
# Pre-conditions (do these before running):
#   1. HestiaCP user exists
#   2. Web domain created in HestiaCP (no SSL needed yet)
#   3. WordPress files rsynced to public_html/
#   4. DB exported as migrate.sql inside public_html (or use --sql=PATH)

if [ "$EUID" -ne 0 ]; then
  echo "ERROR: Please run as root."
  exit 1
fi

export PATH=$PATH:/usr/local/hestia/bin

check_status() {
  if [ $? -ne 0 ]; then
    echo "CRITICAL ERROR: $1"
    echo "Script aborted. Clean up manually if needed."
    exit 1
  fi
}

show_help() {
  cat <<'EOF'
USAGE: v-migrate-wp [OPTIONS]

Finish importing a WordPress site that has been rsynced to a HestiaCP domain.

Pre-conditions:
  - HestiaCP user and domain already created (no SSL needed yet)
  - WordPress files rsynced to /home/USER/web/DOMAIN/public_html/
  - DB exported as migrate.sql inside public_html (or use --sql=PATH)

Steps performed:
  1. Fix file ownership and permissions
  2. Create new database in HestiaCP
  3. Update wp-config.php: new DB credentials, fresh salts, unique Redis prefix
     Disable WP_DEBUG. Update WP_HOME/WP_SITEURL if hardcoded.
  4. Import SQL dump
  5. Search-replace old URL → https://DOMAIN  (both http/https variants)
  6. Flush object cache and expired transients
  7. Delete migrate.sql from public_html

OPTIONS:
  --user=USER       HestiaCP user who owns the domain
  --domain=DOMAIN   Domain name
  --sql=PATH        Path to SQL dump (default: public_html/migrate.sql)
  --old-url=URL     Old site URL for search-replace (auto-detected if omitted)
  --force           Skip confirmation prompt
  -h, --help        Show this help

EXAMPLE:
  v-migrate-wp --user=client1 --domain=example.com
  v-migrate-wp --user=client1 --domain=example.com --sql=/tmp/backup.sql --old-url=https://oldhost.com
EOF
}

# --- Flags ---
WEB_USER=""
DOMAIN=""
SQL_PATH=""
OLD_URL_FLAG=""
FORCE=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --user=*)    WEB_USER="${1#*=}" ;;
    --domain=*)  DOMAIN="${1#*=}" ;;
    --sql=*)     SQL_PATH="${1#*=}" ;;
    --old-url=*) OLD_URL_FLAG="${1#*=}" ;;
    --force)     FORCE=true ;;
    -h|--help)   show_help; exit 0 ;;
    *) echo "ERROR: Unknown option: $1"; show_help; exit 1 ;;
  esac
  shift
done

echo "==================================================="
echo "  HestiaCP WordPress Migration"
echo "==================================================="

# --- Interactive prompts for missing flags ---
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

[ -z "$SQL_PATH" ] && SQL_PATH="$WEB_DIR/migrate.sql"

# --- Pre-flight ---
echo ""
echo "---------------------------------------------------"
echo "Pre-flight checks..."

if ! v-list-user "$WEB_USER" &>/dev/null; then
  echo "ERROR: HestiaCP user '$WEB_USER' does not exist."
  exit 1
fi

if ! v-list-web-domain "$WEB_USER" "$DOMAIN" &>/dev/null; then
  echo "ERROR: Domain '$DOMAIN' not found for user '$WEB_USER' in HestiaCP."
  echo "Create the domain first (without SSL), then rsync files and run this script."
  exit 1
fi

if [ ! -d "$WEB_DIR" ]; then
  echo "ERROR: $WEB_DIR does not exist."
  exit 1
fi

if [ ! -f "$WEB_DIR/wp-config.php" ]; then
  echo "ERROR: No wp-config.php in $WEB_DIR."
  echo "Rsync the WordPress files to public_html before running this script."
  exit 1
fi

if [ ! -f "$SQL_PATH" ]; then
  echo "ERROR: SQL dump not found at $SQL_PATH"
  echo "Place the exported database as migrate.sql in public_html, or use --sql=PATH"
  exit 1
fi

if ! command -v wp &>/dev/null; then
  echo "ERROR: WP-CLI is not installed."
  exit 1
fi

# Ensure bash shell access for wp-cli
_ensure_bash() {
  local user="$1" conf="/usr/local/hestia/data/users/$1/user.conf"
  if [ -f "$conf" ]; then
    local shell; shell=$(grep "^SHELL=" "$conf" | cut -d"'" -f2)
    if [[ "$shell" != "bash" && "$shell" != "sh" ]]; then
      echo "  Granting bash shell to $user..."
      v-change-user-shell "$user" bash
      check_status "Failed to set shell for $user"
    fi
  fi
}
_ensure_bash "$WEB_USER"

# Check PHP CLI exec functions are available
_check_php_cli() {
  sudo -u "$1" php -r "
    \$d = array_map('trim', explode(',', ini_get('disable_functions')));
    exit((in_array('exec', \$d) || in_array('proc_open', \$d)) ? 1 : 0);
  "
  if [ $? -eq 1 ]; then
    echo "ERROR: exec/proc_open disabled in PHP CLI for $1 — WP-CLI cannot run."
    exit 1
  fi
}
_check_php_cli "$WEB_USER"

SQL_SIZE=$(du -sh "$SQL_PATH" 2>/dev/null | cut -f1)
FILES_SIZE=$(du -sh "$WEB_DIR" 2>/dev/null | cut -f1)

echo "Pre-flight checks passed."
echo ""
echo "  User:      $WEB_USER"
echo "  Domain:    $DOMAIN"
echo "  Web root:  $WEB_DIR  ($FILES_SIZE)"
echo "  SQL dump:  $SQL_PATH  ($SQL_SIZE)"
[ -n "$OLD_URL_FLAG" ] && echo "  Old URL:   $OLD_URL_FLAG  (provided)"
echo ""
echo "  Steps: permissions → create DB → update wp-config → import SQL"
echo "         → search-replace URL → flush caches → delete migrate.sql"
echo ""

if [ "$FORCE" = false ] && [ -t 0 ]; then
  read -r -p "  Proceed? [y/N] " _ans
  [[ "$_ans" =~ ^[Yy]$ ]] || { echo "  Aborted."; exit 0; }
  echo ""
fi

RAND_STR=$(openssl rand -hex 3)
DB_BASENAME="mig_${RAND_STR}"
DB_NAME="${WEB_USER}_${DB_BASENAME}"
DB_USER="${WEB_USER}_${DB_BASENAME}"
DB_PASS=$(openssl rand -base64 18 | tr -dc 'a-zA-Z0-9' | head -c 16)

# --- Step 1: Fix permissions ---
echo "[1/6] Fixing file ownership and permissions..."
chown -R "$WEB_USER:$WEB_USER" "$WEB_DIR"
check_status "chown failed"
find "$WEB_DIR" -type d -exec chmod 755 {} \;
find "$WEB_DIR" -type f -exec chmod 644 {} \;
chmod 640 "$WEB_DIR/wp-config.php" 2>/dev/null
# Uploads must stay writable by PHP-FPM (runs as WEB_USER, so 755 is enough)
find "$WEB_DIR/wp-content/uploads" -type d -exec chmod 755 {} \; 2>/dev/null
echo "  Done."

# --- Step 2: Create database ---
echo "[2/6] Creating database in HestiaCP..."
v-add-database "$WEB_USER" "$DB_BASENAME" "$DB_BASENAME" "$DB_PASS"
check_status "Failed to create database"
echo "  Created: $DB_NAME"

# --- Step 3: Update wp-config.php ---
echo "[3/6] Updating wp-config.php..."

sudo -u "$WEB_USER" wp --path="$WEB_DIR" config set DB_NAME     "$DB_NAME" --quiet
check_status "Failed to set DB_NAME"
sudo -u "$WEB_USER" wp --path="$WEB_DIR" config set DB_USER     "$DB_USER" --quiet
check_status "Failed to set DB_USER"
sudo -u "$WEB_USER" wp --path="$WEB_DIR" config set DB_PASSWORD "$DB_PASS" --quiet
check_status "Failed to set DB_PASSWORD"
sudo -u "$WEB_USER" wp --path="$WEB_DIR" config set DB_HOST     "localhost" --quiet

echo "  Shuffling salts..."
sudo -u "$WEB_USER" wp --path="$WEB_DIR" config shuffle-salts --quiet

echo "  Setting unique Redis cache prefix..."
REDIS_PREFIX="${DOMAIN//./_}_$(openssl rand -hex 4)"
sudo -u "$WEB_USER" wp --path="$WEB_DIR" config set WP_CACHE_KEY_SALT "$REDIS_PREFIX" --type=constant --quiet
sudo -u "$WEB_USER" wp --path="$WEB_DIR" config set WP_REDIS_PREFIX   "$REDIS_PREFIX" --type=constant --quiet

echo "  Disabling WP_DEBUG..."
sudo -u "$WEB_USER" wp --path="$WEB_DIR" config set WP_DEBUG false --raw --type=constant --quiet 2>/dev/null || true

# Update WP_HOME / WP_SITEURL if hardcoded (they would override the DB values)
if sudo -u "$WEB_USER" wp --path="$WEB_DIR" config has WP_HOME 2>/dev/null; then
  echo "  Updating hardcoded WP_HOME → https://$DOMAIN"
  sudo -u "$WEB_USER" wp --path="$WEB_DIR" config set WP_HOME "https://$DOMAIN" --type=constant --quiet
fi
if sudo -u "$WEB_USER" wp --path="$WEB_DIR" config has WP_SITEURL 2>/dev/null; then
  echo "  Updating hardcoded WP_SITEURL → https://$DOMAIN"
  sudo -u "$WEB_USER" wp --path="$WEB_DIR" config set WP_SITEURL "https://$DOMAIN" --type=constant --quiet
fi

# --- Step 4: Import database ---
echo "[4/6] Importing database..."
sudo -u "$WEB_USER" wp --path="$WEB_DIR" db import "$SQL_PATH" --quiet
check_status "Database import failed"
echo "  Done."

# --- Step 5: Search-replace ---
echo "[5/6] Running search-replace..."

if [ -n "$OLD_URL_FLAG" ]; then
  OLD_URL="$OLD_URL_FLAG"
else
  OLD_URL=$(sudo -u "$WEB_USER" wp --path="$WEB_DIR" option get home \
    --skip-plugins --skip-themes 2>/dev/null)
fi

if [ -z "$OLD_URL" ]; then
  echo "  WARNING: Could not detect old URL. Skipping search-replace."
  echo "  Run manually after DNS is pointed:"
  echo "    wp search-replace 'https://oldsite.com' 'https://$DOMAIN' --all-tables --path=$WEB_DIR"
else
  NEW_URL="https://$DOMAIN"
  # Normalise: strip trailing slash
  OLD_URL="${OLD_URL%/}"
  OLD_HOST=$(echo "$OLD_URL" | sed 's|https\?://||')

  # Replace both protocol variants of old host → new https URL
  for OLD in "https://$OLD_HOST" "http://$OLD_HOST"; do
    if [ "$OLD" != "$NEW_URL" ]; then
      echo "  $OLD → $NEW_URL"
      sudo -u "$WEB_USER" wp --path="$WEB_DIR" search-replace "$OLD" "$NEW_URL" \
        --all-tables --skip-plugins --skip-themes --quiet
      check_status "search-replace failed for $OLD"
    fi
  done
fi

# --- Step 6: Flush and cleanup ---
echo "[6/6] Flushing caches and cleaning up..."

sudo -u "$WEB_USER" wp --path="$WEB_DIR" cache flush \
  --skip-plugins --skip-themes --quiet 2>/dev/null || true
sudo -u "$WEB_USER" wp --path="$WEB_DIR" transient delete --expired \
  --skip-plugins --skip-themes --quiet 2>/dev/null || true

# Remove file-based cache (WP Rocket, W3TC, etc.)
rm -rf "$WEB_DIR/wp-content/cache/"* 2>/dev/null || true

echo "  Deleting SQL dump from public_html..."
rm -f "$SQL_PATH"
# Clean up any other .sql files left in docroot root
find "$WEB_DIR" -maxdepth 1 -name "*.sql" -delete 2>/dev/null

echo ""
echo "=================================================================="
echo "Migration complete!"
echo ""
echo "  User:      $WEB_USER"
echo "  Domain:    $DOMAIN"
echo "  Database:  $DB_NAME"
[ -n "$OLD_URL" ] && echo "  URL:       $OLD_URL  →  https://$DOMAIN"
echo ""
echo "Next steps:"
echo "  1. Test before DNS cutover — add to your local /etc/hosts:"
echo "       $(hostname -I | awk '{print $1}')  $DOMAIN"
echo "  2. When DNS is pointing here, add SSL:"
echo "       v-add-web-domain-ssl $WEB_USER $DOMAIN"
echo "=================================================================="
