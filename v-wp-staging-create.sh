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

# The box's copy registry (v-wp-copies reads it; EngineLink lists copies from
# it). One root-only KEY=value file per copy, named after the copy's domain.
copy_registry_file() {
  echo "/root/.hestia-site-copies/${1}.conf"
}

register_copy() { # KIND COPY_DOMAIN COPY_USER SRC_DOMAIN SRC_USER
  local f; f=$(copy_registry_file "$2")
  mkdir -p "$(dirname "$f")" && chmod 700 "$(dirname "$f")"
  printf 'KIND=%s\nDOMAIN=%s\nUSER=%s\nSOURCE_DOMAIN=%s\nSOURCE_USER=%s\nCREATED=%s\nSCRIPT=%s\n' \
    "$1" "$2" "$3" "$4" "$5" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$(basename "$0")" > "$f"
  chmod 600 "$f"
}

# HTTP basic auth on staging (docs/security-hardening.md §7): scanners get a
# 401 instead of a copy of a live site. The credentials live root-only in
# /root/.hestia-staging-auth/<domain> ("user password") and are reused on
# every refresh, so whoever already has them keeps access. Never printed.
staging_auth_store() {
  echo "/root/.hestia-staging-auth/${1}"
}

ensure_staging_httpauth() { # DEST_USER DOMAIN
  local user="$1" domain="$2" store auth_user pass
  store=$(staging_auth_store "$domain")
  mkdir -p "$(dirname "$store")" && chmod 700 "$(dirname "$store")"
  if [ -s "$store" ]; then
    read -r auth_user pass < "$store"
  else
    auth_user="staging"
    pass=$(openssl rand -base64 30 | tr -dc 'A-Za-z0-9' | head -c 20)
    (umask 077 && echo "$auth_user $pass" > "$store")
  fi
  chmod 600 "$store"
  # Hestia keeps the domain's auth users in web.conf (AUTH_USER='a:b').
  if grep "DOMAIN='$domain'" "/usr/local/hestia/data/users/$user/web.conf" 2>/dev/null \
      | grep -oP "AUTH_USER='\K[^']*" | tr ':' '\n' | grep -qx "$auth_user"; then
    v-change-web-domain-httpauth "$user" "$domain" "$auth_user" "$pass" > /dev/null 2>&1
  else
    v-add-web-domain-httpauth "$user" "$domain" "$auth_user" "$pass"
  fi
}

# Reload every installed PHP-FPM service so workers drop their cached wp-config.php
# and pick up the new WP_REDIS_PREFIX / DB creds. Reload is graceful.
# Copies share the box with the live site and must yield to it (2026-09-30, after
# kuumalahde's dev copies ran live-sized PHP pools, WP-cron and day-long Redis
# keys beside production): the copy's PHP pool goes on the PHP-FPM profile
# (default staging: 4 ondemand workers at nice 10, PHP memory capped at 512M),
# its Redis keys expire within an hour (Redis cannot cap memory per database, and
# its LRU evicts live's keys for the copy's), and its database user gets at most
# 10 connections. The profile template comes from `hs op fpm-profile`; without it
# the pool is left as it is and the command to create it is printed.
apply_copy_limits() {  # user domain dir db_user profile
  local user=$1 domain=$2 dir=$3 db_user=$4 profile=$5 pool ver tpl host
  echo "       Copy limits: PHP profile '$profile', Redis keys ≤ 1 h, ≤ 10 DB connections..."
  if [ "$profile" != "none" ]; then
    pool=$(ls /etc/php/*/fpm/pool.d/"$domain".conf 2>/dev/null | head -1)
    ver=$(echo "$pool" | cut -d/ -f4)
    tpl="$profile-PHP-${ver//./_}"
    if [ -z "$ver" ]; then
      echo "       ⚠️  No PHP-FPM pool found for $domain — PHP profile not set."
    elif [ -f "/usr/local/hestia/data/templates/web/php-fpm/$tpl.tpl" ]; then
      v-change-web-domain-backend-tpl "$user" "$domain" "$tpl" > /dev/null \
        && echo "       ✓ PHP pool on $tpl" \
        || echo "       ⚠️  Could not switch $domain to $tpl."
    else
      echo "       ⚠️  No $tpl template — create it with: hs op fpm-profile version=$ver profile=$profile"
    fi
  fi
  sudo -u "$user" wp --path="$dir" config set WP_REDIS_MAXTTL 3600 --type=constant --raw --quiet
  for host in $(timeout 10 mariadb -N -B -e "SELECT Host FROM mysql.user WHERE User='$db_user'" 2>/dev/null); do
    timeout 10 mariadb -e "ALTER USER '$db_user'@'$host' WITH MAX_USER_CONNECTIONS 10" 2>/dev/null \
      || echo "       ⚠️  Could not limit connections of $db_user@$host."
  done
}

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
    # WP Rocket's functions: `wp rocket clean` is a root-only WP-CLI package.
    sudo -u "$user" wp --path="$path" eval 'rocket_clean_domain(); rocket_clean_minify(); echo "ok\n";' 2>/dev/null | grep -qx ok \
      && echo "        ✓ WP Rocket" \
      || echo "        ⚠ WP Rocket clean failed"
  fi

  if sudo -u "$user" wp --path="$path" plugin is-active woo-product-feed-pro --quiet 2>/dev/null \
     || sudo -u "$user" wp --path="$path" plugin is-active woo-feed --quiet 2>/dev/null; then
    echo "        ℹ product-feed plugin active — feeds will rebuild on next scheduled run"
  fi
}

# WP Rocket keeps the home URL base64-encoded in wp_rocket_last_base_url,
# which search-replace can't see, so every copy greets the admin with "the
# website domain has changed" and a Regenerate button. Do what that button
# does (Engine/Admin/DomainChange/Subscriber.php, 3.23): record the new URL
# and fire rocket_domain_changed, which regenerates advanced-cache.php, the
# per-host config file and .htaccess, drops the old configs and clears the
# cache. Must run on the published path: WP Rocket writes ABSPATH into
# those files. Slug-agnostic (the fleet has both wp-rocket and wprocket).
# Live's uploads are mounted read-only into staging, so files in them that
# embed live's address — GeneratePress's fonts.css and style.min.css,
# Elementor's post CSS, and WP Rocket's minified copies of them — point
# staging back at live. Browsers refuse cross-origin fonts without a CORS
# header, so staging renders in fallback fonts and the update-run checks see
# CORS errors that live never has (cafepetris.fi, 2026-09-30). Rewrite live's
# address to staging's in staging's nginx responses only; live and its files
# are untouched. Skipped without nginx's sub module; removed again if
# `nginx -t` rejects it, so staging never goes down over this.
staging_url_rewrite() { # DEST_USER STAGING_DOMAIN LIVE_BARE_HOST
  local user="$1" domain="$2" live="$3"
  local dir="/home/$user/conf/web/$domain"
  local conf="$dir/nginx.conf_staging_urls"
  local sconf="$dir/nginx.ssl.conf_staging_urls"
  if ! nginx -V 2>&1 | grep -q -- "--with-http_sub_module"; then
    echo "       ⚠️  nginx has no sub module: files in uploads keep live's address (fonts may be blocked on staging)"
    return 0
  fi
  [ -d "$dir" ] || return 0
  {
    echo "# v-wp-staging-create: live's address -> staging's in files served from"
    echo "# live's read-only uploads (and caches built from them). Staging only."
    echo "sub_filter_types text/css application/javascript text/javascript application/json image/svg+xml;"
    echo "sub_filter_once off;"
    local host scheme
    for host in "www.$live" "$live"; do
      for scheme in "https://" "http://" "//"; do
        echo "sub_filter '${scheme}${host}' 'https://${domain}';"
      done
    done
  } > "$conf"
  ln -sf "$conf" "$sconf"
  if nginx -t >/dev/null 2>&1; then
    systemctl reload nginx
    echo "       ✓ live's address rewritten to staging's in CSS/JS responses"
  else
    rm -f "$conf" "$sconf"
    echo "       ⚠️  nginx rejected the URL rewrite — left out (fonts from uploads may be blocked on staging)"
  fi
}

rocket_domain_moved() {
  local user="$1" path="$2"
  local out
  out=$(sudo -u "$user" wp --path="$path" eval '
    if (!function_exists("rocket_generate_config_file")) { echo "absent"; return; }
    $new = trailingslashit(get_option("home"));
    $old = base64_decode((string) get_option("wp_rocket_last_base_url"));
    update_option("wp_rocket_last_base_url", base64_encode($new), true);
    delete_transient("rocket_domain_changed");
    if ($old !== "" && $old !== $new) { do_action("rocket_domain_changed", $new, $old); }
    echo "ok";
  ' 2>/dev/null | tail -n1)
  case "$out" in
    ok)     echo "        ✓ WP Rocket configuration regenerated for the staging domain" ;;
    absent) ;;
    *)      echo "        ⚠ WP Rocket regenerate failed — use the notice's Regenerate button" ;;
  esac
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
    # JSON-escaped shape (https:\/\/host) inside serialized settings.
    sudo -u "$user" wp --path="$path" \
      search-replace "${v//\//\\/}" "${new_url//\//\\/}" --all-tables --quiet --skip-columns=guid 2>/dev/null
  done
}

# Pick a Redis database index no other site on this box uses. Each WordPress
# site gets its own database so a flush on one site (the drop-in's flush() is
# FLUSHDB) never empties another site's cache, and key prefixes cannot collide
# however the sites are cloned. Database 0 is left to sites installed before
# this rule. Prints the index; prints nothing when redis-cli is missing or every
# database is taken, and the caller then leaves the constant unset.
redis_next_free_db() {
  local max used i
  command -v redis-cli >/dev/null 2>&1 || return 1
  max=$(redis-cli config get databases 2>/dev/null | tail -1)
  [ "$max" -gt 1 ] 2>/dev/null || max=16
  used=$(grep -hoE "WP_REDIS_DATABASE'[[:space:]]*,[[:space:]]*'?[0-9]+" \
           /home/*/web/*/public_html/wp-config.php \
           /home/*/web/*/public_html.setup/wp-config.php 2>/dev/null \
         | grep -oE '[0-9]+$' | sort -un)
  for ((i = 1; i < max; i++)); do
    printf '%s\n' "$used" | grep -qx "$i" || { echo "$i"; return 0; }
  done
  return 1
}

# Two-step bind mount to RO, with a write probe to PROVE the mount is RO.
# Bind + remount,ro is the historically-portable way; a single mount -o ro,bind
# is silently ignored on older util-linux. The probe runs as root so the test
# is meaningful even when POSIX perms would already block writes for DEST_USER.
# Persist the read-only uploads mount across reboots with a tagged fstab line.
# `nofail` keeps a missing directory from blocking boot; util-linux applies the
# `ro` to a bind mount itself. Without this a reboot silently turns staging's
# uploads into its own writable, nearly empty folder (kuumalahde, 2026-09-25).
FSTAB_TAG="# hestia-staging-uploads"
persist_uploads_mount() {
  local src="$1" dst="$2"
  sed -i "\#[[:space:]]$dst[[:space:]]#d" /etc/fstab
  printf '%s %s none bind,ro,nofail 0 0 %s %s\n' "$src" "$dst" "$FSTAB_TAG" "$dst" >> /etc/fstab
  systemctl daemon-reload 2>/dev/null || true
  findmnt --verify --tab-file /etc/fstab >/dev/null 2>&1 \
    || echo "       ⚠ findmnt --verify reports a problem in /etc/fstab — check it before the next reboot."
}
forget_uploads_mount() {
  local dst="$1"
  sed -i "\#[[:space:]]$dst[[:space:]]#d" /etc/fstab
  systemctl daemon-reload 2>/dev/null || true
}

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
  --no-httpauth        Leave the staging site open to everyone (default: HTTP
                       basic auth; credentials in /root/.hestia-staging-auth/)
  --print-auth         On success, print the basic-auth pair once as
                       STAGING_AUTH=<user>:<password>. EngineLink captures it
                       so its screenshots and the operator get through; treat
                       the output as secret
  --flush-live         Also flush the live site's caches (object cache,
                       transients, WP Rocket). Off by default since 2026-09-29:
                       staging is built isolated with its own Redis prefix and
                       database, so live's cache is not touched by it. Use it
                       when you suspect staging URLs in live's cache.
  --php-profile=NAME   PHP-FPM profile for the staging pool (default staging:
                       4 workers at nice 10, PHP memory 512M; none = leave the
                       pool). Redis keys ≤ 1 h and ≤ 10 DB connections always.
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
HTTPAUTH=true
PRINT_AUTH=false
FLUSH_LIVE=false
PHP_PROFILE=staging

while [[ $# -gt 0 ]]; do
  case "$1" in
    --src-user=*)   SRC_USER="${1#*=}" ;;
    --src-domain=*) OLD_WEB_DOMAIN="${1#*=}" ;;
    --dest-user=*)  DEST_USER="${1#*=}" ;;
    --new-domain=*) NEW_DOMAIN="${1#*=}" ;;
    --force)        FORCE=true ;;
    --teardown)     TEARDOWN=true ;;
    --no-httpauth)  HTTPAUTH=false ;;
    --print-auth)   PRINT_AUTH=true ;;
    --flush-live)   FLUSH_LIVE=true ;;
    --php-profile=*) PHP_PROFILE="${1#*=}" ;;
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
    forget_uploads_mount "$mp"
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
        reload_php_fpm   # live wp-config changed
      fi
      if [ "$FLUSH_LIVE" = true ]; then
        flush_site_caches "$SRC_USER" "$LIVE_DIR" "live ($OLD_WEB_DOMAIN)"
      fi
    fi
  fi

  rm -f "$(staging_auth_store "$NEW_DOMAIN")"
  rm -f "$(copy_registry_file "$NEW_DOMAIN")"

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
  # On the source site's IP: on UpCloud boxes the public address is NATed to a
  # private interface, and a new user's default IP is the private one — a copy
  # created there is unreachable and Let's Encrypt fails (dev2, 2026-09-29).
  SRC_IP=$(v-list-web-domain "$SRC_USER" "$OLD_WEB_DOMAIN" json 2>/dev/null | grep -oP '"IP": "\K[0-9.]+' | head -1)
  if [ -n "$SRC_IP" ]; then
    v-add-web-domain "$DEST_USER" "$NEW_WEB_DOMAIN" "$SRC_IP"
  else
    v-add-web-domain "$DEST_USER" "$NEW_WEB_DOMAIN"
  fi
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

# The prefix keeps keys apart; a Redis database of its own keeps *flushes*
# apart (the drop-in's flush() is FLUSHDB). Without it a `wp cache flush` on
# staging empties live's object cache, and vice versa.
REDIS_DB=$(redis_next_free_db || true)
if [ -n "$REDIS_DB" ]; then
  echo "       Isolating the object cache in Redis database $REDIS_DB..."
  $WP_STG config set WP_REDIS_DATABASE        "$REDIS_DB" --type=constant --raw --quiet
  $WP_STG config set WP_REDIS_SELECTIVE_FLUSH true        --type=constant --raw --quiet
else
  echo "       ⚠️  No free Redis database — staging shares database 0 with live; a cache flush on either empties both."
fi

echo "       Pinning WP_HOME/WP_SITEURL to staging..."
$WP_STG config set WP_HOME    "$NEW_WP_URL" --type=constant --quiet
$WP_STG config set WP_SITEURL "$NEW_WP_URL" --type=constant --quiet

echo "       Hardening: WP_ENVIRONMENT_TYPE=staging, DISABLE_WP_CRON, DISALLOW_FILE_MODS..."
$WP_STG config set WP_ENVIRONMENT_TYPE "staging" --type=constant --quiet
$WP_STG config set DISABLE_WP_CRON     "true"    --type=constant --raw --quiet
$WP_STG config set DISALLOW_FILE_MODS  "true"    --type=constant --raw --quiet

# Live's uploads are mounted read-only here, so a WP_TEMP_DIR inside them (live
# kuumalahde.fi sets it to wp-content/uploads) makes every update download fail
# on the copy (2026-10-01). The copy's own tmp is in its open_basedir.
case "$($WP_STG config get WP_TEMP_DIR 2>/dev/null || true)" in
  *uploads*)
    echo "       WP_TEMP_DIR pointed into the (read-only) uploads — now /home/$DEST_USER/tmp"
    $WP_STG config set WP_TEMP_DIR "/home/$DEST_USER/tmp" --type=constant --quiet ;;
esac

# PHP errors go to a log, never onto the page: EngineLink's update run
# empties it before updating staging and reads it after the page checks
# (v-wp-debug-log). private/ is outside the docroot, inside open_basedir.
echo "       Debug log: private/wp-debug.log (display off)..."
$WP_STG config set WP_DEBUG         "true"  --type=constant --raw --quiet
$WP_STG config set WP_DEBUG_DISPLAY "false" --type=constant --raw --quiet
$WP_STG config set WP_DEBUG_LOG     "/home/$DEST_USER/web/$NEW_WEB_DOMAIN/private/wp-debug.log" --type=constant --quiet

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

# Paths outside public_html (private/, logs/) and constants in wp-config.php
# are not covered above. On the same user nothing blocks them, so a cloned
# WP_DEBUG_LOG under live's private/ makes staging write into live's log
# (staging.kuumalahde.fi, 2026-09-22).
LIVE_HOME="/home/$SRC_USER/web/$OLD_WEB_DOMAIN"
NEW_HOME="/home/$DEST_USER/web/$NEW_WEB_DOMAIN"
echo "       Domain-root paths: $LIVE_HOME  →  $NEW_HOME (database and wp-config.php)"
$WP_STG search-replace "$LIVE_HOME" "$NEW_HOME" --all-tables --quiet --skip-columns=guid
# Regex-escape: a bare "valolink.fi" would also match "valolink-fi…".
LIVE_HOME_RE=$(printf '%s' "$LIVE_HOME" | sed 's/[.[\*^$#]/\\&/g')
sed -i "s#$LIVE_HOME_RE#$NEW_HOME#g" "$SETUP_DIR/wp-config.php"

# Files outside the database that hardcode live's absolute path, copied
# verbatim by the rsync: cache drop-ins (WP Rocket's advanced-cache.php
# carries its plugin path) and root-level loaders (.user.ini
# auto_prepend_file, wordfence-waf.php). On staging they point into live,
# which open_basedir refuses with a warning on every page. Only these
# generated top-level files — plugin code is never rewritten.
for f in "$SETUP_DIR"/*.php "$SETUP_DIR"/.user.ini "$SETUP_DIR"/.htaccess "$SETUP_DIR"/wp-content/*.php; do
  [ -f "$f" ] && [ ! -L "$f" ] && [ "$f" != "$SETUP_DIR/wp-config.php" ] || continue
  if grep -qF "$LIVE_HOME" "$f"; then
    sed -i "s#$LIVE_HOME_RE#$NEW_HOME#g" "$f"
    echo "       Live path rewritten in ${f#$SETUP_DIR/}"
  fi
done
# Absolute symlinks into live (Query Monitor's wp-content/db.php links to
# its plugin file by absolute path) are copied as-is by rsync -a and would
# resolve to live's files. Repoint them at the same path under staging.
while IFS= read -r -d '' link; do
  target=$(readlink "$link")
  ln -sfn "$NEW_HOME${target#"$LIVE_HOME"}" "$link"
  chown -h "$DEST_USER:$DEST_USER" "$link"
  echo "       Symlink repointed: ${link#$SETUP_DIR/} → staging"
done < <(find "$SETUP_DIR" -path "$SETUP_DIR/wp-content/uploads" -prune -o -type l -lname "$LIVE_HOME/*" -print0)
mkdir -p "$NEW_HOME/private" && chown "$DEST_USER:$DEST_USER" "$NEW_HOME/private"

# valolink-plugin's Staging module (noindex, mail interception, live payment
# gateways off, auto-updates off) — switched on in the copy only. Live keeps
# whatever it had: enabling the module on live is unsafe, because its
# subdomain heuristic would flip a live site served on a subdomain into
# staging mode. WP_ENVIRONMENT_TYPE=staging (above) is what the module's
# detector treats as authoritative, so it activates here and nowhere else.
if $WP_STG plugin is-active valolink-plugin --skip-plugins --skip-themes 2>/dev/null; then
  echo "       Enabling valolink-plugin's Staging module in the copy..."
  $WP_STG eval --skip-plugins --skip-themes '
    $s = get_option("valolink_settings");
    if (!is_array($s)) { $s = []; }
    $s["modules"]["staging"]["enabled"] = true;
    update_option("valolink_settings", $s);
    $check = get_option("valolink_settings");
    echo empty($check["modules"]["staging"]["enabled"]) ? "fail" : "ok";
  ' 2>/dev/null | grep -qx ok
  check_status "Failed to enable the Staging module in the copy."
  echo "       ✓ Staging module on (copy only)"
else
  echo "       ⚠️  valolink-plugin not active — no Staging module: mail and payment gateways are NOT intercepted on staging."
fi

# [8/11] Pre-publish verification.
# Refuse to publish if any of the safety constants or URLs are wrong.
echo "[8/11] Verifying staging is safe to publish..."
require_wpconfig "$DEST_USER" "$SETUP_DIR" DB_NAME             "$NEW_DB_NAME"
require_wpconfig "$DEST_USER" "$SETUP_DIR" WP_REDIS_PREFIX     "$REDIS_SAFE_PREFIX"
require_wpconfig "$DEST_USER" "$SETUP_DIR" WP_CACHE_KEY_SALT   "$REDIS_SAFE_PREFIX"
[ -z "$REDIS_DB" ] || require_wpconfig "$DEST_USER" "$SETUP_DIR" WP_REDIS_DATABASE "$REDIS_DB"
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
if [ "$HTTPAUTH" = true ]; then
  echo "       HTTP basic auth on $NEW_WEB_DOMAIN (credentials: $(staging_auth_store "$NEW_WEB_DOMAIN"), root only)"
  ensure_staging_httpauth "$DEST_USER" "$NEW_WEB_DOMAIN"
  check_status "Failed to set HTTP auth on $NEW_WEB_DOMAIN."
fi
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
persist_uploads_mount "$LIVE_UPLOADS" "$NEW_UPLOADS"
echo "       ✓ persisted in /etc/fstab (bind,ro,nofail) — survives reboots"

# [11/11] Reload PHP-FPM (so workers drop stale wp-config) and flush staging's
# caches: its workers may have cached the empty disarmed wp-config. Live's
# caches are flushed only with --flush-live (since 2026-09-29). The live flush
# was added 2026-06-26 against staging URLs leaking into live's Redis during
# the build's traffic window; the isolated build, staging's own Redis prefix
# and database closed that, and the flush emptied live's object cache,
# transients and page cache on every refresh — on a box whose sites share a
# Redis database, every site's object cache.
echo "[11/11] Reloading PHP-FPM and flushing caches..."
apply_copy_limits "$DEST_USER" "$NEW_WEB_DOMAIN" "$NEW_DIR" "$NEW_DB_USER" "$PHP_PROFILE"
reload_php_fpm
staging_url_rewrite "$DEST_USER" "$NEW_WEB_DOMAIN" "$OLD_BARE"
rocket_domain_moved "$DEST_USER" "$NEW_DIR"
flush_site_caches "$DEST_USER" "$NEW_DIR"  "staging ($NEW_WEB_DOMAIN)"
if [ "$FLUSH_LIVE" = true ]; then
  flush_site_caches "$SRC_USER"  "$LIVE_DIR" "live ($OLD_WEB_DOMAIN)"
else
  echo "      Live caches left alone (--flush-live to flush them)."
fi

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

register_copy staging "$NEW_WEB_DOMAIN" "$DEST_USER" "$OLD_WEB_DOMAIN" "$SRC_USER"

echo ""
echo "=================================================================="
echo "✅ Staging Setup Complete!"
echo "   Live site:    $OLD_DOMAIN (user: $SRC_USER)"
echo "   Staging site: $NEW_WEB_DOMAIN (user: $DEST_USER)"
echo "   Database:     $NEW_DB_NAME"
echo "   Redis prefix: $REDIS_SAFE_PREFIX"
echo "   Uploads:      RO bind-mount from live (write-probed)"
if [ "$HTTPAUTH" = true ]; then
  echo "   Access:       HTTP basic auth — $(staging_auth_store "$NEW_WEB_DOMAIN") (root only)"
else
  echo "   Access:       open to everyone (--no-httpauth)"
fi
echo "   Stored URL:   $URL_STORE"
echo ""
if [ "$HTTPAUTH" = true ] && [ "$PRINT_AUTH" = true ]; then
  read -r _auth_user _auth_pass < "$(staging_auth_store "$NEW_WEB_DOMAIN")"
  echo "STAGING_AUTH=${_auth_user}:${_auth_pass}"
fi
echo "   To tear down:"
echo "   v-wp-staging-create --teardown --dest-user=$DEST_USER \\"
echo "       --new-domain=$NEW_WEB_DOMAIN --src-user=$SRC_USER \\"
echo "       --src-domain=$OLD_WEB_DOMAIN"
echo "=================================================================="
