#!/bin/bash
# v-server-setup-status — non-interactive JSON snapshot of the same checks
# run.sh's status dashboard (setup/status.sh) shows, so EngineLink can render
# the box's setup coverage remotely. Self-contained (v-scripts don't source
# the setup/ modules); every probe is bounded so a wedged daemon can't hang
# the streamer request.
#
# Contract (same as v-server-health): progress lines are free-form, the
# FINAL non-empty stdout line is a single-line JSON object. Always exits 0
# on a completed run — missing tools degrade to false/null, never failure.

if [ "$EUID" -ne 0 ]; then
  echo "ERROR: Please run as root."
  exit 1
fi

export PATH=$PATH:/usr/local/hestia/bin

echo "Collecting server setup status..."

json_escape() { sed 's/\\/\\\\/g; s/"/\\"/g' <<<"$1" | tr -d '\n'; }
b() { [ "$1" = "1" ] && echo true || echo false; }

get_php_versions() {
  find /etc/php -maxdepth 1 -mindepth 1 -type d -printf '%f\n' 2>/dev/null | sort -V
}

# --- streamer + deployed v-scripts ------------------------------------------
STREAMER_RUNNING=0
systemctl is-active --quiet hestia-streamer 2>/dev/null && STREAMER_RUNNING=1
VSCRIPT_COUNT=$(find /usr/local/hestia/bin/ -maxdepth 1 -name "v-*" -type l 2>/dev/null | wc -l)

# --- WP-CLI -------------------------------------------------------------------
WPCLI_INSTALLED=0 WPCLI_VERSION=""
if command -v wp &>/dev/null; then
  WPCLI_INSTALLED=1
  WPCLI_VERSION=$(timeout 5 wp --version --allow-root 2>/dev/null | awk '{print $2}')
fi

# --- Chromium (headless rendering) ---------------------------------------------
CHROMIUM_INSTALLED=0 CHROMIUM_VERSION=""
CHROMIUM_BIN=$(command -v chromium 2>/dev/null || command -v chromium-browser 2>/dev/null)
if [ -n "$CHROMIUM_BIN" ]; then
  CHROMIUM_INSTALLED=1
  CHROMIUM_VERSION=$(timeout 5 "$CHROMIUM_BIN" --version 2>/dev/null | grep -oP '[0-9][0-9.]*' | head -1)
fi

# --- Redis ---------------------------------------------------------------------
REDIS_INSTALLED=0 REDIS_RUNNING=0 REDIS_MAXMEM=0 REDIS_POLICY=""
PHP_REDIS_EXT="{}"
if command -v redis-server &>/dev/null; then
  REDIS_INSTALLED=1
  if systemctl is-active --quiet redis-server 2>/dev/null; then
    REDIS_RUNNING=1
    REDIS_MAXMEM=$(timeout 3 redis-cli config get maxmemory 2>/dev/null | tail -1)
    REDIS_POLICY=$(timeout 3 redis-cli config get maxmemory-policy 2>/dev/null | tail -1)
    [[ "$REDIS_MAXMEM" =~ ^[0-9]+$ ]] || REDIS_MAXMEM=0
  fi
fi
ext_parts=""
for ver in $(get_php_versions); do
  if timeout 3 php${ver} -r "echo extension_loaded('redis') ? 1 : 0;" 2>/dev/null | grep -q "^1$"; then
    ext_parts+="\"${ver}\":true,"
  else
    ext_parts+="\"${ver}\":false,"
  fi
done
PHP_REDIS_EXT="{${ext_parts%,}}"

# --- Fail2ban -------------------------------------------------------------------
F2B_INSTALLED=0 F2B_RUNNING=0 F2B_WPJAIL="missing"
if command -v fail2ban-client &>/dev/null; then
  F2B_INSTALLED=1
  if systemctl is-active --quiet fail2ban 2>/dev/null; then
    F2B_RUNNING=1
    timeout 3 fail2ban-client status wordpress &>/dev/null
    case $? in
      0)   F2B_WPJAIL="active" ;;
      124) F2B_WPJAIL="unresponsive" ;;
      *)   F2B_WPJAIL="missing" ;;
    esac
  fi
fi

# --- Maldet ----------------------------------------------------------------------
MALDET_INSTALLED=0 MALDET_LAST_SCAN=""
if [ -f /usr/local/maldetect/maldet ]; then
  MALDET_INSTALLED=1
  MALDET_LAST_SCAN=$(grep "scan completed" /usr/local/maldetect/logs/event_log 2>/dev/null | tail -1 | awk '{print $1, $2}')
fi

# --- Netdata ---------------------------------------------------------------------
NETDATA_INSTALLED=0 NETDATA_RUNNING=0
if command -v netdata &>/dev/null; then
  NETDATA_INSTALLED=1
  systemctl is-active --quiet netdata 2>/dev/null && NETDATA_RUNNING=1
fi

# --- Security (swap / SSH / unattended upgrades) ----------------------------------
SWAP_OK=0
[ -n "$(swapon --show --noheadings 2>/dev/null)" ] && SWAP_OK=1
SSH_PW=$(grep "^PasswordAuthentication" /etc/ssh/sshd_config 2>/dev/null | awk '{print $2}')
SSH_KEY_ONLY=0
[ "$SSH_PW" = "no" ] && SSH_KEY_ONLY=1
UU_FILE="/etc/apt/apt.conf.d/50unattended-upgrades"
if [ "$(dpkg -l unattended-upgrades 2>/dev/null | grep -c '^ii')" -eq 0 ]; then
  UU_STATE="missing"
elif [ ! -f "$UU_FILE" ]; then
  UU_STATE="unknown"
elif grep -qE '^[[:space:]]*"origin=Debian[^"]*,label=Debian";' "$UU_FILE" 2>/dev/null; then
  UU_STATE="all"
else
  UU_STATE="security-only"
fi

# --- SMTP relay --------------------------------------------------------------------
SMTP_RELAY=$(postconf -h relayhost 2>/dev/null)
[ "$SMTP_RELAY" = "[]" ] && SMTP_RELAY=""

# --- PHP-FPM profiles ----------------------------------------------------------------
FPM_OK=0 FPM_MISSING=0
TPL_DIR="/usr/local/hestia/data/templates/web/php-fpm"
for ver in $(get_php_versions); do
  ver_tag="${ver//./_}"
  for p in production standard staging small; do
    if [ -f "$TPL_DIR/${p}-PHP-${ver_tag}.tpl" ]; then FPM_OK=$((FPM_OK+1)); else FPM_MISSING=$((FPM_MISSING+1)); fi
  done
done

# --- OpCache ---------------------------------------------------------------------------
OC_OK=0 OC_BAD=0
for ver in $(get_php_versions); do
  ini="/etc/php/${ver}/fpm/php.ini"
  [ -f "$ini" ] || continue
  enabled=$(grep -oP "(?<=opcache\.enable=)\d" "$ini" 2>/dev/null | head -1)
  mem=$(grep -oP "(?<=opcache\.memory_consumption=)\d+" "$ini" 2>/dev/null | head -1)
  if [ "${enabled:-0}" = "1" ] && [ "${mem:-0}" -ge 256 ] 2>/dev/null; then OC_OK=$((OC_OK+1)); else OC_BAD=$((OC_BAD+1)); fi
done

# --- MariaDB buffer pool -----------------------------------------------------------------
MARIADB_BUFFER=$(grep -oP "(?<=innodb_buffer_pool_size\s=\s)\S+" /etc/mysql/mariadb.conf.d/50-server.cnf 2>/dev/null | head -1)

# --- Nginx templates ----------------------------------------------------------------------
NTPL_DIR="/usr/local/hestia/data/templates/web/nginx"
WP_SECURE=0 WP_ROCKET=0
[ -f "$NTPL_DIR/wp-secure.tpl" ] && [ -f "$NTPL_DIR/wp-secure.stpl" ] && WP_SECURE=1
[ -f "$NTPL_DIR/wp-rocket.tpl" ] && [ -f "$NTPL_DIR/wp-rocket.stpl" ] && WP_ROCKET=1

# --- HestiaCP version (latest lookup bounded; offline → null) --------------------------------
HESTIA_INSTALLED=$(grep -oP "(?<=VERSION=')[^']+" /usr/local/hestia/conf/hestia.conf 2>/dev/null | head -1)
HESTIA_LATEST=$(curl -sf --max-time 4 "https://api.github.com/repos/hestiacp/hestiacp/releases/latest" 2>/dev/null \
  | grep '"tag_name"' | cut -d'"' -f4 | tr -d 'v')

# --- Restic backups (config posture only — freshness is v-server-health's job) ------------------
# Same paths v-server-health uses: system repo in conf/restic.conf (REPO=),
# per-user keys in data/users/<u>/restic.conf. No network calls here.
RESTIC_SYSTEM_REPO=0
if [ -r /usr/local/hestia/conf/restic.conf ] \
  && grep -q "^REPO=" /usr/local/hestia/conf/restic.conf 2>/dev/null; then
  RESTIC_SYSTEM_REPO=1
fi
RESTIC_USERS_TOTAL=0
RESTIC_USERS_KEYED=0
for udir in /usr/local/hestia/data/users/*/; do
  [ -d "$udir" ] || continue
  RESTIC_USERS_TOTAL=$((RESTIC_USERS_TOTAL+1))
  [ -r "${udir}restic.conf" ] && RESTIC_USERS_KEYED=$((RESTIC_USERS_KEYED+1))
done

# --- Disk --------------------------------------------------------------------------------------
DISK_PCT=$(df / 2>/dev/null | awk 'NR==2 {gsub(/%/,"",$5); print $5}')
DISK_INFO=$(df -h / 2>/dev/null | awk 'NR==2 {print $3 " / " $2}')

echo "Checks complete."

printf '{"streamer":{"running":%s,"vScripts":%s},"wpcli":{"installed":%s,"version":"%s"},"chromium":{"installed":%s,"version":"%s"},"redis":{"installed":%s,"running":%s,"maxmemory":%s,"policy":"%s","phpExt":%s},"fail2ban":{"installed":%s,"running":%s,"wpJail":"%s"},"maldet":{"installed":%s,"lastScan":"%s"},"netdata":{"installed":%s,"running":%s},"security":{"swap":%s,"sshKeyOnly":%s,"unattendedUpgrades":"%s"},"smtp":{"relay":"%s"},"phpFpmProfiles":{"installed":%s,"missing":%s},"opcache":{"ok":%s,"needsAttention":%s},"mariadb":{"bufferPool":"%s"},"nginxTemplates":{"wpSecure":%s,"wpRocket":%s},"hestia":{"installed":"%s","latest":"%s"},"restic":{"systemRepo":%s,"usersWithKeys":%s,"usersTotal":%s},"disk":{"usedPct":%s,"info":"%s"}}\n' \
  "$(b $STREAMER_RUNNING)" "${VSCRIPT_COUNT:-0}" \
  "$(b $WPCLI_INSTALLED)" "$(json_escape "${WPCLI_VERSION}")" \
  "$(b $CHROMIUM_INSTALLED)" "$(json_escape "${CHROMIUM_VERSION}")" \
  "$(b $REDIS_INSTALLED)" "$(b $REDIS_RUNNING)" "${REDIS_MAXMEM:-0}" "$(json_escape "${REDIS_POLICY}")" "$PHP_REDIS_EXT" \
  "$(b $F2B_INSTALLED)" "$(b $F2B_RUNNING)" "$F2B_WPJAIL" \
  "$(b $MALDET_INSTALLED)" "$(json_escape "${MALDET_LAST_SCAN}")" \
  "$(b $NETDATA_INSTALLED)" "$(b $NETDATA_RUNNING)" \
  "$(b $SWAP_OK)" "$(b $SSH_KEY_ONLY)" "$UU_STATE" \
  "$(json_escape "${SMTP_RELAY}")" \
  "${FPM_OK:-0}" "${FPM_MISSING:-0}" \
  "${OC_OK:-0}" "${OC_BAD:-0}" \
  "$(json_escape "${MARIADB_BUFFER}")" \
  "$(b $WP_SECURE)" "$(b $WP_ROCKET)" \
  "$(json_escape "${HESTIA_INSTALLED}")" "$(json_escape "${HESTIA_LATEST}")" \
  "$(b $RESTIC_SYSTEM_REPO)" "${RESTIC_USERS_KEYED:-0}" "${RESTIC_USERS_TOTAL:-0}" \
  "${DISK_PCT:-0}" "$(json_escape "${DISK_INFO}")"

exit 0
