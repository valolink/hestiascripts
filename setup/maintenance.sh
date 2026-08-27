#!/bin/bash
# Maintenance — updates, scans, fixes, deploy

menu_maintenance() {
  while true; do
    clear
    echo ""
    echo -e "  ${BOLD}Maintenance${NC}"
    echo "$DIV"

    local hestia_ver
    hestia_ver=$(grep -oP "(?<=VERSION=')[^']+" /usr/local/hestia/conf/hestia.conf 2>/dev/null | head -1)
    sub_line "HestiaCP" "${hestia_ver:-(unknown)}"

    local last_update
    last_update=$(stat -c %y /var/lib/dpkg/info/hestia.list 2>/dev/null | cut -d' ' -f1)
    sub_line "Last package update" "${last_update:-(unknown)}"

    echo ""
    echo "  1) Deploy v-scripts & hestia-streamer  (runs install-scripts.sh)"
    echo "  2) Update system packages  (apt update && apt upgrade)"
    echo "  3) Update HestiaCP"
    echo "  4) Run malware scan"
    echo "  5) Apply HestiaCP filemanager fix"
    echo "  6) Remove unnecessary services"
    echo "  7) Check apt repositories"
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice

    case "$choice" in
      1) _maintenance_deploy ;;
      2) _maintenance_system_update ;;
      3) _maintenance_hestia_update ;;
      4) _maintenance_maldet_scan ;;
      5) _maintenance_filemanager_fix ;;
      6) menu_service_cleanup ;;
      7) _maintenance_check_repos ;;
      0) return ;;
    esac
  done
}

_maintenance_deploy() {
  local deploy="$SCRIPT_DIR/install-scripts.sh"
  if [ ! -f "$deploy" ]; then
    echo "  install-scripts.sh not found at $deploy"; press_enter; return
  fi
  echo ""
  echo "  Will run: bash $deploy"
  if ! confirm "Deploy?"; then return; fi
  echo ""
  bash "$deploy"
  press_enter
}

_maintenance_system_update() {
  run_action "Update system packages" \
    "apt_update_safe" \
    "apt upgrade -y"
  echo "  ℹ️  Verify that WooCommerce and site functionality still works after updates."
  press_enter
}

_maintenance_hestia_update() {
  run_action "Update HestiaCP" \
    "apt_update_safe" \
    "apt install hestia hestia-nginx hestia-php -y" \
    "systemctl restart hestia"
}

_maintenance_maldet_scan() {
  if [ ! -f /usr/local/maldetect/maldet ]; then
    echo "  Maldet is not installed. Install it from the Core Tools menu."
    press_enter; return
  fi
  echo ""
  echo "  Scanning /home/*/web/*/public_html/ — this may take several minutes."
  if ! confirm "Run scan?"; then return; fi
  echo ""
  maldet -a /home/*/web/*/public_html/
  press_enter
}

_maintenance_filemanager_fix() {
  local target_dir="/usr/local/hestia/web/fm/backend/Services/Session/Adapters"
  local target="$target_dir/SessionStorage.php"
  local url="https://raw.githubusercontent.com/hestiacp/hestiacp/refs/heads/main/install/deb/filemanager/filegator/backend/Services/Session/Adapters/SessionStorage.php"

  echo ""
  echo "  This fixes HestiaCP's file manager session handling."
  echo "  Backs up existing SessionStorage.php → SessionStorage.php.bak"
  echo "  Downloads fresh copy from HestiaCP's main branch."
  echo ""

  run_action "Apply filemanager fix" \
    "mv $target ${target}.bak" \
    "curl -fsSLm20 '$url' -o $target" \
    "chown hestiaweb:hestiaweb $target"
}

# --- Service cleanup ---

HESTIA_CONF="/usr/local/hestia/conf/hestia.conf"

_svc_installed() {
  dpkg -l "$1" 2>/dev/null | grep -q "^ii"
}

# Read a key's value out of hestia.conf (lines look like KEY='value')
_hestia_conf_get() {
  [ -f "$HESTIA_CONF" ] || return 0
  grep -m1 "^$1=" "$HESTIA_CONF" 2>/dev/null | cut -d "'" -f2
}

# True (and echoes the value) when hestia.conf still points KEY at one of the
# values that mean "this service". Purging the package without clearing the key
# is what produces Hestia's "<service> restart failed" errors + email reports:
# v-restart-mail / v-restart-ftp gate on `[ -n "$KEY" ]` and then hand the value
# to v-restart-service, which fails on a unit that no longer exists.
_hestia_conf_stale() {
  local key="$1" values="$2" cur v
  cur=$(_hestia_conf_get "$key")
  [ -n "$cur" ] || return 1
  for v in $values; do
    if [ "$cur" = "$v" ]; then echo "$cur"; return 0; fi
  done
  return 1
}

# Clear KEY in hestia.conf, but only when it holds one of VALUES — so the vsftpd
# path can never wipe FTP_SYSTEM on a box that actually runs proftpd. An empty
# value is the same state a box has when the component was never installed
# (hst-install only writes these keys for components it installs).
_hestia_conf_clear() {
  local key="$1" values="$2" cur
  cur=$(_hestia_conf_stale "$key" "$values") || return 0
  echo -e "  ${CYAN}→${NC} Clearing $key='$cur' in hestia.conf..."
  if [ -x /usr/local/hestia/bin/v-change-sys-config-value ]; then
    /usr/local/hestia/bin/v-change-sys-config-value "$key" ""
  else
    sed -i "s|^$key=.*|$key=''|" "$HESTIA_CONF"
  fi
}

# _svc_status_line LABEL PKG CONF_KEY "conf value(s) meaning this service"
_svc_status_line() {
  local label="$1" pkg="$2" key="$3" values="$4" cur
  if _svc_installed "$pkg"; then
    status_line "$label" WARN "installed"
  elif cur=$(_hestia_conf_stale "$key" "$values"); then
    status_line "$label" ERR "gone, but $key='$cur'"
  else
    status_line "$label" OK "not installed"
  fi
}

# Package already purged, hestia.conf never updated → offer the config fix alone.
# Returns 0 when it handled the situation (caller should stop).
_cleanup_repair_conf() {
  local label="$1" key="$2" values="$3" cur
  cur=$(_hestia_conf_stale "$key" "$values") || return 1
  echo ""
  echo "  $label is not installed, but hestia.conf still has $key='$cur'."
  echo "  That is what makes Hestia report \"$cur restart failed\" — it keeps trying"
  echo "  to restart a service that is no longer on the box."
  if ! confirm "Clear $key in hestia.conf?"; then return 0; fi
  echo ""
  _hestia_conf_clear "$key" "$values"
  echo -e "  ${GREEN}✓ Done${NC}"
  press_enter
  return 0
}

menu_service_cleanup() {
  while true; do
    clear
    echo ""
    echo -e "  ${BOLD}Remove Unnecessary Services${NC}"
    echo "$DIV"
    echo "  Not needed for WordPress-only hosting (no email mailboxes):"
    echo ""
    _svc_status_line "Dovecot  (IMAP/POP3)" "dovecot-core" "IMAP_SYSTEM" "dovecot"
    _svc_status_line "ClamAV   (antivirus)" "clamav" "ANTIVIRUS_SYSTEM" "clamav-daemon clamav clamd"
    _svc_status_line "SpamAssassin" "spamassassin" "ANTISPAM_SYSTEM" "spamassassin spamd"
    _svc_status_line "VSFTPD   (FTP)" "vsftpd" "FTP_SYSTEM" "vsftpd"
    echo ""
    echo "  1) Remove Dovecot"
    echo "  2) Remove ClamAV"
    echo "  3) Remove SpamAssassin"
    echo "  4) Remove VSFTPD"
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice
    case "$choice" in
      1) _cleanup_dovecot ;;
      2) _cleanup_clamav ;;
      3) _cleanup_spamassassin ;;
      4) _cleanup_vsftpd ;;
      0) return ;;
    esac
  done
}

_cleanup_dovecot() {
  if ! _svc_installed dovecot-core; then
    _cleanup_repair_conf "Dovecot" "IMAP_SYSTEM" "dovecot" && return
    echo "  Dovecot is not installed."; press_enter; return
  fi
  echo ""
  echo "  Removing Dovecot disables IMAP/POP3 mailbox access."
  echo "  Exim SMTP AUTH (used when clients send mail through this server) will also"
  echo "  stop working — fine if all outbound mail goes via Resend."
  echo "  hestia.conf IMAP_SYSTEM is cleared so Hestia stops trying to restart it."
  echo ""
  if ! confirm "Remove Dovecot?"; then return; fi
  echo ""
  echo -e "  ${CYAN}→${NC} Stopping and removing Dovecot..."
  systemctl stop dovecot 2>/dev/null
  apt remove --purge -y dovecot-core dovecot-imapd dovecot-pop3d dovecot-lmtpd 2>/dev/null
  apt autoremove -y
  systemctl reset-failed dovecot 2>/dev/null
  _hestia_conf_clear "IMAP_SYSTEM" "dovecot"
  echo -e "  ${GREEN}✓ Done${NC}"
  press_enter
}

_cleanup_clamav() {
  if ! _svc_installed clamav; then
    _cleanup_repair_conf "ClamAV" "ANTIVIRUS_SYSTEM" "clamav-daemon clamav clamd" && return
    echo "  ClamAV is not installed."; press_enter; return
  fi
  echo ""
  echo "  Removing ClamAV will also update the Exim config to disable AV scanning,"
  echo "  then restart Exim. Outbound mail delivery will not be affected."
  echo "  hestia.conf ANTIVIRUS_SYSTEM is cleared so Hestia stops trying to restart it."
  echo ""
  if ! confirm "Remove ClamAV?"; then return; fi
  echo ""
  echo -e "  ${CYAN}→${NC} Stopping ClamAV services..."
  systemctl stop clamav-daemon clamav-freshclam 2>/dev/null
  systemctl disable clamav-daemon clamav-freshclam 2>/dev/null
  echo -e "  ${CYAN}→${NC} Removing packages..."
  apt remove --purge -y clamav clamav-daemon clamav-freshclam clamav-base 2>/dev/null
  apt autoremove -y
  systemctl reset-failed clamav-daemon clamav-freshclam 2>/dev/null
  _hestia_conf_clear "ANTIVIRUS_SYSTEM" "clamav-daemon clamav clamd"
  _cleanup_fix_exim_clamav
  echo -e "  ${GREEN}✓ Done${NC}"
  press_enter
}

_cleanup_fix_exim_clamav() {
  local cf="/etc/exim4/exim4.conf.template"
  [ -f "$cf" ] || return 0
  echo -e "  ${CYAN}→${NC} Updating Exim config (disabling AV scanner)..."
  sed -i 's/^\(av_scanner\s*=.*\)$/#\1/' "$cf"
  sed -i 's/^\(\s*deny\s\+malware\s*=.*\)$/#\1/' "$cf"
  sed -i 's/^\(\s*message\s*=.*[Vv]irus.*\)$/#\1/' "$cf"
  if exim4 -bV &>/dev/null; then
    systemctl restart exim4
    echo -e "  ${CYAN}→${NC} Exim restarted"
  else
    echo -e "  ${YELLOW}⚠${NC}  Exim config check failed — review manually: exim4 -bV"
  fi
}

_cleanup_spamassassin() {
  if ! _svc_installed spamassassin; then
    _cleanup_repair_conf "SpamAssassin" "ANTISPAM_SYSTEM" "spamassassin spamd" && return
    echo "  SpamAssassin is not installed."; press_enter; return
  fi
  echo ""
  echo "  Removing SpamAssassin will also update the Exim config to disable spam"
  echo "  scanning, then restart Exim."
  echo "  hestia.conf ANTISPAM_SYSTEM is cleared so Hestia stops trying to restart it."
  echo ""
  if ! confirm "Remove SpamAssassin?"; then return; fi
  echo ""
  echo -e "  ${CYAN}→${NC} Stopping SpamAssassin..."
  systemctl stop spamassassin 2>/dev/null
  systemctl disable spamassassin 2>/dev/null
  echo -e "  ${CYAN}→${NC} Removing packages..."
  apt remove --purge -y spamassassin spamc 2>/dev/null
  apt autoremove -y
  systemctl reset-failed spamassassin 2>/dev/null
  _hestia_conf_clear "ANTISPAM_SYSTEM" "spamassassin spamd"
  _cleanup_fix_exim_spamassassin
  echo -e "  ${GREEN}✓ Done${NC}"
  press_enter
}

_cleanup_fix_exim_spamassassin() {
  local cf="/etc/exim4/exim4.conf.template"
  [ -f "$cf" ] || return 0
  echo -e "  ${CYAN}→${NC} Updating Exim config (disabling spam scanning)..."
  sed -i 's/^\(spamd_address\s*=.*\)$/#\1/' "$cf"
  sed -i 's/^\(\s*warn\s\+spam\s*=.*\)$/#\1/' "$cf"
  sed -i 's/^\(\s*add header.*X-Spam.*\)$/#\1/' "$cf"
  if exim4 -bV &>/dev/null; then
    systemctl restart exim4
    echo -e "  ${CYAN}→${NC} Exim restarted"
  else
    echo -e "  ${YELLOW}⚠${NC}  Exim config check failed — review manually: exim4 -bV"
  fi
}

_cleanup_vsftpd() {
  if ! _svc_installed vsftpd; then
    _cleanup_repair_conf "VSFTPD" "FTP_SYSTEM" "vsftpd" && return
    echo "  VSFTPD is not installed."; press_enter; return
  fi
  echo ""
  echo "  Clients using FTP will lose access. SFTP (via SSH) continues to work."
  echo "  hestia.conf FTP_SYSTEM is cleared so Hestia stops trying to restart it."
  echo ""
  if ! confirm "Remove VSFTPD?"; then return; fi
  echo ""
  echo -e "  ${CYAN}→${NC} Stopping and removing VSFTPD..."
  systemctl stop vsftpd 2>/dev/null
  apt remove --purge -y vsftpd
  apt autoremove -y
  systemctl reset-failed vsftpd 2>/dev/null
  _hestia_conf_clear "FTP_SYSTEM" "vsftpd"
  echo -e "  ${GREEN}✓ Done${NC}"
  press_enter
}

# Diagnose apt repository failures: which repo broke, why, which file defines
# it, and what is actually safe to change. Read-only — refreshing package lists
# installs, removes and upgrades nothing.
_maintenance_check_repos() {
  clear
  echo ""
  echo -e "  ${BOLD}Check apt repositories${NC}"
  echo "$DIV"
  echo "  Refreshing package lists. Nothing is installed, removed or upgraded."
  echo ""
  echo -e "  ${DIM}Working...${NC}"

  local out rc total ok
  out=$(apt-get update 2>&1); rc=$?
  printf '\e[1A\e[2K'

  total=$(echo "$out" | grep -cE '^(Hit|Get|Err):')
  ok=$(echo "$out" | grep -cE '^(Hit|Get):')

  if [ "$rc" -eq 0 ]; then
    status_line "Repositories" OK "$ok / $total reachable"
    echo ""
    echo -e "  ${GREEN}✓ Every configured repository responded.${NC}"
    press_enter
    return
  fi

  status_line "Repositories" WARN "$ok / $total reachable"

  local url suite reason host file
  while IFS=$'\t' read -r url suite reason; do
    [ -n "$url" ] || continue
    host="${url#*://}"; host="${host%%/*}"
    file=$(grep -rls -- "$url" /etc/apt/sources.list /etc/apt/sources.list.d/ 2>/dev/null | head -1)

    echo ""
    echo -e "  ${RED}✗${NC} ${BOLD}${host}${NC}"
    sub_line "  URL" "$url"
    sub_line "  Suite" "$suite"
    sub_line "  Reason" "$reason"
    sub_line "  Defined in" "${file:-(not found under /etc/apt)}"
    _maintenance_repo_advice "$reason"
  done < <(echo "$out" | awk '
    /^Err:/ { url=$2; suite=$3; getline r; gsub(/^[ \t]+/, "", r); print url "\t" suite "\t" r }
  ')

  _maintenance_repo_primer
  press_enter
}

_maintenance_repo_advice() {
  case "$1" in
    *403*|*404*)
      sub_line "  Meaning" "the host stopped serving this suite (moved or blocked)"
      sub_line "  Fix" "point the line at another mirror of the same version" ;;
    *NO_PUBKEY*|*EXPKEYSIG*|*KEYEXPIRED*)
      sub_line "  Meaning" "the signing key is missing or expired, not the URL"
      sub_line "  Fix" "re-import the vendor's key; leave the URL alone" ;;
    *"Could not resolve"*|*"Temporary failure"*|*"Connection failed"*|*"Connection timed out"*)
      sub_line "  Meaning" "DNS or network, likely transient"
      sub_line "  Fix" "retry; if it persists the host may be gone for good" ;;
    *"expired"*|*"Release file"*)
      sub_line "  Meaning" "the mirror is stale — its Release file aged out"
      sub_line "  Fix" "switch mirrors; a stale mirror is an abandoned mirror" ;;
    *)
      sub_line "  Fix" "see the primer below" ;;
  esac
}

# The part that makes this non-scary. Printed after every failure so the
# reasoning is on screen at the moment the decision has to be made, rather
# than in a document nobody opens.
_maintenance_repo_primer() {
  echo ""
  echo "$DIV"
  echo -e "  ${BOLD}What a broken repository actually costs you${NC}"
  echo ""
  echo "  Nothing is broken right now. Each repository supplies updates for its"
  echo "  own packages only, so one failing means you stop receiving updates"
  echo "  for that vendor's software while everything else carries on. The"
  echo "  danger is quiet: missed security updates, no warning."
  echo ""
  echo -e "  ${BOLD}Why editing the URL is safe${NC}"
  echo ""
  echo "  Every package is GPG-signed. apt refuses anything not signed by a key"
  echo "  already trusted on this box, so a wrong URL fails loudly and visibly."
  echo "  You cannot quietly pull in a hostile package by mistyping a mirror."
  echo ""
  echo -e "  ${BOLD}The one rule${NC}"
  echo ""
  echo -e "  Change the ${BOLD}host${NC}. Never change the ${BOLD}version${NC} in the path."
  echo ""
  echo "    https://dlm.mariadb.com/repo/mariadb-server/11.4/repo/debian"
  echo "            ^^^^^^^^^^^^^^^ safe to swap        ^^^^ leave alone"
  echo ""
  echo "  The version segment decides which MariaDB you get. Changing it means"
  echo "  the next 'apt upgrade' migrates your databases to a new major"
  echo "  version — that is the one edit here that can genuinely hurt."
  echo ""
  echo "  Back up the file first, then re-run this check:"
  echo "    cp /etc/apt/sources.list.d/NAME.list{,.bak}"
}
