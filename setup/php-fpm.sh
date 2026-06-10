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
    echo "  3) Install a PHP version  (MultiPHP)"
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice

    case "$choice" in
      1) _fpm_install_profile ;;
      2) _fpm_show_profiles ;;
      3) _php_install_version ;;
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

# --- MultiPHP install ---

_PHP_ALL_VERSIONS=("7.4" "8.0" "8.1" "8.2" "8.3" "8.4")

_php_version_installed() {
  [ -d "/etc/php/$1/fpm" ]
}

_php_ensure_sury_repo() {
  if grep -rq "sury.org/php\|deb.sury.org" /etc/apt/sources.list /etc/apt/sources.list.d/ 2>/dev/null; then
    return 0
  fi
  echo -e "  ${CYAN}→${NC} Adding packages.sury.org/php repository..."
  local codename; codename=$(lsb_release -sc 2>/dev/null || echo "bookworm")
  curl -fsSL https://packages.sury.org/php/apt.gpg \
    | gpg --dearmor -o /usr/share/keyrings/php-sury.gpg
  echo "deb [signed-by=/usr/share/keyrings/php-sury.gpg] https://packages.sury.org/php/ ${codename} main" \
    > /etc/apt/sources.list.d/php-sury.list
  apt update -q
}

_php_install_version() {
  clear
  echo ""
  echo -e "  ${BOLD}Install PHP Version${NC}"
  echo "$DIV"
  echo ""

  local i=1
  local available=()
  for ver in "${_PHP_ALL_VERSIONS[@]}"; do
    if _php_version_installed "$ver"; then
      printf "  %s) PHP %s  ✅ installed\n" "$i" "$ver"
    else
      printf "  %s) PHP %s\n" "$i" "$ver"
      available+=("$ver")
    fi
    ((i++))
  done

  if [ ${#available[@]} -eq 0 ]; then
    echo ""
    echo "  All PHP versions are already installed."
    press_enter; return
  fi

  echo ""
  read -r -p "  Select version number to install: " idx
  local ver="${_PHP_ALL_VERSIONS[$((idx-1))]}"
  if [ -z "$ver" ]; then echo "  Invalid."; press_enter; return; fi
  if _php_version_installed "$ver"; then
    echo "  PHP $ver is already installed."; press_enter; return
  fi

  local packages=(
    php${ver}-fpm php${ver}-cli php${ver}-common
    php${ver}-mysql php${ver}-xml php${ver}-mbstring
    php${ver}-curl php${ver}-zip php${ver}-intl
    php${ver}-gd php${ver}-bcmath php${ver}-soap
    php${ver}-redis
  )

  echo ""
  echo "  Will install PHP ${ver} with WordPress extensions:"
  echo "  ${packages[*]}"
  echo "  php-imagick (shared, version-agnostic)"
  echo ""
  echo "  HestiaCP will auto-detect the new version after install."
  echo ""

  if ! confirm "Install PHP ${ver}?"; then return; fi

  echo ""
  if ! _php_ensure_sury_repo; then
    echo -e "  ${RED}✗ Failed to add repo${NC}"; press_enter; return
  fi

  echo -e "  ${CYAN}→${NC} Installing packages..."
  if ! apt install -y "${packages[@]}"; then
    echo -e "  ${RED}✗ Install failed — check output above${NC}"; press_enter; return
  fi

  # imagick is version-agnostic on some distros
  apt install -y php-imagick 2>/dev/null || true

  echo -e "  ${CYAN}→${NC} Enabling PHP ${ver}-FPM..."
  systemctl enable php${ver}-fpm
  systemctl start php${ver}-fpm

  echo -e "  ${CYAN}→${NC} Restarting HestiaCP to pick up new PHP version..."
  systemctl restart hestia

  echo ""
  echo -e "  ${GREEN}✓ PHP ${ver} installed${NC}"
  echo ""
  echo "  Verify in HestiaCP: Web → Edit domain → Advanced → PHP version"
  echo "  Then use option 1 to install PHP-FPM profiles for PHP ${ver}."
  press_enter
}
