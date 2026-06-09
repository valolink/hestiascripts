#!/bin/bash
# Redis install, configuration, PHP extension management

menu_redis() {
  while true; do
    clear
    echo ""
    echo -e "  ${BOLD}Redis${NC}"
    echo "$DIV"

    if command -v redis-server &>/dev/null; then
      if systemctl is-active --quiet redis-server; then
        local maxmem policy
        maxmem=$(redis-cli config get maxmemory 2>/dev/null | tail -1)
        policy=$(redis-cli config get maxmemory-policy 2>/dev/null | tail -1)
        local maxmem_h="${maxmem:-0}"
        [ "$maxmem_h" -gt 0 ] 2>/dev/null && maxmem_h=$(bytes_to_human "$maxmem") || maxmem_h="not set ⚠️"
        status_line "Redis" OK "running"
        sub_line "  maxmemory" "$maxmem_h"
        sub_line "  maxmemory-policy" "${policy:-not set ⚠️}"
      else
        status_line "Redis" WARN "installed but not running"
      fi
    else
      status_line "Redis" ERR "not installed"
    fi

    echo ""
    local ext_parts=""
    for ver in $(get_php_versions); do
      if php${ver} -r "echo extension_loaded('redis') ? 1 : 0;" 2>/dev/null | grep -q "^1$"; then
        ext_parts+="  ${ver} ✅"
      else
        ext_parts+="  ${ver} ❌"
      fi
    done
    [ -n "$ext_parts" ] && echo "  PHP redis extensions:$ext_parts"
    echo ""

    echo "  1) Install Redis"
    echo "  2) Configure maxmemory and policy"
    echo "  3) Install PHP redis extension for a specific version"
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice

    case "$choice" in
      1) _redis_install ;;
      2) _redis_configure_memory ;;
      3) _redis_install_php_ext ;;
      0) return ;;
    esac
  done
}

_redis_install() {
  run_action "Install Redis" \
    "apt update" \
    "apt install redis-server -y" \
    "systemctl enable redis-server" \
    "systemctl start redis-server" \
    "redis-cli ping"

  if [ $? -eq 0 ]; then
    echo "  Redis installed. Use option 2 to configure maxmemory."
    press_enter
  fi
}

_redis_configure_memory() {
  if ! command -v redis-server &>/dev/null; then
    echo "  Redis is not installed. Use option 1 first."; press_enter; return
  fi

  local ram_mb suggested_mb
  ram_mb=$(get_ram_mb)
  # Suggest ~15% of total RAM for Redis on a multi-site server
  suggested_mb=$(( ram_mb * 15 / 100 ))
  # Round to nearest 64MB
  suggested_mb=$(( (suggested_mb / 64) * 64 ))
  [ "$suggested_mb" -lt 256 ] && suggested_mb=256

  echo ""
  echo "  Server RAM: ${ram_mb}MB"
  echo "  Suggested maxmemory: ${suggested_mb}mb  (based on ~15% of total RAM)"
  echo "  For a single large WooCommerce site: 256mb is fine."
  echo "  For multiple stores: 512mb or more is recommended."
  echo ""
  read -r -p "  Enter maxmemory in mb [${suggested_mb}]: " input_mb
  local target_mb="${input_mb:-$suggested_mb}"

  run_action "Configure Redis maxmemory=${target_mb}mb, policy=allkeys-lru" \
    "redis-cli config set maxmemory ${target_mb}mb" \
    "redis-cli config set maxmemory-policy allkeys-lru" \
    "sed -i 's/^#*\s*maxmemory .*/maxmemory ${target_mb}mb/' /etc/redis/redis.conf" \
    "sed -i 's/^#*\s*maxmemory-policy .*/maxmemory-policy allkeys-lru/' /etc/redis/redis.conf" \
    "systemctl restart redis-server"
}

_redis_install_php_ext() {
  local versions=()
  mapfile -t versions < <(get_php_versions)
  if [ ${#versions[@]} -eq 0 ]; then
    echo "  No PHP versions found."; press_enter; return
  fi

  echo ""
  echo "  Installed PHP versions:"
  local i=1
  for ver in "${versions[@]}"; do
    local installed=""
    php${ver} -r "echo extension_loaded('redis') ? 1 : 0;" 2>/dev/null | grep -q "^1$" \
      && installed=" (already installed)" || installed=" (not installed)"
    echo "  $i) PHP $ver$installed"
    ((i++))
  done
  echo ""
  read -r -p "  Select PHP version: " idx
  local ver="${versions[$((idx-1))]}"
  if [ -z "$ver" ]; then echo "  Invalid selection."; press_enter; return; fi

  run_action "Install php${ver}-redis" \
    "apt install php${ver}-redis -y" \
    "systemctl restart php${ver}-fpm"
}
