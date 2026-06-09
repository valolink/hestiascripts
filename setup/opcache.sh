#!/bin/bash
# OpCache tuning

menu_opcache() {
  while true; do
    clear
    echo ""
    echo -e "  ${BOLD}OpCache${NC}"
    echo "$DIV"

    local versions=()
    mapfile -t versions < <(get_php_versions)

    for ver in "${versions[@]}"; do
      local ini="/etc/php/${ver}/fpm/php.ini"
      [ -f "$ini" ] || continue
      local enabled mem files
      enabled=$(grep -oP "(?<=^opcache\.enable=)\d" "$ini" 2>/dev/null | head -1)
      mem=$(grep -oP "(?<=^opcache\.memory_consumption=)\d+" "$ini" 2>/dev/null | head -1)
      files=$(grep -oP "(?<=^opcache\.max_accelerated_files=)\d+" "$ini" 2>/dev/null | head -1)

      if [ "${enabled:-0}" = "1" ] && [ "${mem:-0}" -ge 256 ] 2>/dev/null; then
        status_line "PHP $ver" OK "enabled  ${mem}MB  files=${files:-?}"
      elif [ "${enabled:-0}" = "1" ]; then
        status_line "PHP $ver" WARN "enabled but memory ${mem:-?}MB (recommended ≥256)"
      else
        status_line "PHP $ver" ERR "disabled"
      fi
    done

    echo ""
    echo "  Recommended: enable=1, memory=512, max_files=50000, validate_timestamps=1"
    echo ""
    echo "  1) Apply recommended settings to a PHP version"
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice

    case "$choice" in
      1) _opcache_configure ;;
      0) return ;;
    esac
  done
}

_opcache_configure() {
  local versions=()
  mapfile -t versions < <(get_php_versions)

  echo ""
  echo "  Select PHP version:"
  local i=1
  for ver in "${versions[@]}"; do echo "  $i) PHP $ver"; ((i++)); done
  echo ""
  read -r -p "  Version: " idx
  local ver="${versions[$((idx-1))]}"
  if [ -z "$ver" ]; then echo "  Invalid selection."; press_enter; return; fi

  local ini="/etc/php/${ver}/fpm/php.ini"
  if [ ! -f "$ini" ]; then
    echo "  $ini not found."; press_enter; return
  fi

  local ram_mb mem_suggestion
  ram_mb=$(get_ram_mb)
  mem_suggestion=$([ "$ram_mb" -ge 8192 ] && echo "512" || echo "256")

  echo ""
  read -r -p "  opcache.memory_consumption [$mem_suggestion]: " mem
  mem="${mem:-$mem_suggestion}"

  run_action "Apply OpCache settings to PHP $ver (fpm)" \
    "sed -i 's/^;*opcache\.enable=.*/opcache.enable=1/' $ini" \
    "sed -i 's/^;*opcache\.memory_consumption=.*/opcache.memory_consumption=$mem/' $ini" \
    "sed -i 's/^;*opcache\.max_accelerated_files=.*/opcache.max_accelerated_files=50000/' $ini" \
    "sed -i 's/^;*opcache\.validate_timestamps=.*/opcache.validate_timestamps=1/' $ini" \
    "systemctl restart php${ver}-fpm"
}
