#!/bin/bash
# PHP-FPM profile template management

_HESTIA_FPM_DIR="/usr/local/hestia/data/templates/web/php-fpm"
_PROFILES=("production" "standard" "staging" "small")

menu_php_fpm() {
  while true; do
    clear
    echo ""
    echo -e "  ${BOLD}PHP-FPM Profiles${NC}"
    echo "$DIV"

    local versions=()
    mapfile -t versions < <(get_php_versions)

    if [ ${#versions[@]} -eq 0 ]; then
      echo "  No PHP versions found in /etc/php/"
      press_enter; return
    fi

    for ver in "${versions[@]}"; do
      local ver_tag="${ver//./_}"
      local line=""
      for p in "${_PROFILES[@]}"; do
        [ -f "$_HESTIA_FPM_DIR/${p}-PHP-${ver_tag}.tpl" ] \
          && line+="${p} ✅  " \
          || line+="${p} ❌  "
      done
      sub_line "  PHP $ver" "$line"
    done

    echo ""
    echo "  1) Install a profile for a PHP version"
    echo "  2) Show profile details"
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice

    case "$choice" in
      1) _fpm_install_profile ;;
      2) _fpm_show_profiles ;;
      0) return ;;
    esac
  done
}

_fpm_install_profile() {
  local versions=()
  mapfile -t versions < <(get_php_versions)

  echo ""
  echo "  Select PHP version:"
  local i=1
  for ver in "${versions[@]}"; do echo "  $i) PHP $ver"; ((i++)); done
  echo ""
  read -r -p "  Version: " idx
  local ver="${versions[$((idx-1))]}"
  if [ -z "$ver" ]; then echo "  Invalid."; press_enter; return; fi

  echo ""
  echo "  Select profile:"
  i=1
  for p in "${_PROFILES[@]}"; do
    local conf="$SCRIPT_DIR/templates/php-fpm/${p}.conf"
    local desc=""
    [ -f "$conf" ] && desc=$(grep "^PROFILE_DESC=" "$conf" | cut -d'"' -f2)
    echo "  $i) $p — $desc"
    ((i++))
  done
  echo ""
  read -r -p "  Profile: " pidx
  local profile="${_PROFILES[$((pidx-1))]}"
  if [ -z "$profile" ]; then echo "  Invalid."; press_enter; return; fi

  _fpm_apply_profile "$ver" "$profile"
}

_fpm_apply_profile() {
  local ver="$1" profile="$2"
  local ver_tag="${ver//./_}"
  local src="$_HESTIA_FPM_DIR/PHP-${ver_tag}.tpl"
  local dst="$_HESTIA_FPM_DIR/${profile}-PHP-${ver_tag}.tpl"
  local conf="$SCRIPT_DIR/templates/php-fpm/${profile}.conf"

  if [ ! -f "$src" ]; then
    echo "  Base template not found: $src"
    echo "  Is PHP $ver installed in HestiaCP?"
    press_enter; return
  fi

  if [ ! -f "$conf" ]; then
    echo "  Profile config not found: $conf"; press_enter; return
  fi

  # Load profile variables
  source "$conf"

  echo ""
  echo "  Profile: $profile  |  PHP: $ver"
  echo "  pm = $PM_MODE  |  max_children = $PM_MAX_CHILDREN  |  max_requests = $PM_MAX_REQUESTS"
  [ "$PM_MODE" = "dynamic" ]  && echo "  start=$PM_START_SERVERS  min_spare=$PM_MIN_SPARE  max_spare=$PM_MAX_SPARE"
  [ "$PM_MODE" = "ondemand" ] && echo "  idle_timeout=$PM_IDLE_TIMEOUT"
  echo ""
  echo "  Will copy $src → $dst then apply profile settings."
  echo ""

  if ! confirm "Create ${profile}-PHP-${ver_tag}.tpl?"; then return; fi

  cp "$src" "$dst"

  # Apply pm settings via sed
  _fpm_sed_apply "$dst"

  echo -e "  ${GREEN}✓ Template created: $dst${NC}"
  echo "  Apply it to a domain in HestiaCP: Web → Edit domain → Advanced → Web Template (PHP-FPM)"
  press_enter
}

_fpm_sed_apply() {
  local tpl="$1"

  # Helper: set or add a pm key
  _set_pm() {
    local key="$1" val="$2"
    if grep -qP "^;*${key}" "$tpl"; then
      sed -i "s|^;*${key}.*|${key} = ${val}|" "$tpl"
    else
      sed -i "/^pm\.max_children/a ${key} = ${val}" "$tpl"
    fi
  }

  # Comment out a pm key
  _comment_pm() {
    sed -i "s|^${1} |;${1} |" "$tpl"
  }

  # Set pm mode
  sed -i "s|^;*pm = .*|pm = $PM_MODE|" "$tpl"

  _set_pm "pm.max_children"  "$PM_MAX_CHILDREN"
  _set_pm "pm.max_requests"  "$PM_MAX_REQUESTS"

  case "$PM_MODE" in
    static)
      _comment_pm "pm.start_servers"
      _comment_pm "pm.min_spare_servers"
      _comment_pm "pm.max_spare_servers"
      _comment_pm "pm.process_idle_timeout"
      ;;
    dynamic)
      _set_pm "pm.start_servers"       "$PM_START_SERVERS"
      _set_pm "pm.min_spare_servers"   "$PM_MIN_SPARE"
      _set_pm "pm.max_spare_servers"   "$PM_MAX_SPARE"
      _comment_pm "pm.process_idle_timeout"
      ;;
    ondemand)
      _set_pm "pm.process_idle_timeout" "$PM_IDLE_TIMEOUT"
      _comment_pm "pm.start_servers"
      _comment_pm "pm.min_spare_servers"
      _comment_pm "pm.max_spare_servers"
      ;;
  esac
}

_fpm_show_profiles() {
  clear
  echo ""
  echo -e "  ${BOLD}Profile Definitions${NC}"
  echo "$DIV"
  for p in "${_PROFILES[@]}"; do
    local conf="$SCRIPT_DIR/templates/php-fpm/${p}.conf"
    [ -f "$conf" ] || continue
    echo ""
    echo -e "  ${BOLD}$p${NC}"
    grep -v "^$" "$conf" | while IFS= read -r line; do echo "    $line"; done
  done
  echo ""
  press_enter
}
