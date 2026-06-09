#!/bin/bash

if [ "$EUID" -ne 0 ]; then
  echo "ERROR: Please run as root."
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export SCRIPT_DIR

# Source all modules
for mod in common status wpcli redis fail2ban maldet netdata security smtp \
           php-fpm opcache mariadb nginx-templates maintenance disk; do
  source "$SCRIPT_DIR/setup/${mod}.sh" || { echo "Failed to load setup/${mod}.sh"; exit 1; }
done

show_menu() {
  echo ""
  echo -e "  ${BOLD}What would you like to do?${NC}"
  echo ""
  echo "  Deploy"
  echo "    1) Deploy v-scripts & hestia-streamer"
  echo ""
  echo "  Install & Configure"
  echo "    2) WP-CLI"
  echo "    3) Redis"
  echo "    4) Fail2ban  (WP brute-force protection)"
  echo "    5) Maldet    (malware scanning)"
  echo "    6) Netdata   (monitoring)"
  echo "    7) Security  (unattended upgrades, swap, SSH)"
  echo "    8) SMTP relay  (Resend)"
  echo ""
  echo "  Performance"
  echo "    9) PHP-FPM profiles"
  echo "   10) OpCache"
  echo "   11) MariaDB"
  echo ""
  echo "  Nginx Templates"
  echo "   12) wp-secure  (security hardening)"
  echo "   13) wp-rocket  (WP Rocket cache proxy)"
  echo ""
  echo "  Maintenance"
  echo "   14) System updates / HestiaCP update / filemanager fix"
  echo "   15) Disk usage & cleanup / ncdu"
  echo ""
  echo "    0) Exit"
  echo ""
  echo "$DIV"
}

while true; do
  print_status
  show_menu
  read -r -p "  Select: " choice
  echo ""

  case "$choice" in
    1)  _maintenance_deploy ;;
    2)  menu_wpcli ;;
    3)  menu_redis ;;
    4)  menu_fail2ban ;;
    5)  menu_maldet ;;
    6)  menu_netdata ;;
    7)  menu_security ;;
    8)  menu_smtp ;;
    9)  menu_php_fpm ;;
    10) menu_opcache ;;
    11) menu_mariadb ;;
    12) _nginx_install_wpsecure ;;
    13) _nginx_install_wprocket ;;
    14) menu_maintenance ;;
    15) menu_disk ;;
    0)  echo "  Bye!"; exit 0 ;;
    *)  echo "  Invalid option."; sleep 1 ;;
  esac
done
