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
    "apt update" \
    "apt upgrade -y"
  echo "  ℹ️  Verify that WooCommerce and site functionality still works after updates."
  press_enter
}

_maintenance_hestia_update() {
  run_action "Update HestiaCP" \
    "apt update" \
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

_svc_installed() {
  dpkg -l "$1" 2>/dev/null | grep -q "^ii"
}

_svc_status_line() {
  local label="$1" pkg="$2"
  if _svc_installed "$pkg"; then
    status_line "$label" WARN "installed"
  else
    status_line "$label" OK "not installed"
  fi
}

menu_service_cleanup() {
  while true; do
    clear
    echo ""
    echo -e "  ${BOLD}Remove Unnecessary Services${NC}"
    echo "$DIV"
    echo "  Not needed for WordPress-only hosting (no email mailboxes):"
    echo ""
    _svc_status_line "Dovecot  (IMAP/POP3)" "dovecot-core"
    _svc_status_line "ClamAV   (antivirus)" "clamav"
    _svc_status_line "SpamAssassin" "spamassassin"
    _svc_status_line "VSFTPD   (FTP)" "vsftpd"
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
    echo "  Dovecot is not installed."; press_enter; return
  fi
  echo ""
  echo "  Removing Dovecot disables IMAP/POP3 mailbox access."
  echo "  Exim SMTP AUTH (used when clients send mail through this server) will also"
  echo "  stop working — fine if all outbound mail goes via Resend."
  echo ""
  if ! confirm "Remove Dovecot?"; then return; fi
  echo ""
  echo -e "  ${CYAN}→${NC} Stopping and removing Dovecot..."
  systemctl stop dovecot 2>/dev/null
  apt remove --purge -y dovecot-core dovecot-imapd dovecot-pop3d dovecot-lmtpd 2>/dev/null
  apt autoremove -y
  echo -e "  ${GREEN}✓ Done${NC}"
  press_enter
}

_cleanup_clamav() {
  if ! _svc_installed clamav; then
    echo "  ClamAV is not installed."; press_enter; return
  fi
  echo ""
  echo "  Removing ClamAV will also update the Exim config to disable AV scanning,"
  echo "  then restart Exim. Outbound mail delivery will not be affected."
  echo ""
  if ! confirm "Remove ClamAV?"; then return; fi
  echo ""
  echo -e "  ${CYAN}→${NC} Stopping ClamAV services..."
  systemctl stop clamav-daemon clamav-freshclam 2>/dev/null
  systemctl disable clamav-daemon clamav-freshclam 2>/dev/null
  echo -e "  ${CYAN}→${NC} Removing packages..."
  apt remove --purge -y clamav clamav-daemon clamav-freshclam clamav-base 2>/dev/null
  apt autoremove -y
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
    echo "  SpamAssassin is not installed."; press_enter; return
  fi
  echo ""
  echo "  Removing SpamAssassin will also update the Exim config to disable spam"
  echo "  scanning, then restart Exim."
  echo ""
  if ! confirm "Remove SpamAssassin?"; then return; fi
  echo ""
  echo -e "  ${CYAN}→${NC} Stopping SpamAssassin..."
  systemctl stop spamassassin 2>/dev/null
  systemctl disable spamassassin 2>/dev/null
  echo -e "  ${CYAN}→${NC} Removing packages..."
  apt remove --purge -y spamassassin spamc 2>/dev/null
  apt autoremove -y
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
    echo "  VSFTPD is not installed."; press_enter; return
  fi
  echo ""
  echo "  Clients using FTP will lose access. SFTP (via SSH) continues to work."
  echo ""
  if ! confirm "Remove VSFTPD?"; then return; fi
  echo ""
  echo -e "  ${CYAN}→${NC} Stopping and removing VSFTPD..."
  systemctl stop vsftpd 2>/dev/null
  apt remove --purge -y vsftpd
  apt autoremove -y
  echo -e "  ${GREEN}✓ Done${NC}"
  press_enter
}
