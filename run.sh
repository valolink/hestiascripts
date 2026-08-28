#!/bin/bash

if [ "$EUID" -ne 0 ]; then
  echo "ERROR: Please run as root."
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export SCRIPT_DIR

# Source all modules
for mod in common status wpcli redis fail2ban maldet netdata security smtp \
           php-fpm opcache mariadb nginx-templates maintenance disk audit; do
  source "$SCRIPT_DIR/setup/${mod}.sh" || { echo "Failed to load setup/${mod}.sh"; exit 1; }
done

hestiascripts_check_version

while true; do
  print_status
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
    12) menu_nginx_templates ;;
    13) menu_maintenance ;;
    14) menu_disk ;;
    15) menu_audit ;;
    0)  echo "  Bye!"; exit 0 ;;
    *)  echo "  Invalid option."; sleep 1 ;;
  esac
done
