#!/bin/bash
# Status dashboard — sourced by hestia-setup.sh

_status_streamer() {
  if systemctl is-active --quiet hestia-streamer 2>/dev/null; then
    status_line "Streamer" OK "running  port 8091"
  else
    status_line "Streamer" ERR "not running — choose option 1 to deploy"
  fi
}

_status_vscripts() {
  local count=0
  while IFS= read -r link; do
    local target
    target=$(readlink -f "$link" 2>/dev/null)
    [[ "$target" == "$SCRIPT_DIR"* ]] && ((count++))
  done < <(find /usr/local/hestia/bin/ -maxdepth 1 -name "v-*" -type l 2>/dev/null)
  if [ "$count" -gt 0 ]; then
    status_line "v-scripts" OK "$count linked"
  else
    status_line "v-scripts" ERR "none linked — choose option 1 to deploy"
  fi
}

_status_wpcli() {
  if command -v wp &>/dev/null; then
    local ver
    ver=$(wp --version --allow-root 2>/dev/null | awk '{print $2}')
    status_line "WP-CLI" OK "$ver"
  else
    status_line "WP-CLI" ERR "not installed"
  fi
}

_status_redis() {
  if ! command -v redis-server &>/dev/null; then
    status_line "Redis" ERR "not installed"
    return
  fi
  if ! systemctl is-active --quiet redis-server 2>/dev/null; then
    status_line "Redis" WARN "installed but not running"
    return
  fi

  local maxmem maxmem_h policy
  maxmem=$(redis-cli config get maxmemory 2>/dev/null | tail -1)
  policy=$(redis-cli config get maxmemory-policy 2>/dev/null | tail -1)

  if [ "${maxmem:-0}" -gt 0 ] 2>/dev/null; then
    maxmem_h=$(bytes_to_human "$maxmem")
    status_line "Redis" OK "running  ${maxmem_h}  ${policy}"
  else
    status_line "Redis" WARN "running — maxmemory not set"
  fi

  # PHP redis extensions per installed version
  local ext_parts=""
  for ver in $(get_php_versions); do
    if php${ver} -r "echo extension_loaded('redis') ? 1 : 0;" 2>/dev/null | grep -q "^1$"; then
      ext_parts+="${ver} ✅  "
    else
      ext_parts+="${ver} ❌  "
    fi
  done
  [ -n "$ext_parts" ] && sub_line "  PHP redis ext" "$ext_parts"
}

_status_fail2ban() {
  if ! command -v fail2ban-client &>/dev/null; then
    status_line "Fail2ban" ERR "not installed"
    return
  fi
  if ! systemctl is-active --quiet fail2ban 2>/dev/null; then
    status_line "Fail2ban" WARN "installed but not running"
    return
  fi
  if fail2ban-client status wordpress &>/dev/null; then
    status_line "Fail2ban" OK "running  WP jail active"
  else
    status_line "Fail2ban" WARN "running — no WordPress jail"
  fi
}

_status_maldet() {
  if [ ! -f /usr/local/maldetect/maldet ]; then
    status_line "Maldet" ERR "not installed"
    return
  fi
  local last_scan
  last_scan=$(grep "scan completed" /usr/local/maldetect/logs/event_log 2>/dev/null | tail -1 | awk '{print $1, $2}')
  if [ -n "$last_scan" ]; then
    status_line "Maldet" OK "last scan: $last_scan"
  else
    status_line "Maldet" WARN "installed — no scan on record"
  fi
}

_status_smtp() {
  local relay
  relay=$(postconf -h relayhost 2>/dev/null)
  if echo "$relay" | grep -q "smtp.resend.com"; then
    status_line "SMTP relay" OK "Resend"
  elif [ -n "$relay" ] && [ "$relay" != "[]" ] && [ "$relay" != "" ]; then
    status_line "SMTP relay" WARN "$relay"
  else
    status_line "SMTP relay" ERR "no relay — outbound mail may not work"
  fi
}

_status_netdata() {
  if ! command -v netdata &>/dev/null; then
    status_line "Netdata" ERR "not installed"
  elif systemctl is-active --quiet netdata 2>/dev/null; then
    status_line "Netdata" OK "running  port 19999"
  else
    status_line "Netdata" WARN "installed but not running"
  fi
}

_status_disk() {
  local used_pct info
  used_pct=$(df / | awk 'NR==2 {gsub(/%/,"",$5); print $5}')
  info=$(df -h / | awk 'NR==2 {print $3 " / " $2 " (" $5 ")"}')
  if [ "${used_pct:-0}" -ge 90 ]; then
    status_line "Disk (/)" ERR "$info"
  elif [ "${used_pct:-0}" -ge 75 ]; then
    status_line "Disk (/)" WARN "$info"
  else
    status_line "Disk (/)" OK "$info"
  fi
}

_status_swap() {
  local swap_info
  swap_info=$(swapon --show --noheadings 2>/dev/null | awk '{print $3}')
  if [ -n "$swap_info" ]; then
    status_line "Swap" OK "$swap_info"
  else
    local ram_mb suggested
    ram_mb=$(get_ram_mb)
    suggested=$([ "$ram_mb" -le 4096 ] && echo "2G" || echo "1G")
    status_line "Swap" ERR "none  (recommended: ${suggested})"
  fi
}

_status_unattended() {
  if dpkg -l unattended-upgrades 2>/dev/null | grep -q "^ii" && \
     systemctl is-active --quiet unattended-upgrades 2>/dev/null; then
    status_line "Unattended upgrades" OK "enabled"
  elif dpkg -l unattended-upgrades 2>/dev/null | grep -q "^ii"; then
    status_line "Unattended upgrades" WARN "installed but service not active"
  else
    status_line "Unattended upgrades" ERR "not set up"
  fi
}

_status_opcache() {
  local any=false
  for ver in $(get_php_versions); do
    local ini="/etc/php/${ver}/fpm/php.ini"
    [ -f "$ini" ] || continue
    any=true
    local enabled mem
    enabled=$(grep -oP "(?<=opcache\.enable=)\d" "$ini" 2>/dev/null | head -1)
    mem=$(grep -oP "(?<=opcache\.memory_consumption=)\d+" "$ini" 2>/dev/null | head -1)
    if [ "${enabled:-0}" = "1" ] && [ "${mem:-0}" -ge 256 ] 2>/dev/null; then
      status_line "  OpCache ${ver}" OK "${mem}MB"
    elif [ "${enabled:-0}" = "1" ]; then
      status_line "  OpCache ${ver}" WARN "enabled but memory low (${mem:-?}MB)"
    else
      status_line "  OpCache ${ver}" ERR "disabled"
    fi
  done
  $any || status_line "OpCache" ERR "no PHP versions found"
}

_status_mariadb() {
  local cnf="/etc/mysql/mariadb.conf.d/50-server.cnf"
  if [ ! -f "$cnf" ]; then
    status_line "MariaDB buffer" ERR "config file not found"
    return
  fi
  local val
  val=$(grep -oP "(?<=innodb_buffer_pool_size\s=\s)\S+" "$cnf" 2>/dev/null | head -1)
  if [ -n "$val" ]; then
    local ram_mb recommended
    ram_mb=$(get_ram_mb)
    recommended=$(echo "$ram_mb * 0.5 / 1024" | awk '{printf "%dG", $1}')
    status_line "MariaDB buffer" OK "${val}  (RAM: $((ram_mb/1024))G, recommended ~${recommended})"
  else
    status_line "MariaDB buffer" WARN "innodb_buffer_pool_size not set"
  fi
}

_status_fpm_templates() {
  local tpl_dir="/usr/local/hestia/data/templates/web/php-fpm"
  local profiles=("production" "standard" "staging" "small")
  for ver in $(get_php_versions); do
    local ver_tag="${ver//./_}"
    local found=()
    local missing=()
    for p in "${profiles[@]}"; do
      if [ -f "$tpl_dir/${p}-PHP-${ver_tag}.tpl" ]; then
        found+=("$p")
      else
        missing+=("$p")
      fi
    done
    local detail=""
    for p in "${found[@]}";   do detail+="${p} ✅  "; done
    for p in "${missing[@]}"; do detail+="${p} ❌  "; done
    if [ ${#found[@]} -gt 0 ]; then
      status_line "  PHP-FPM ${ver}" OK "$detail"
    else
      status_line "  PHP-FPM ${ver}" ERR "no profiles installed"
    fi
  done
}

_status_nginx_templates() {
  local tpl_dir="/usr/local/hestia/data/templates/web/nginx/php-fpm"
  for name in "wp-secure" "wp-rocket"; do
    if [ -f "$tpl_dir/${name}.tpl" ] && [ -f "$tpl_dir/${name}.stpl" ]; then
      status_line "  nginx ${name}" OK "installed"
    else
      status_line "  nginx ${name}" ERR "not installed"
    fi
  done
}

_status_hestia() {
  local installed latest
  installed=$(grep -oP "(?<=VERSION=')[^']+" /usr/local/hestia/conf/hestia.conf 2>/dev/null | head -1)
  if [ -z "$installed" ]; then
    status_line "HestiaCP" ERR "could not determine version"
    return
  fi
  latest=$(curl -sf --max-time 4 "https://api.github.com/repos/hestiacp/hestiacp/releases/latest" \
    | grep '"tag_name"' | cut -d'"' -f4 | tr -d 'v')
  if [ -n "$latest" ] && [ "$installed" != "$latest" ]; then
    status_line "HestiaCP" WARN "installed: ${installed}  latest: ${latest}"
  else
    status_line "HestiaCP" OK "${installed}${latest:+  (up to date)}"
  fi
}

print_status() {
  clear
  echo ""
  echo -e "  ${BOLD}=== HestiaCP Server Setup — $(hostname) ===${NC}"
  echo ""

  section "Deployed"
  _status_streamer
  _status_vscripts

  section "Core Tools"
  _status_disk
  _status_wpcli
  _status_redis
  _status_fail2ban
  _status_maldet
  _status_smtp
  _status_netdata
  _status_swap
  _status_unattended

  section "Performance"
  _status_opcache
  _status_mariadb
  _status_fpm_templates

  section "Nginx Templates"
  _status_nginx_templates

  section "HestiaCP"
  _status_hestia

  echo ""
  echo "$DIV"
}
