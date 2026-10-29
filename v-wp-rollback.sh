#!/bin/bash

if [ "$EUID" -ne 0 ]; then
  echo "❌ ERROR: Please run as root."
  exit 1
fi

export PATH=$PATH:/usr/local/hestia/bin

show_help() {
  cat <<'EOF'
USAGE: v-wp-rollback [OPTIONS]

Put a WordPress site back to the backup set v-wp-update made before its
updates (/root/pre-update/USER/DOMAIN_pre_update_TIMESTAMP/): the plugin,
theme and core files that run replaced, and the database.

Restoring the database discards everything written since the backup —
orders, comments, posts, users. --dry-run counts those first; --no-db puts
back only the files. What the rollback replaces is kept in the set under
rolled-back-from/ (files and a dump of the database as it was).

OPTIONS:
  --user=USER      HestiaCP user who owns the site
  --domain=DOMAIN  Domain to roll back
  --backup=NAME    Backup set folder name, or "latest" (default)
  --no-db          Restore the files only, leave the database as it is
  --dry-run        Show what would be restored and what the database restore
                   would discard, change nothing
  --list           List the backup sets for the domain
  -h, --help       Show this help

EXAMPLES:
  v-wp-rollback --user=shop --domain=shop.fi --dry-run
  v-wp-rollback --user=shop --domain=shop.fi --backup=shop.fi_pre_update_20261029_101500
EOF
}

HESTIA_USER=""
DOMAIN=""
BACKUP="latest"
NO_DB=false
DRY_RUN=false
LIST=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --user=*)   HESTIA_USER="${1#*=}" ;;
    --domain=*) DOMAIN="${1#*=}" ;;
    --backup=*) BACKUP="${1#*=}" ;;
    --no-db)    NO_DB=true ;;
    --dry-run)  DRY_RUN=true ;;
    --list)     LIST=true ;;
    -h|--help)  show_help; exit 0 ;;
    *) echo "❌ ERROR: Unknown option: $1"; exit 1 ;;
  esac
  shift
done

if [ -z "$HESTIA_USER" ] || [ -z "$DOMAIN" ]; then
  echo "❌ ERROR: --user and --domain are required."
  exit 1
fi
if ! [[ "$HESTIA_USER" =~ ^[a-z0-9_.-]+$ ]] || ! [[ "$DOMAIN" =~ ^[a-z0-9.-]+$ ]]; then
  echo "❌ ERROR: Invalid user or domain."
  exit 1
fi
DOMAIN="${DOMAIN#www.}"
WP_PATH="/home/$HESTIA_USER/web/$DOMAIN/public_html"
WP="sudo -u $HESTIA_USER wp --path=$WP_PATH"
BACKUP_DIR="/root/pre-update/$HESTIA_USER"

if [ ! -f "$WP_PATH/wp-config.php" ]; then
  echo "❌ ERROR: No WordPress installation found at $WP_PATH"
  exit 1
fi

# Sets only (folders with an items list); the plain .sql.gz dumps from before
# 2026-10-29 hold no files and are not rollback points.
list_sets() { ls -1dt "$BACKUP_DIR/${DOMAIN}_pre_update_"*/ 2>/dev/null | while read -r d; do [ -f "$d/items" ] && echo "${d%/}"; done; }

if [ "$LIST" = true ]; then
  echo "Backup sets for $DOMAIN (newest first):"
  list_sets | while read -r d; do
    n=$(grep -c . "$d/items")
    rb=""; [ -f "$d/rolled-back" ] && rb="  (rolled back $(cat "$d/rolled-back"))"
    echo "  $(basename "$d")  created $(cat "$d/created" 2>/dev/null)  $n package(s), $(du -sh "$d" | cut -f1)$rb"
  done
  exit 0
fi

if [ "$BACKUP" = latest ]; then
  SET=$(list_sets | head -n1)
else
  if ! [[ "$BACKUP" =~ ^${DOMAIN//./\\.}_pre_update_[0-9]{8}_[0-9]{6}$ ]]; then
    echo "❌ ERROR: --backup must be a set name of this domain (see --list)."
    exit 1
  fi
  SET="$BACKUP_DIR/$BACKUP"
fi
if [ -z "$SET" ] || [ ! -f "$SET/items" ] || [ ! -s "$SET/db.sql.gz" ]; then
  echo "❌ ERROR: No usable backup set found${BACKUP:+ ($BACKUP)}. See --list."
  exit 1
fi
CREATED=$(cat "$SET/created")
if ! [[ "$CREATED" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]]; then
  echo "❌ ERROR: The set has no valid creation time."
  exit 1
fi

echo ""
echo "======================================================"
[ "$DRY_RUN" = true ] && echo "  Rollback (Dry Run): $DOMAIN" || echo "  Rollback: $DOMAIN"
echo "======================================================"
echo "Backup set: $(basename "$SET")"
echo "Taken:      $CREATED (UTC)"
[ -f "$SET/rolled-back" ] && echo "⚠️  This set was already rolled back to on $(cat "$SET/rolled-back")."

# What the files part puts back, with the versions now and then.
version_in() { awk -F, -v n="$2" '$1 == n { print $2; exit }' "$1" 2>/dev/null; }
NOW_PLUGINS=$(mktemp); NOW_THEMES=$(mktemp)
$WP plugin list --fields=name,version,status --format=csv > "$NOW_PLUGINS" 2>/dev/null
$WP theme list --fields=name,version,status --format=csv > "$NOW_THEMES" 2>/dev/null
# A package whose version did not change (its update failed, or the set
# copied it for a step that stopped) is left as it is.
echo ""
echo "Files:"
RESTORE=()
while read -r kind slug; do
  case "$kind" in
    core)   now=$($WP core version 2>/dev/null); was=$(cat "$SET/core-version" 2>/dev/null); label="WordPress" ;;
    plugin) now=$(version_in "$NOW_PLUGINS" "$slug"); was=$(version_in "$SET/plugins.csv" "$slug"); label=$slug ;;
    theme)  now=$(version_in "$NOW_THEMES" "$slug"); was=$(version_in "$SET/themes.csv" "$slug"); label=$slug ;;
    *) continue ;;
  esac
  if [ -n "$now" ] && [ "$now" = "$was" ]; then
    printf "  %-8s %s %s (unchanged, left as it is)\n" "$kind" "$label" "$now"
  else
    printf "  %-8s %s %s → %s\n" "$kind" "$label" "${now:-?}" "${was:-?}"
    RESTORE+=("$kind ${slug:-core}")
  fi
done < "$SET/items"
[ ${#RESTORE[@]} -eq 0 ] && echo "  (nothing to put back)"
rm -f "$NOW_PLUGINS" "$NOW_THEMES"

# What a database restore would throw away: rows written since the backup.
echo ""
if [ "$NO_DB" = true ]; then
  echo "Database: left as it is (--no-db)."
else
  echo "Database: restored to $CREATED. Written since then, and lost by the restore:"
  $WP eval '
    global $wpdb;
    $since = gmdate("Y-m-d H:i:s", strtotime("'"$CREATED"'"));
    $orders = 0;
    $hpos = $wpdb->prefix . "wc_orders";
    if ($wpdb->get_var($wpdb->prepare("SHOW TABLES LIKE %s", $hpos)) === $hpos) {
      $orders = (int) $wpdb->get_var($wpdb->prepare("SELECT COUNT(*) FROM $hpos WHERE type = %s AND date_created_gmt > %s", "shop_order", $since));
    } else {
      $orders = (int) $wpdb->get_var($wpdb->prepare("SELECT COUNT(*) FROM $wpdb->posts WHERE post_type = %s AND post_date_gmt > %s", "shop_order", $since));
    }
    $comments = (int) $wpdb->get_var($wpdb->prepare("SELECT COUNT(*) FROM $wpdb->comments WHERE comment_date_gmt > %s AND comment_type IN (\"\", \"comment\", \"review\")", $since));
    $posts = (int) $wpdb->get_var($wpdb->prepare("SELECT COUNT(*) FROM $wpdb->posts WHERE post_modified_gmt > %s AND post_type NOT IN (\"revision\", \"shop_order\", \"shop_order_placehold\") AND post_status NOT IN (\"auto-draft\", \"inherit\")", $since));
    $users = (int) $wpdb->get_var($wpdb->prepare("SELECT COUNT(*) FROM $wpdb->users WHERE user_registered > %s", $since));
    printf("  orders %d · comments/reviews %d · posts/pages/products changed %d · new users %d\n", $orders, $comments, $posts, $users);
    if ($orders > 0) echo "  ⚠️  Orders since the backup would be lost. Consider --no-db.\n";
  ' 2>/dev/null || echo "  (could not count)"
fi

if [ "$DRY_RUN" = true ]; then
  echo ""
  echo "Run without --dry-run to roll back."
  exit 0
fi

# --- Roll back ---
STAMP=$(date +%Y%m%d_%H%M%S)
ASIDE="$SET/rolled-back-from/$STAMP"
mkdir -p "$ASIDE"
ERRORS=()

cleanup() { $WP maintenance-mode deactivate &>/dev/null; }
trap cleanup EXIT

# The current database is saved with the site still open; the site closes
# only for the copying and the import.
echo ""
if [ "$NO_DB" = false ]; then
  echo "Saving the current database to $ASIDE/db.sql.gz ..."
  $WP db export - 2>/dev/null | gzip > "$ASIDE/db.sql.gz"
  if [ "${PIPESTATUS[0]}" -ne 0 ] || [ ! -s "$ASIDE/db.sql.gz" ]; then
    echo "❌ ERROR: Could not save the current database. Nothing changed."
    exit 1
  fi
fi

echo "Closing the site (maintenance mode)..."
CLOSED_AT=$(date +%s.%N)
$WP maintenance-mode activate &>/dev/null

# A package back: the current folder (or file) moves aside, the backup's copy
# goes in its place. Same owner and modes as when it was copied.
restore_path() { # backup-path target-path aside-dir
  local from=$1 to=$2 aside=$3
  mkdir -p "$aside"
  if [ -e "$to" ]; then mv "$to" "$aside/" || return 1; fi
  cp -a "$from" "$to"
}

echo "Restoring files..."
while read -r kind slug; do
  case "$kind" in
    plugin|theme)
      base="$WP_PATH/wp-content/${kind}s"
      if [ -e "$SET/files/${kind}s/$slug" ]; then src="$SET/files/${kind}s/$slug"; dst="$base/$slug"
      else src="$SET/files/${kind}s/$slug.php"; dst="$base/$slug.php"; fi
      if restore_path "$src" "$dst" "$ASIDE/${kind}s"; then echo "  ✅ $kind $slug"
      else echo "  ❌ $kind $slug"; ERRORS+=("$kind $slug not restored"); fi
      ;;
    core)
      ok=true
      for d in wp-admin wp-includes; do restore_path "$SET/files/core/$d" "$WP_PATH/$d" "$ASIDE/core" || ok=false; done
      for f in "$SET/files/core/"*.php; do
        cp -a "$WP_PATH/$(basename "$f")" "$ASIDE/core/" 2>/dev/null
        cp -a "$f" "$WP_PATH/" || ok=false
      done
      if $ok; then echo "  ✅ core $(cat "$SET/core-version" 2>/dev/null)"; else echo "  ❌ core"; ERRORS+=("core not fully restored"); fi
      ;;
  esac
done < <(printf "%s\n" "${RESTORE[@]}")

if [ "$NO_DB" = false ]; then
  echo "Restoring the database..."
  if gunzip -c "$SET/db.sql.gz" | $WP db import - &>/dev/null; then
    echo "  ✅ database as of $CREATED"
  else
    ERRORS+=("database import failed — the pre-rollback dump is $ASIDE/db.sql.gz")
    echo "  ❌ database import failed"
  fi
fi

echo "Flushing caches..."
$WP cache flush --quiet 2>/dev/null
if $WP plugin is-active wp-rocket 2>/dev/null; then
  $WP eval 'rocket_clean_domain(); rocket_clean_minify();' &>/dev/null
fi

$WP maintenance-mode deactivate &>/dev/null
CLOSED_S=$(awk -v a="$CLOSED_AT" -v b="$(date +%s.%N)" "BEGIN { printf \"%.1f\", b - a }")
trap - EXIT
date -u +%Y-%m-%dT%H:%M:%SZ > "$SET/rolled-back"

CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 "https://$DOMAIN/" 2>/dev/null)

echo ""
echo "======================================================"
echo "  Summary: $DOMAIN"
echo "======================================================"
echo "  ↩️  Rolled back to $(basename "$SET") (${#RESTORE[@]} package(s)$([ "$NO_DB" = false ] && echo ", database"))"
echo "  🔒 Site closed: ${CLOSED_S} s (maintenance mode)"
echo "  📦 What was replaced: $ASIDE"
echo "  🌐 Front page: HTTP ${CODE:-?}"
if [ ${#ERRORS[@]} -gt 0 ]; then
  echo ""
  echo "  ⚠️  Errors:"
  for err in "${ERRORS[@]}"; do echo "     - $err"; done
  echo "======================================================"
  exit 1
fi
echo "======================================================"
