#!/bin/bash
# WP-CLI install / update

menu_wpcli() {
  while true; do
    clear
    echo ""
    echo -e "  ${BOLD}WP-CLI${NC}"
    echo "$DIV"

    if command -v wp &>/dev/null; then
      local ver
      ver=$(wp --version --allow-root 2>/dev/null | awk '{print $2}')
      status_line "WP-CLI" OK "$ver"
    else
      status_line "WP-CLI" ERR "not installed"
    fi

    echo ""
    echo "  1) Install WP-CLI"
    echo "  2) Update WP-CLI"
    echo "  3) Run secure_php.sh  (fix disabled PHP CLI functions for WP-CLI)"
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice

    case "$choice" in
      1) _wpcli_install ;;
      2) _wpcli_update ;;
      3) _wpcli_secure_php ;;
      0) return ;;
    esac
  done
}

_wpcli_install() {
  run_action "Install WP-CLI" \
    "curl -O https://raw.githubusercontent.com/wp-cli/builds/gh-pages/phar/wp-cli.phar" \
    "chmod +x wp-cli.phar" \
    "mv wp-cli.phar /usr/local/bin/wp"
}

_wpcli_update() {
  if ! command -v wp &>/dev/null; then
    echo "  WP-CLI is not installed. Use option 1 first."
    press_enter; return
  fi
  run_action "Update WP-CLI" \
    "wp cli update --allow-root"
}

_wpcli_secure_php() {
  local script="/usr/local/hestia/install/upgrade/manual/secure_php.sh"
  if [ ! -f "$script" ]; then
    echo "  $script not found on this server."
    press_enter; return
  fi
  run_action "Run secure_php.sh (re-enables exec/proc_open in PHP CLI)" \
    "bash $script"
}
