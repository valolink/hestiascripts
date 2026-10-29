#!/bin/bash

# Standalone mode (--path=…): a site on a host that is not ours (cPanel,
# CloudLinux …), run as its own user over SSH — no root, no Hestia.
if [ "$EUID" -ne 0 ] && [[ " $* " != *" --path="* ]]; then
  echo "❌ ERROR: Please run as root (or as the site user with --path)."
  exit 1
fi

export PATH=$PATH:/usr/local/hestia/bin

show_help() {
  cat <<'EOF2'
USAGE: v-wp-update [OPTIONS]

Run all available WordPress updates (core, plugins, themes).

Unless --skip-backup is set, a backup set is made first under
/root/pre-update/USER/DOMAIN_pre_update_TIMESTAMP/: the database dump, a copy
of every plugin, theme and core file the run is about to replace (taken just
before each step replaces it) and the versions before. v-wp-rollback restores
a set. The newest 3 sets per domain are kept.

Update packages are downloaded while the site is still open; the site is
closed (maintenance mode) only while WordPress unpacks and copies them and
while a database migration runs. The summary reports how long it was closed.

OPTIONS:
  --user=USER      HestiaCP user who owns the site
  --domain=DOMAIN  Domain to update
  --exclude=LIST   Comma-separated plugin / theme slugs to leave as they are;
                   "core" leaves WordPress core
  --skip-backup    Skip the backup set (staging copies)
  --path=DIR       Standalone: the WordPress directory of a site on a host
                   that is not ours, run as the site user (no root, no
                   Hestia); backup sets go to ~/.vl-pre-update/ (newest 2)
  --wp-cli=CMD     Standalone: the WP-CLI command, e.g. on CloudLinux
                   "/opt/alt/php84/usr/bin/php /usr/local/bin/wp" (default wp)
  --dry-run        Show what would be updated without applying any changes
  -h, --help       Show this help

EXAMPLES:
  # Check what needs updating without touching anything
  v-wp-update --user=admin --domain=mysite.fi --dry-run

  # Run all updates except two plugins
  v-wp-update --user=admin --domain=mysite.fi --exclude=js_composer,wp-all-import-pro
EOF2
}

# --- Flag Parsing ---
HESTIA_USER=""
DOMAIN=""
SKIP_BACKUP=false
DRY_RUN=false
EXCLUDE=""
STANDALONE_PATH=""
WP_CLI="wp"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --user=*)      HESTIA_USER="${1#*=}" ;;
    --domain=*)    DOMAIN="${1#*=}" ;;
    --exclude=*)   EXCLUDE="${1#*=}" ;;
    --path=*)      STANDALONE_PATH="${1#*=}" ;;
    --wp-cli=*)    WP_CLI="${1#*=}" ;;
    --skip-backup) SKIP_BACKUP=true ;;
    --dry-run)     DRY_RUN=true ;;
    -h|--help)     show_help; exit 0 ;;
    *) echo "❌ ERROR: Unknown option: $1"; exit 1 ;;
  esac
  shift
done

# Slugs only: they reach WP-CLI's --exclude and a PHP argument.
EXCLUDE="${EXCLUDE// /}"
if [ -n "$EXCLUDE" ] && ! [[ "$EXCLUDE" =~ ^[A-Za-z0-9._-]+(,[A-Za-z0-9._-]+)*$ ]]; then
  echo "❌ ERROR: --exclude takes comma-separated slugs."
  exit 1
fi
is_excluded() { [[ ",$EXCLUDE," == *",$1,"* ]]; }

# --- Interactive prompts for missing values ---
if [ -n "$STANDALONE_PATH" ]; then
  [ -n "$DOMAIN" ] || { echo "❌ ERROR: --path needs --domain (it names the backup sets)."; exit 1; }
  HESTIA_USER=$(id -un)
elif [ -z "$HESTIA_USER" ]; then
  mapfile -t USERS < <(v-list-users plain | cut -f1)
  PS3="Select user: "
  select HESTIA_USER in "${USERS[@]}"; do
    [ -n "$HESTIA_USER" ] && break
    echo "Invalid selection."
  done
fi

if [ -z "$DOMAIN" ] && [ -z "$STANDALONE_PATH" ]; then
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
if [ -n "$STANDALONE_PATH" ]; then
  WP_PATH="${STANDALONE_PATH%/}"
  WP="$WP_CLI --path=$WP_PATH"
  own() { :; } # everything is the site user's already
else
  WP_PATH="/home/$HESTIA_USER/web/$DOMAIN/public_html"
  WP="sudo -u $HESTIA_USER wp --path=$WP_PATH"
  own() { chown "$HESTIA_USER:$HESTIA_USER" "$@"; }
fi

if [ ! -f "$WP_PATH/wp-config.php" ]; then
  echo "❌ ERROR: No WordPress installation found at $WP_PATH"
  exit 1
fi

if ! command -v ${WP_CLI%% *} &>/dev/null; then
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

# What is on offer, minus --exclude (slug lines only: a LiteSpeed host printed
# "Success: Purged all caches" into the list, 2026-10-29). WP-CLI's names are the plugin / theme
# folders, the same slugs --exclude takes; the update steps below name the
# packages explicitly, so an excluded one is never touched.
CORE_UPDATE=$($WP core check-update --format=csv --fields=version 2>/dev/null | tail -n +2 | head -n1)
SKIPPED=()
if [ -n "$CORE_UPDATE" ] && { is_excluded core || is_excluded wordpress; }; then
  SKIPPED+=("core"); CORE_UPDATE=""
fi
PLUGIN_LIST=(); THEME_LIST=()
for s in $($WP plugin list --update=available --field=name 2>/dev/null | grep -E "^[A-Za-z0-9._-]+$"); do
  if is_excluded "$s"; then SKIPPED+=("$s"); else PLUGIN_LIST+=("$s"); fi
done
for s in $($WP theme list --update=available --field=name 2>/dev/null | grep -E "^[A-Za-z0-9._-]+$"); do
  if is_excluded "$s"; then SKIPPED+=("$s"); else THEME_LIST+=("$s"); fi
done
PLUGIN_UPDATES=${#PLUGIN_LIST[@]}
THEME_UPDATES=${#THEME_LIST[@]}

[ ${#SKIPPED[@]} -gt 0 ] && echo "  ⏭️  Skipped (excluded): ${SKIPPED[*]}"
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
    $WP plugin list --update=available --fields=name,version,update_version --format=table 2>/dev/null
  fi
  if [ "$THEME_UPDATES" -gt 0 ]; then
    echo ""
    echo "Themes with updates:"
    $WP theme list --update=available --fields=name,version,update_version --format=table 2>/dev/null
  fi
  echo ""
  echo "Run without --dry-run to apply updates."
  exit 0
fi

# Licence keys and download tokens ride in the package URLs that WP-CLI
# prints ("Downloading update from …?code=…", markup.fi's license_key=…), and
# EngineLink keeps this output in the run log. Mask the values.
mask() { sed -u -E 's/([?&](code|license_key|licence_key|license|licence|purchase_code|api_key|key|token)=)[^&[:space:]"'\'']+/\1***/g'; }

# --- Work dir (prefetched packages, helpers run as the site user) ---
WORK=$(mktemp -d /tmp/vl-wp-update.XXXXXX)
own "$WORK"
chmod 700 "$WORK"
UPDATE_LOG="$WORK/step.log"
WATCH_PID=""

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
  own "$MAINT_PAGE"
  chmod 644 "$MAINT_PAGE"
}
remove_maintenance_page() {
  grep -q "$MAINT_MARK" "$MAINT_PAGE" 2>/dev/null && rm -f "$MAINT_PAGE"
}

# Always: site open, our page gone, watcher stopped, work dir removed.
cleanup() {
  $WP maintenance-mode deactivate &>/dev/null
  remove_maintenance_page
  rm -f "$WORK/watching"
  [ -n "$WATCH_PID" ] && wait "$WATCH_PID" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

# --- Backup set ---
# Not /home/USER/backup: the nightly v-backup-user-config starts with
# `rm -fr /home/$user/backup/`, so a dump there was gone by the next morning.
# Root-only, outside the web tree, untouched by Hestia. The database first;
# the files each step replaces are copied just before that step (below), so
# the set holds exactly what a rollback has to put back.
SET=""
if [ "$SKIP_BACKUP" = false ]; then
  echo ""
  echo "---------------------------------------------------"
  if [ -n "$STANDALONE_PATH" ]; then
    # Hosting accounts have quotas: keep two sets, outside the web root.
    BACKUP_DIR="$HOME/.vl-pre-update"; KEEP_SETS=2
  else
    BACKUP_DIR="/root/pre-update/$HESTIA_USER"; KEEP_SETS=3
    mkdir -p /root/pre-update; chmod 700 /root/pre-update
  fi
  TIMESTAMP=$(date +%Y%m%d_%H%M%S)
  SET="$BACKUP_DIR/${DOMAIN}_pre_update_${TIMESTAMP}"
  mkdir -p "$SET"
  chmod 700 "$BACKUP_DIR" "$SET"

  echo "Backing up the database to $SET/db.sql.gz ..."
  $WP db export - 2>/dev/null | gzip > "$SET/db.sql.gz"
  if [ "${PIPESTATUS[0]}" -eq 0 ] && [ -s "$SET/db.sql.gz" ]; then
    date -u +%Y-%m-%dT%H:%M:%SZ > "$SET/created"
    $WP core version > "$SET/core-version" 2>/dev/null
    $WP plugin list --fields=name,version,status --format=csv > "$SET/plugins.csv" 2>/dev/null
    $WP theme list --fields=name,version,status --format=csv > "$SET/themes.csv" 2>/dev/null
    : > "$SET/items"
    echo "✅ Database backed up ($(du -sh "$SET/db.sql.gz" | cut -f1))"
    # Newest 3 per domain (a set with WooCommerce runs to ~300 MB): sets,
    # and the plain dumps from before 2026-10-29.
    ls -1dt "$BACKUP_DIR/${DOMAIN}_pre_update_"* 2>/dev/null | tail -n +$((KEEP_SETS + 1)) | xargs -r rm -rf
  else
    echo "❌ ERROR: Database backup failed. Aborting."
    echo "   Use --skip-backup to proceed without a backup."
    rm -rf "$SET"
    exit 1
  fi
fi

# Copies what the next step replaces, just before it does: plugins and themes
# by folder (a single-file plugin by its file), core as wp-admin, wp-includes
# and the root PHP files except wp-config.php. Listed in $SET/items for
# v-wp-rollback. A failed copy stops the run before that step changes anything.
backup_packages() { # plugin|theme slug...
  [ -n "$SET" ] || return 0
  local kind=$1 s base="$WP_PATH/wp-content/${1}s"
  shift
  mkdir -p "$SET/files/${kind}s"
  for s in "$@"; do
    grep -qx "$kind $s" "$SET/items" && continue
    if [ -d "$base/$s" ]; then cp -a "$base/$s" "$SET/files/${kind}s/" || return 1
    elif [ -f "$base/$s.php" ]; then cp -a "$base/$s.php" "$SET/files/${kind}s/" || return 1
    else continue; fi
    echo "$kind $s" >> "$SET/items"
  done
}
backup_core() {
  [ -n "$SET" ] || return 0
  grep -qx "core" "$SET/items" && return 0
  mkdir -p "$SET/files/core"
  cp -a "$WP_PATH/wp-admin" "$WP_PATH/wp-includes" "$SET/files/core/" || return 1
  local f
  for f in "$WP_PATH"/*.php; do
    [ "$(basename "$f")" = wp-config.php ] && continue
    cp -a "$f" "$SET/files/core/" || return 1
  done
  echo "core" >> "$SET/items"
}
backup_or_stop() {
  "$@" && return 0
  echo "❌ ERROR: Could not copy the files this step replaces into $SET. Stopping before it."
  ERRORS+=("Backup copy failed; run stopped")
  exit 1
}

# --- Prefetch ---
# WordPress's bulk upgrader turns maintenance mode on, then downloads, unpacks
# and installs each package in turn, so every download (licence servers can
# take seconds each) happened with the site closed. prefetch.php downloads
# the packages first with the site open; use-prefetched.php hands them to the
# upgrader through upgrader_pre_download. A package that will not prefetch
# (or a premium plugin that downloads in its own hook) downloads as before.
cat > "$WORK/prefetch.php" <<'PHP'
<?php
// v-wp-update: download update packages while the site is still open.
require_once ABSPATH . 'wp-admin/includes/file.php';
list($kind, $exclude, $work, $wanted) = array_pad($args, 4, '');
$skip = array_filter(explode(',', $exclude));
$want = $wanted === 'none' ? [] : array_filter(explode(',', $wanted));
$map_file = $work . '/map.json';
$map = is_readable($map_file) ? (json_decode((string) file_get_contents($map_file), true) ?: []) : [];
$read = function () use ($kind): array {
    $todo = [];
    if ($kind === 'plugins') {
        $t = get_site_transient('update_plugins');
        foreach ((array) ($t->response ?? []) as $file => $u) {
            $slug = strpos($file, '/') !== false ? dirname($file) : basename($file, '.php');
            $todo[$slug] = is_object($u) ? (string) ($u->package ?? '') : (string) ($u['package'] ?? '');
        }
    } else {
        $t = get_site_transient('update_themes');
        foreach ((array) ($t->response ?? []) as $slug => $u) {
            $todo[$slug] = is_array($u) ? (string) ($u['package'] ?? '') : (string) ($u->package ?? '');
        }
    }
    return $todo;
};
$todo = $read();
// The stored update data can lack packages WP-CLI's own listing showed
// (kovanen.com 2026-10-29: 1 of 15): refresh it once, as WordPress does
// after installing a plugin, and read again.
if ($want && array_diff($want, array_keys($todo))) {
    if ($kind === 'plugins') { wp_clean_plugins_cache(true); wp_update_plugins(); }
    else { delete_site_transient('update_themes'); wp_update_themes(); }
    $todo = $read();
}
if ($want) {
    $todo = array_intersect_key($todo, array_flip($want));
}
foreach ($todo as $slug => $package) {
    if (in_array($slug, $skip, true) || isset($map[$package]) || !preg_match('#^https?://#i', $package)) continue;
    $t0 = microtime(true);
    $tmp = download_url($package, 300);
    if (is_wp_error($tmp)) {
        WP_CLI::line("   $slug: not prefetched (" . $tmp->get_error_message() . '), the update downloads it');
        continue;
    }
    $fh = fopen($tmp, 'rb');
    $magic = $fh ? fread($fh, 4) : '';
    if ($fh) fclose($fh);
    $dest = $work . '/' . $kind . '-' . $slug . '.zip';
    $moved = $magic === "PK\x03\x04" && (@rename($tmp, $dest) || (copy($tmp, $dest) && @unlink($tmp)));
    if (!$moved) {
        @unlink($tmp);
        WP_CLI::line("   $slug: not prefetched (not a zip), the update downloads it");
        continue;
    }
    $map[$package] = $dest;
    WP_CLI::line(sprintf('   %s: %s in %.1f s', $slug, size_format(filesize($dest)), microtime(true) - $t0));
}
file_put_contents($map_file, json_encode($map));
PHP
cat > "$WORK/use-prefetched.php" <<'PHP'
<?php
// v-wp-update: give the upgrader the package prefetch.php downloaded before
// the site closed. Anything else downloads as usual.
WP_CLI::add_wp_hook('upgrader_pre_download', function ($reply, $package) {
    static $map = null;
    if (false !== $reply || !is_string($package)) return $reply;
    if (null === $map) {
        $f = __DIR__ . '/map.json';
        $map = is_readable($f) ? (json_decode((string) file_get_contents($f), true) ?: []) : [];
    }
    return (isset($map[$package]) && is_readable($map[$package])) ? $map[$package] : $reply;
}, 99, 2);
PHP
own "$WORK"/*.php
# Only prefetch.php's own lines (and WP-CLI errors): some plugins print
# markup whenever WordPress loads (soutuveneet.fi, 2026-10-29).
prefetch() { # plugins|themes slug...
  local kind=$1; shift
  local wanted; wanted=$(IFS=,; echo "$*")
  echo "Downloading $kind before the site closes..."
  $WP eval-file "$WORK/prefetch.php" "$kind" "${EXCLUDE:-none}" "$WORK" "${wanted:-none}" 2>&1 | mask | grep -E "^(   [^ ]|Error:)"
}
USE_PREFETCHED="--require=$WORK/use-prefetched.php"

# Runs one WP-CLI update with its output on screen as it happens — EngineLink
# streams this script over SSE, and capturing into a variable held the whole
# plugin step back until it ended (~50 s on alavusikkunat.fi) — while keeping
# a copy in $OUT for the counts. $RC is WP-CLI's exit code.
run_live() {
  "$@" 2>&1 | mask | tee "$UPDATE_LOG"
  RC=${PIPESTATUS[0]}
  OUT=$(<"$UPDATE_LOG")
}

ERRORS=()

# --- Maintenance mode: only when needed ---
# The upgraders close the site themselves (.maintenance) while they unpack
# and copy, and open it when done. We close it on top of that only for
# database migrations (core update-db, a pending WooCommerce DB update), which
# must not run under traffic, and open it right after. The watcher counts the
# time .maintenance exists, so the summary says how long the site was closed.
hold_maintenance() { $WP maintenance-mode activate &>/dev/null; }
release_maintenance() { $WP maintenance-mode deactivate &>/dev/null; }
install_maintenance_page
touch "$WORK/watching"
( n=0
  while [ -e "$WORK/watching" ]; do
    [ -e "$WP_PATH/.maintenance" ] && n=$((n + 1)) && echo "$n" > "$WORK/closed-ticks"
    sleep 0.2
  done ) &
WATCH_PID=$!
echo ""

# --- Core Update ---
# Core's upgrader downloads and unpacks before it closes the site; it is
# closed only while the files are copied.
if [ -n "$CORE_UPDATE" ]; then
  echo "[ Core Update ]"
  backup_or_stop backup_core
  WP_VERSION_BEFORE=$($WP core version 2>/dev/null)
  $WP core update 2>&1 | mask
  CORE_RC=${PIPESTATUS[0]}
  if [ "$CORE_RC" -eq 0 ]; then
    WP_VERSION_AFTER=$($WP core version 2>/dev/null)
    echo "✅ Core updated: $WP_VERSION_BEFORE → $WP_VERSION_AFTER"
    if [ "$($WP eval 'global $wp_db_version; echo (int) ((int) get_option("db_version") !== (int) $wp_db_version);' 2>/dev/null)" = 1 ]; then
      echo ""
      echo "   Running database upgrade (site closed)..."
      hold_maintenance
      $WP core update-db 2>&1
      release_maintenance
    fi
  else
    ERRORS+=("Core update failed")
    echo "❌ Core update failed — skipping DB upgrade."
  fi
  echo ""
fi

# WP-CLI ends `plugin update` / `theme update` with a table whose rows end in
# a tab and Updated or Error. The summary counts those rows: the number
# available before the run says nothing about what landed (on a staging copy
# licence-bound packages fail, 2026-10-01).
count_status() { printf '%s\n' "$1" | grep -cP "\t$2\s*$"; }
failed_names() { printf '%s\n' "$1" | grep -P '\tError\s*$' | cut -f1 | paste -sd ' '; }
PLUGINS_DONE=0; THEMES_DONE=0

update_plugins() { # slug...
  prefetch plugins "$@"
  backup_or_stop backup_packages plugin "$@"
  run_live $WP plugin update "$@" "$USE_PREFETCHED"
  PLUGINS_DONE=$((PLUGINS_DONE + $(count_status "$OUT" Updated)))
  FAILED=$(failed_names "$OUT")
  if [ -n "$FAILED" ]; then ERRORS+=("Plugins not updated: $FAILED")
  elif [ $RC -ne 0 ]; then ERRORS+=("One or more plugin updates failed"); fi
  echo ""
}

# --- Plugin Updates ---
if [ "$PLUGIN_UPDATES" -gt 0 ]; then
  echo "[ Plugin Updates ]"
  update_plugins "${PLUGIN_LIST[@]}"

  # Follow-ups the plugin update itself does not do.
  if $WP plugin is-active woocommerce 2>/dev/null \
     && [ "$($WP eval 'echo (int) \WC_Install::needs_db_update();' 2>/dev/null)" = 1 ]; then
    echo "[ WooCommerce database update ] (site closed)"
    hold_maintenance
    $WP wc update 2>&1 || ERRORS+=("WooCommerce database update failed")
    release_maintenance
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
  prefetch themes "${THEME_LIST[@]}"
  backup_or_stop backup_packages theme "${THEME_LIST[@]}"
  run_live $WP theme update "${THEME_LIST[@]}" "$USE_PREFETCHED"
  THEMES_DONE=$(count_status "$OUT" Updated)
  FAILED=$(failed_names "$OUT")
  if [ -n "$FAILED" ]; then ERRORS+=("Themes not updated: $FAILED")
  elif [ $RC -ne 0 ]; then ERRORS+=("One or more theme updates failed"); fi
  echo ""

  # Themes that bundle plugins (Avada → Fusion Core / Fusion Builder) only
  # offer those updates once the theme itself is updated, so the plugin step
  # above never saw them (logscale + operaria, 2026-10-06). Look again —
  # for the new ones only: a plugin the first pass already tried (a refused
  # licence) would only fail again, with the site closed (soutuveneet.fi
  # 2026-10-29: ACF Pro + Complianz twice).
  if [ "$THEMES_DONE" -gt 0 ]; then
    MORE_LIST=()
    for s in $($WP plugin list --update=available --field=name 2>/dev/null | grep -E "^[A-Za-z0-9._-]+$"); do
      [[ " ${PLUGIN_LIST[*]} " == *" $s "* ]] && continue
      if is_excluded "$s"; then [[ " ${SKIPPED[*]} " == *" $s "* ]] || SKIPPED+=("$s"); else MORE_LIST+=("$s"); fi
    done
    if [ ${#MORE_LIST[@]} -gt 0 ]; then
      echo "[ Plugin Updates ] (offered after the theme update)"
      PLUGIN_UPDATES=$((PLUGIN_UPDATES + ${#MORE_LIST[@]}))
      update_plugins "${MORE_LIST[@]}"
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
# LiteSpeed hosts (Hostingpalvelu) cache pages in the server; the plugin purges them.
if $WP plugin is-active litespeed-cache 2>/dev/null; then
  $WP litespeed-purge all &>/dev/null && echo "✅ LiteSpeed cache purged" || echo "⚠️  LiteSpeed cache purge failed"
fi

# --- Open (normally already open; the trap does the same on any exit) ---
release_maintenance
remove_maintenance_page
rm -f "$WORK/watching"
wait "$WATCH_PID" 2>/dev/null
CLOSED_TICKS=$(cat "$WORK/closed-ticks" 2>/dev/null || echo 0)
CLOSED_S=$(awk -v t="$CLOSED_TICKS" 'BEGIN { printf "%.1f", t / 5 }')
WATCH_PID=""

# --- Summary ---
echo ""
echo "======================================================"
echo "  Summary: $DOMAIN"
echo "======================================================"
[ -n "$CORE_UPDATE" ] && [[ " ${ERRORS[*]} " != *" Core"* ]] && echo "  ✅ Core updated to $($WP core version 2>/dev/null)"
mark() { [ "$1" -ge "$2" ] && echo "✅" || echo "⚠️ "; }
[ "$PLUGIN_UPDATES" -gt 0 ] && echo "  $(mark "$PLUGINS_DONE" "$PLUGIN_UPDATES") $PLUGINS_DONE of $PLUGIN_UPDATES plugin update(s) applied"
[ "$THEME_UPDATES" -gt 0 ] && echo "  $(mark "$THEMES_DONE" "$THEME_UPDATES") $THEMES_DONE of $THEME_UPDATES theme update(s) applied"
[ ${#SKIPPED[@]} -gt 0 ] && echo "  ⏭️  Skipped (excluded): ${SKIPPED[*]}"
echo "  🔒 Site closed: ${CLOSED_S} s (maintenance mode)"
if [ -n "$SET" ]; then
  echo "  💾 Backup: $SET (database + $(grep -c . "$SET/items") package(s), $(du -sh "$SET" | cut -f1))"
  if [ -n "$STANDALONE_PATH" ]; then
    echo "     Roll back: v-wp-rollback --path=$WP_PATH --wp-cli=\"$WP_CLI\" --domain=$DOMAIN --backup=$(basename "$SET")"
  else
    echo "     Roll back: v-wp-rollback --user=$HESTIA_USER --domain=$DOMAIN --backup=$(basename "$SET")"
  fi
fi

if [ ${#ERRORS[@]} -gt 0 ]; then
  echo ""
  echo "  ⚠️  Errors:"
  for err in "${ERRORS[@]}"; do
    echo "     - $err"
  done
fi
echo "======================================================"
