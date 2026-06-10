#!/bin/bash

if [ "$EUID" -ne 0 ]; then
  echo "❌ ERROR: Please run as root."
  exit 1
fi

export PATH=$PATH:/usr/local/hestia/bin

show_help() {
  cat <<'EOF'
USAGE: v-wp-info [OPTIONS]

Print a full diagnostic report for a WordPress site: versions, available
updates, active theme, PHP environment, database, security posture, admin
users, object cache status, plugin list, and content counts.

OPTIONS:
  --user=USER      HestiaCP user who owns the site
  --domain=DOMAIN  Domain to inspect
  -h, --help       Show this help

EXAMPLES:
  # Run interactively over SSH
  v-wp-info

  # Direct (e.g. via hestia-streamer)
  v-wp-info --user=admin --domain=mysite.fi
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

echo ""
echo "======================================================"
echo "  WordPress Site Info: $DOMAIN"
echo "======================================================"

# --- Site ---
echo ""
echo "[ Site ]"
SITE_TITLE=$($WP option get blogname 2>/dev/null)
SITE_URL=$($WP option get siteurl 2>/dev/null)
HOME_URL=$($WP option get home 2>/dev/null)
WP_VERSION=$($WP core version 2>/dev/null)

echo "  Title:       $SITE_TITLE"
echo "  Site URL:    $SITE_URL"
[ "$HOME_URL" != "$SITE_URL" ] && echo "  Home URL:    $HOME_URL  ⚠️  (differs from Site URL)"
echo "  WP Version:  $WP_VERSION"

if $WP core is-installed --network &>/dev/null; then
  SITE_COUNT=$($WP site list --format=count 2>/dev/null)
  echo "  Multisite:   Yes ($SITE_COUNT sites)"
else
  echo "  Multisite:   No"
fi

# --- Updates ---
echo ""
echo "[ Updates ]"

CORE_UPDATE=$($WP core check-update --format=csv --fields=version 2>/dev/null | tail -n +2 | head -n1)
if [ -n "$CORE_UPDATE" ]; then
  echo "  Core:    ⚠️  $WP_VERSION → $CORE_UPDATE"
else
  echo "  Core:    ✅ Up to date ($WP_VERSION)"
fi

PLUGIN_UPDATES=$($WP plugin list --update=available --format=count 2>/dev/null)
PLUGIN_UPDATES=${PLUGIN_UPDATES:-0}
if [ "$PLUGIN_UPDATES" -gt 0 ]; then
  echo "  Plugins: ⚠️  $PLUGIN_UPDATES update(s) available"
else
  echo "  Plugins: ✅ All up to date"
fi

THEME_UPDATES=$($WP theme list --update=available --format=count 2>/dev/null)
THEME_UPDATES=${THEME_UPDATES:-0}
if [ "$THEME_UPDATES" -gt 0 ]; then
  echo "  Themes:  ⚠️  $THEME_UPDATES update(s) available"
else
  echo "  Themes:  ✅ All up to date"
fi

# --- Active Theme ---
echo ""
echo "[ Active Theme ]"
read -r THEME_NAME THEME_VERSION THEME_UPDATE < <($WP theme list --status=active --fields=name,version,update --format=tsv 2>/dev/null | tail -n +2)
STYLESHEET=$($WP eval "echo get_stylesheet();" 2>/dev/null)
TEMPLATE=$($WP eval "echo get_template();" 2>/dev/null)

echo "  Name:    $THEME_NAME ($THEME_VERSION)"
if [ -n "$TEMPLATE" ] && [ "$STYLESHEET" != "$TEMPLATE" ]; then
  echo "  Type:    Child theme (parent: $TEMPLATE)"
else
  echo "  Type:    Standalone"
fi
[ "$THEME_UPDATE" = "available" ] && echo "  Update:  ⚠️  Available"

# --- PHP Environment ---
echo ""
echo "[ PHP Environment ]"
PHP_VERSION=$(sudo -u "$HESTIA_USER" php -r "echo PHP_VERSION;" 2>/dev/null)
PHP_MEMORY=$(sudo -u "$HESTIA_USER" php -r "echo ini_get('memory_limit');" 2>/dev/null)
PHP_UPLOAD=$(sudo -u "$HESTIA_USER" php -r "echo ini_get('upload_max_filesize');" 2>/dev/null)
PHP_POST=$(sudo -u "$HESTIA_USER" php -r "echo ini_get('post_max_size');" 2>/dev/null)
PHP_EXEC=$(sudo -u "$HESTIA_USER" php -r "echo ini_get('max_execution_time');" 2>/dev/null)

echo "  PHP Version:   $PHP_VERSION"
echo "  Memory Limit:  $PHP_MEMORY"
echo "  Upload Max:    $PHP_UPLOAD  (post max: $PHP_POST)"
echo "  Max Exec Time: ${PHP_EXEC}s"

# --- Database ---
echo ""
echo "[ Database ]"
DB_PREFIX=$($WP config get table_prefix 2>/dev/null)
DB_SIZE=$($WP db query "SELECT CONCAT(ROUND(SUM(data_length + index_length) / 1024 / 1024, 1), ' MB') FROM information_schema.tables WHERE table_schema = DATABASE();" --skip-column-names 2>/dev/null | tr -d '[:space:]')
MYSQL_VERSION=$($WP db query "SELECT VERSION();" --skip-column-names 2>/dev/null | tr -d '[:space:]')

echo "  Size:          $DB_SIZE"
echo "  Table Prefix:  $DB_PREFIX"
[ "$DB_PREFIX" = "wp_" ] && echo "                 ⚠️  Default prefix"
echo "  MySQL/MariaDB: $MYSQL_VERSION"

# --- Security ---
echo ""
echo "[ Security ]"

DEBUG_VAL=$($WP config get WP_DEBUG 2>/dev/null || echo "false")
if [ "$DEBUG_VAL" = "true" ] || [ "$DEBUG_VAL" = "1" ]; then
  echo "  WP_DEBUG:          ON  ⚠️"
else
  echo "  WP_DEBUG:          Off ✅"
fi

FILE_EDIT=$($WP config get DISALLOW_FILE_EDIT 2>/dev/null || echo "false")
if [ "$FILE_EDIT" = "true" ] || [ "$FILE_EDIT" = "1" ]; then
  echo "  File Editing:      Disabled ✅"
else
  echo "  File Editing:      Enabled  ⚠️  (consider DISALLOW_FILE_EDIT)"
fi

ADMIN_EXISTS=$($WP user list --login=admin --format=count 2>/dev/null || echo "0")
if [ "${ADMIN_EXISTS:-0}" -ge 1 ] 2>/dev/null; then
  echo "  'admin' username:  Exists ⚠️"
else
  echo "  'admin' username:  Not found ✅"
fi

# --- Admin Users ---
echo ""
echo "[ Admin Users ]"
$WP user list --role=administrator --fields=user_login,user_email --format=table 2>/dev/null

# --- Object Cache ---
echo ""
echo "[ Object Cache ]"
if [ -f "$WP_PATH/wp-content/object-cache.php" ]; then
  CACHE_NAME=$(grep -m1 "Plugin Name:" "$WP_PATH/wp-content/object-cache.php" 2>/dev/null | sed 's/.*Plugin Name:[[:space:]]*//')
  if [ -n "$CACHE_NAME" ]; then
    echo "  Status: Active ($CACHE_NAME)"
  else
    echo "  Status: Active (object-cache.php present)"
  fi
else
  echo "  Status: No object cache drop-in"
fi

# --- Active Plugins ---
echo ""
TOTAL_ACTIVE=$($WP plugin list --status=active --format=count 2>/dev/null)
TOTAL_INACTIVE=$($WP plugin list --status=inactive --format=count 2>/dev/null)
echo "[ Plugins ] ($TOTAL_ACTIVE active, $TOTAL_INACTIVE inactive, $PLUGIN_UPDATES update(s) available)"
echo ""
while IFS=$'\t' read -r name status version update; do
  [ "$status" != "active" ] && continue
  if [ "$update" = "available" ]; then
    printf "  ⚠️  %-40s %s\n" "$name" "$version"
  else
    printf "  ✅  %-40s %s\n" "$name" "$version"
  fi
done < <($WP plugin list --fields=name,status,version,update --format=tsv 2>/dev/null)

# --- Content ---
echo ""
echo "[ Content ]"
POSTS=$($WP post list --post_type=post --post_status=publish --format=count 2>/dev/null)
PAGES=$($WP post list --post_type=page --post_status=publish --format=count 2>/dev/null)
MEDIA=$($WP post list --post_type=attachment --format=count 2>/dev/null)
PENDING_COMMENTS=$($WP comment list --status=hold --format=count 2>/dev/null)

echo "  Posts (published):  $POSTS"
echo "  Pages (published):  $PAGES"
echo "  Media items:        $MEDIA"
echo "  Pending comments:   $PENDING_COMMENTS"

echo ""
echo "======================================================"
echo "  Done."
echo "======================================================"
