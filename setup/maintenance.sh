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
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice

    case "$choice" in
      1) _maintenance_deploy ;;
      2) _maintenance_system_update ;;
      3) _maintenance_hestia_update ;;
      4) _maintenance_maldet_scan ;;
      5) _maintenance_filemanager_fix ;;
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
