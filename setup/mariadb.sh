#!/bin/bash
# MariaDB tuning

_MARIADB_CNF="/etc/mysql/mariadb.conf.d/50-server.cnf"

menu_mariadb() {
  while true; do
    clear
    echo ""
    echo -e "  ${BOLD}MariaDB / MySQL${NC}"
    echo "$DIV"

    local current ram_mb recommended
    current=$(grep -oP "(?<=innodb_buffer_pool_size\s=\s)\S+" "$_MARIADB_CNF" 2>/dev/null | head -1)
    ram_mb=$(get_ram_mb)
    recommended=$(awk "BEGIN {printf \"%dG\", int($ram_mb * 0.5 / 1024)}")

    if [ -n "$current" ]; then
      status_line "Buffer pool size" OK "$current"
    else
      status_line "Buffer pool size" WARN "not set in $_MARIADB_CNF"
    fi
    sub_line "  Server RAM" "$((ram_mb/1024))G  →  recommended buffer: ~$recommended (50% of RAM)"

    echo ""
    echo "  1) Set innodb_buffer_pool_size"
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice

    case "$choice" in
      1) _mariadb_set_buffer ;;
      0) return ;;
    esac
  done
}

_mariadb_set_buffer() {
  if [ ! -f "$_MARIADB_CNF" ]; then
    echo "  Config file not found: $_MARIADB_CNF"; press_enter; return
  fi

  local ram_mb recommended
  ram_mb=$(get_ram_mb)
  recommended=$(awk "BEGIN {printf \"%dG\", int($ram_mb * 0.5 / 1024)}")

  echo ""
  echo "  Server RAM: $((ram_mb/1024))G"
  echo "  Recommended: $recommended  (50% of RAM — adjust for RAM shared with Redis, PHP workers, etc.)"
  echo ""
  read -r -p "  innodb_buffer_pool_size [$recommended]: " size
  size="${size:-$recommended}"

  if grep -q "innodb_buffer_pool_size" "$_MARIADB_CNF"; then
    run_action "Update innodb_buffer_pool_size = $size" \
      "sed -i 's/^#*\s*innodb_buffer_pool_size.*/innodb_buffer_pool_size = $size/' $_MARIADB_CNF" \
      "systemctl restart mariadb"
  else
    run_action "Add innodb_buffer_pool_size = $size" \
      "sed -i '/^\[mysqld\]/a innodb_buffer_pool_size = $size' $_MARIADB_CNF" \
      "systemctl restart mariadb"
  fi
}
