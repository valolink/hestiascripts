#!/bin/bash

if [ "$EUID" -ne 0 ]; then
  echo "❌ ERROR: Please run as root."
  exit 1
fi

export PATH=$PATH:/usr/local/hestia/bin

# --- Flag Parsing ---
HESTIA_USER=""
DOMAIN=""
SKIP_BACKUP=false
DRY_RUN=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --user=*)      HESTIA_USER="${1#*=}" ;;
    --domain=*)    DOMAIN="${1#*=}" ;;
    --skip-backup) SKIP_BACKUP=true ;;
    --dry-run)     DRY_RUN=true ;;
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
[ "$DRY_RUN" = true ] && echo "  WordPress Updates (Dry Run): $DOMAIN" || echo "  WordPress Updates: $DOMAIN"
echo "======================================================"

# --- Check what's available ---
echo ""
echo "Checking for available updates..."

CORE_UPDATE=$($WP core check-update --format=csv --fields=version 2>/dev/null | tail -n +2 | head -n1)
PLUGIN_UPDATES=$($WP plugin list --update=available --format=count 2>/dev/null); PLUGIN_UPDATES=${PLUGIN_UPDATES:-0}
THEME_UPDATES=$($WP theme list --update=available --format=count 2>/dev/null); THEME_UPDATES=${THEME_UPDATES:-0}

if [ -z "$CORE_UPDATE" ] && [ "$PLUGIN_UPDATES" -eq 0 ] && [ "$THEME_UPDATES" -eq 0 ]; then
  echo "✅ Everything is up to date. Nothing to do."
  exit 0
fi

[ -n "$CORE_UPDATE" ]          && echo "  ⚠️  Core:    Update available → $CORE_UPDATE"
[ "$PLUGIN_UPDATES" -gt 0 ]   && echo "  ⚠️  Plugins: $PLUGIN_UPDATES update(s) available"
[ "$THEME_UPDATES" -gt 0 ]    && echo "  ⚠️  Themes:  $THEME_UPDATES update(s) available"

# --- Dry Run ---
if [ "$DRY_RUN" = true ]; then
  if [ -n "$CORE_UPDATE" ]; then
    WP_VERSION=$($WP core version 2>/dev/null)
    echo ""
    echo "Core: $WP_VERSION → $CORE_UPDATE"
  fi
  if [ "$PLUGIN_UPDATES" -gt 0 ]; then
    echo ""
    echo "Plugins with updates:"
    $WP plugin list --update=available --fields=name,version --format=table 2>/dev/null
  fi
  if [ "$THEME_UPDATES" -gt 0 ]; then
    echo ""
    echo "Themes with updates:"
    $WP theme list --update=available --fields=name,version --format=table 2>/dev/null
  fi
  echo ""
  echo "Run without --dry-run to apply updates."
  exit 0
fi

# --- Backup ---
BACKUP_FILE=""
if [ "$SKIP_BACKUP" = false ]; then
  echo ""
  echo "---------------------------------------------------"
  BACKUP_DIR="/home/$HESTIA_USER/backup"
  TIMESTAMP=$(date +%Y%m%d_%H%M%S)
  BACKUP_FILE="$BACKUP_DIR/${DOMAIN}_pre_update_${TIMESTAMP}.sql.gz"

  mkdir -p "$BACKUP_DIR"
  chown "$HESTIA_USER:$HESTIA_USER" "$BACKUP_DIR"

  echo "Backing up database to $BACKUP_FILE ..."
  $WP db export - 2>/dev/null | gzip > "$BACKUP_FILE"
  if [ "${PIPESTATUS[0]}" -eq 0 ] && [ -s "$BACKUP_FILE" ]; then
    BACKUP_SIZE=$(du -sh "$BACKUP_FILE" | cut -f1)
    echo "✅ Backup complete ($BACKUP_SIZE)"
  else
    echo "❌ ERROR: Database backup failed. Aborting."
    echo "   Use --skip-backup to proceed without a backup."
    rm -f "$BACKUP_FILE"
    exit 1
  fi
fi

# Ensure maintenance mode is always disabled on exit
cleanup() {
  $WP maintenance-mode deactivate &>/dev/null
}
trap cleanup EXIT

ERRORS=()

# --- Maintenance Mode On ---
echo ""
echo "---------------------------------------------------"
echo "Enabling maintenance mode..."
$WP maintenance-mode activate 2>/dev/null
echo ""

# --- Core Update ---
if [ -n "$CORE_UPDATE" ]; then
  echo "[ Core Update ]"
  WP_VERSION_BEFORE=$($WP core version 2>/dev/null)
  $WP core update 2>&1
  if [ $? -eq 0 ]; then
    WP_VERSION_AFTER=$($WP core version 2>/dev/null)
    echo "✅ Core updated: $WP_VERSION_BEFORE → $WP_VERSION_AFTER"
    echo ""
    echo "   Running database upgrade..."
    $WP core update-db 2>&1
  else
    ERRORS+=("Core update failed")
    echo "❌ Core update failed — skipping DB upgrade."
  fi
  echo ""
fi

# --- Plugin Updates ---
if [ "$PLUGIN_UPDATES" -gt 0 ]; then
  echo "[ Plugin Updates ]"
  $WP plugin update --all 2>&1
  [ $? -ne 0 ] && ERRORS+=("One or more plugin updates failed")
  echo ""
fi

# --- Theme Updates ---
if [ "$THEME_UPDATES" -gt 0 ]; then
  echo "[ Theme Updates ]"
  $WP theme update --all 2>&1
  [ $? -ne 0 ] && ERRORS+=("One or more theme updates failed")
  echo ""
fi

# --- Flush Cache ---
echo "Flushing cache..."
$WP cache flush --quiet 2>/dev/null
echo "✅ Cache flushed"

# --- Maintenance Mode Off (also called by trap) ---
echo ""
echo "Disabling maintenance mode..."
$WP maintenance-mode deactivate 2>/dev/null
trap - EXIT

# --- Summary ---
echo ""
echo "======================================================"
echo "  Summary: $DOMAIN"
echo "======================================================"
[ -n "$CORE_UPDATE" ] && [ ! " ${ERRORS[*]} " =~ "Core" ] && echo "  ✅ Core updated to $($WP core version 2>/dev/null)"
[ "$PLUGIN_UPDATES" -gt 0 ] && echo "  ✅ $PLUGIN_UPDATES plugin(s) updated"
[ "$THEME_UPDATES" -gt 0 ] && echo "  ✅ $THEME_UPDATES theme(s) updated"
[ -n "$BACKUP_FILE" ] && echo "  💾 Backup: $BACKUP_FILE"

if [ ${#ERRORS[@]} -gt 0 ]; then
  echo ""
  echo "  ⚠️  Errors:"
  for err in "${ERRORS[@]}"; do
    echo "     - $err"
  done
fi
echo "======================================================"
