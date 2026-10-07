#!/bin/bash

if [ "$EUID" -ne 0 ]; then
  echo "❌ ERROR: Please run as root."
  exit 1
fi

export PATH=$PATH:/usr/local/hestia/bin

show_help() {
  cat <<'EOF'
USAGE: v-wp-update [OPTIONS]

Run all available WordPress updates (core, plugins, themes).
Backs up the database to /root/pre-update/USER/ before updating unless
--skip-backup is set (the newest 5 dumps per domain are kept).
Enables maintenance mode for the duration and disables it on exit.

OPTIONS:
  --user=USER      HestiaCP user who owns the site
  --domain=DOMAIN  Domain to update
  --skip-backup    Skip the database backup before updating
  --dry-run        Show what would be updated without applying any changes
  -h, --help       Show this help

EXAMPLES:
  # Check what needs updating without touching anything
  v-wp-update --user=admin --domain=mysite.fi --dry-run

  # Run all updates
  v-wp-update --user=admin --domain=mysite.fi
EOF
}

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
    -h|--help)     show_help; exit 0 ;;
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

# Elementor Pro keeps its own copy of the update data, download token
# included, in elementor_pro_remote_info_api_data_* options. One held a token
# that had expired 29 h earlier (2026-10-01), so the update would have failed
# with a 401. Clear it with Elementor Pro's own function so the check below
# fetches a fresh one.
if $WP plugin is-active elementor-pro 2>/dev/null; then
  $WP eval 'if (class_exists("\\ElementorPro\\Core\\Upgrade\\Upgrades")) { \ElementorPro\Core\Upgrade\Upgrades::_remove_remote_info_api_data(); }' &>/dev/null
fi

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
  # Not /home/USER/backup: the nightly v-backup-user-config starts with
  # `rm -fr /home/$user/backup/`, so a dump there was gone by the next
  # morning. Root-only, outside the web tree, untouched by Hestia.
  BACKUP_DIR="/root/pre-update/$HESTIA_USER"
  TIMESTAMP=$(date +%Y%m%d_%H%M%S)
  BACKUP_FILE="$BACKUP_DIR/${DOMAIN}_pre_update_${TIMESTAMP}.sql.gz"

  mkdir -p "$BACKUP_DIR"
  chmod 700 /root/pre-update

  echo "Backing up database to $BACKUP_FILE ..."
  $WP db export - 2>/dev/null | gzip > "$BACKUP_FILE"
  if [ "${PIPESTATUS[0]}" -eq 0 ] && [ -s "$BACKUP_FILE" ]; then
    BACKUP_SIZE=$(du -sh "$BACKUP_FILE" | cut -f1)
    echo "✅ Backup complete ($BACKUP_SIZE)"
    ls -1t "$BACKUP_DIR/${DOMAIN}_pre_update_"*.sql.gz 2>/dev/null | tail -n +6 | xargs -r rm -f
  else
    echo "❌ ERROR: Database backup failed. Aborting."
    echo "   Use --skip-backup to proceed without a backup."
    rm -f "$BACKUP_FILE"
    exit 1
  fi
fi

# While .maintenance exists WordPress answers 503 with "Briefly unavailable",
# and a visitor who arrives in those seconds leaves. Cached pages never reach
# PHP, but the wp-rocket template sends every query string past the cache, so
# the visitors who do are paid clicks (?gclid=…): a Google Ads visitor hit it
# on delicatessen.fi, 2026-10-07. For the run, wp-content/maintenance.php
# (which WordPress loads instead of its own page) still says 503 but reloads
# itself after a few seconds, so the click lands. A site's own
# maintenance.php is left alone; ours goes when the run ends.
MAINT_PAGE="$WP_PATH/wp-content/maintenance.php"
MAINT_MARK="valolink v-wp-update maintenance page"
install_maintenance_page() {
  [ -e "$MAINT_PAGE" ] && return
  cat > "$MAINT_PAGE" <<'PHP'
<?php
// valolink v-wp-update maintenance page — removed when the update run ends.
http_response_code(503);
header('Retry-After: 10');
header('Cache-Control: no-store');
header('Content-Type: text/html; charset=utf-8');
?><!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex"><meta http-equiv="refresh" content="8">
<title>Hetki vain · One moment</title>
<style>body{font:17px/1.5 system-ui,sans-serif;display:grid;place-items:center;min-height:90vh;margin:0;color:#222;text-align:center}p{margin:.4em}</style>
</head><body><div>
<p>Päivitämme sivustoa. Sivu latautuu hetken kuluttua.</p>
<p>We are updating the site. This page reloads in a moment.</p>
</div></body></html>
PHP
  chown "$HESTIA_USER:$HESTIA_USER" "$MAINT_PAGE"
  chmod 644 "$MAINT_PAGE"
}
remove_maintenance_page() {
  grep -q "$MAINT_MARK" "$MAINT_PAGE" 2>/dev/null && rm -f "$MAINT_PAGE"
}

# Ensure maintenance mode is always disabled on exit
UPDATE_LOG=$(mktemp)
cleanup() {
  $WP maintenance-mode deactivate &>/dev/null
  remove_maintenance_page
  rm -f "$UPDATE_LOG"
}
trap cleanup EXIT

# Runs one WP-CLI update with its output on screen as it happens — EngineLink
# streams this script over SSE, and capturing into a variable held the whole
# plugin step back until it ended (~50 s on alavusikkunat.fi) — while keeping
# a copy in $OUT for the counts. $RC is WP-CLI's exit code, not tee's.
run_live() {
  "$@" 2>&1 | tee "$UPDATE_LOG"
  RC=${PIPESTATUS[0]}
  OUT=$(<"$UPDATE_LOG")
}

ERRORS=()

# WordPress's upgraders delete .maintenance when they finish, so the site is
# open again before the steps that follow them. Re-close it only for steps that
# migrate the database (core's update-db, a pending WooCommerce DB update):
# those must not run under traffic. Everything else runs with the site open,
# keeping the closed window as short as the updates themselves
# (alavusikkunat.fi 2026-10-01: ~50 s for 25 plugins).
hold_maintenance() { $WP maintenance-mode activate &>/dev/null; }

# --- Maintenance Mode On ---
echo ""
echo "---------------------------------------------------"
echo "Enabling maintenance mode..."
install_maintenance_page
$WP maintenance-mode activate 2>/dev/null
echo ""

# --- Core Update ---
if [ -n "$CORE_UPDATE" ]; then
  echo "[ Core Update ]"
  WP_VERSION_BEFORE=$($WP core version 2>/dev/null)
  $WP core update 2>&1
  CORE_RC=$?
  if [ $CORE_RC -eq 0 ]; then
    WP_VERSION_AFTER=$($WP core version 2>/dev/null)
    echo "✅ Core updated: $WP_VERSION_BEFORE → $WP_VERSION_AFTER"
    echo ""
    echo "   Running database upgrade..."
    hold_maintenance
    $WP core update-db 2>&1
  else
    ERRORS+=("Core update failed")
    echo "❌ Core update failed — skipping DB upgrade."
  fi
  echo ""
fi

# WP-CLI ends `plugin update --all` / `theme update --all` with a table whose
# rows end in a tab and Updated or Error. The summary counts those rows: the
# number available before the run says nothing about what landed (on a staging
# copy licence-bound packages fail, 2026-10-01).
count_status() { printf '%s\n' "$1" | grep -cP "\t$2\s*$"; }
failed_names() { printf '%s\n' "$1" | grep -P '\tError\s*$' | cut -f1 | paste -sd ' '; }
PLUGINS_DONE=0; THEMES_DONE=0

# --- Plugin Updates ---
if [ "$PLUGIN_UPDATES" -gt 0 ]; then
  echo "[ Plugin Updates ]"
  run_live $WP plugin update --all
  PLUGINS_DONE=$(count_status "$OUT" Updated)
  FAILED=$(failed_names "$OUT")
  if [ -n "$FAILED" ]; then ERRORS+=("Plugins not updated: $FAILED")
  elif [ $RC -ne 0 ]; then ERRORS+=("One or more plugin updates failed"); fi
  echo ""

  # Follow-ups the plugin update itself does not do.
  if $WP plugin is-active woocommerce 2>/dev/null \
     && [ "$($WP eval 'echo (int) \WC_Install::needs_db_update();' 2>/dev/null)" = 1 ]; then
    echo "[ WooCommerce database update ]"
    hold_maintenance
    $WP wc update 2>&1 || ERRORS+=("WooCommerce database update failed")
    echo ""
  fi
  if $WP plugin is-active redis-cache 2>/dev/null \
     && $WP redis status 2>/dev/null | grep -q "Drop-in is outdated"; then
    echo "[ Redis object-cache drop-in ]"
    $WP redis update-dropin 2>&1 || ERRORS+=("Redis drop-in update failed")
    echo ""
  fi
fi

# --- Theme Updates ---
if [ "$THEME_UPDATES" -gt 0 ]; then
  echo "[ Theme Updates ]"
  run_live $WP theme update --all
  THEMES_DONE=$(count_status "$OUT" Updated)
  FAILED=$(failed_names "$OUT")
  if [ -n "$FAILED" ]; then ERRORS+=("Themes not updated: $FAILED")
  elif [ $RC -ne 0 ]; then ERRORS+=("One or more theme updates failed"); fi
  echo ""

  # Themes that bundle plugins (Avada → Fusion Core / Fusion Builder) only
  # offer those updates once the theme itself is updated, so the plugin step
  # above never saw them (logscale + operaria, 2026-10-06). Look again.
  if [ "$THEMES_DONE" -gt 0 ]; then
    MORE=$($WP plugin list --update=available --format=count 2>/dev/null); MORE=${MORE:-0}
    if [ "$MORE" -gt 0 ]; then
      echo "[ Plugin Updates ] (offered after the theme update)"
      PLUGIN_UPDATES=$((PLUGIN_UPDATES + MORE))
      run_live $WP plugin update --all
      PLUGINS_DONE=$((PLUGINS_DONE + $(count_status "$OUT" Updated)))
      FAILED=$(failed_names "$OUT")
      if [ -n "$FAILED" ]; then ERRORS+=("Plugins not updated: $FAILED")
      elif [ $RC -ne 0 ]; then ERRORS+=("One or more plugin updates failed"); fi
      echo ""
    fi
  fi
fi

# --- Flush Cache ---
echo "Flushing cache..."
$WP cache flush --quiet 2>/dev/null
echo "✅ Cache flushed"
# Cached pages still name the CSS/JS the updated plugins just replaced
# (Elementor regenerates its post CSS), so WP Rocket's page cache and its
# minified files go too (alavusikkunat.fi 2026-10-01).
if $WP plugin is-active wp-rocket 2>/dev/null; then
  $WP eval 'rocket_clean_domain(); rocket_clean_minify(); echo "ok\n";' 2>/dev/null | grep -qx ok \
    && echo "✅ WP Rocket page cache and minified files cleared" \
    || echo "⚠️  WP Rocket cache clear failed"
fi

# --- Maintenance Mode Off (also called by trap) ---
echo ""
echo "Disabling maintenance mode..."
$WP maintenance-mode deactivate 2>/dev/null
remove_maintenance_page
rm -f "$UPDATE_LOG"
trap - EXIT

# --- Summary ---
echo ""
echo "======================================================"
echo "  Summary: $DOMAIN"
echo "======================================================"
[ -n "$CORE_UPDATE" ] && [ ! " ${ERRORS[*]} " =~ "Core" ] && echo "  ✅ Core updated to $($WP core version 2>/dev/null)"
mark() { [ "$1" -ge "$2" ] && echo "✅" || echo "⚠️ "; }
[ "$PLUGIN_UPDATES" -gt 0 ] && echo "  $(mark "$PLUGINS_DONE" "$PLUGIN_UPDATES") $PLUGINS_DONE of $PLUGIN_UPDATES plugin update(s) applied"
[ "$THEME_UPDATES" -gt 0 ] && echo "  $(mark "$THEMES_DONE" "$THEME_UPDATES") $THEMES_DONE of $THEME_UPDATES theme update(s) applied"
[ -n "$BACKUP_FILE" ] && echo "  💾 Backup: $BACKUP_FILE"

if [ ${#ERRORS[@]} -gt 0 ]; then
  echo ""
  echo "  ⚠️  Errors:"
  for err in "${ERRORS[@]}"; do
    echo "     - $err"
  done
fi
echo "======================================================"
