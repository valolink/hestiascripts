#!/bin/bash
# Status dashboard — sourced by hestia-setup.sh

print_status() {
  clear
  echo ""
  echo -e "  ${BOLD}=== HestiaCP Server Setup — $(hostname) ===${NC}"
  echo ""

  # --- Install & Configure ---
  section "Install & Configure"

  # 1) Deploy
  if systemctl is-active --quiet hestia-streamer 2>/dev/null; then
    local vcount=0
    while IFS= read -r link; do
      local target; target=$(readlink -f "$link" 2>/dev/null)
      [[ "$target" == "$SCRIPT_DIR"* ]] && ((vcount++))
    done < <(find /usr/local/hestia/bin/ -maxdepth 1 -name "v-*" -type l 2>/dev/null)
    menu_status_line 1 "Deploy" OK "streamer running, ${vcount} v-scripts"
  else
    menu_status_line 1 "Deploy" ERR "streamer not running"
  fi

  # 2) WP-CLI
  if command -v wp &>/dev/null; then
    local wpver; wpver=$(wp --version --allow-root 2>/dev/null | awk '{print $2}')
    menu_status_line 2 "WP-CLI" OK "$wpver"
  else
    menu_status_line 2 "WP-CLI" ERR "not installed"
  fi

  # 3) Redis
  if ! command -v redis-server &>/dev/null; then
    menu_status_line 3 "Redis" ERR "not installed"
  elif ! systemctl is-active --quiet redis-server 2>/dev/null; then
    menu_status_line 3 "Redis" WARN "installed but not running"
  else
    local rmaxmem rpolicy rmaxmem_h
    rmaxmem=$(redis-cli config get maxmemory 2>/dev/null | tail -1)
    rpolicy=$(redis-cli config get maxmemory-policy 2>/dev/null | tail -1)
    if [ "${rmaxmem:-0}" -gt 0 ] 2>/dev/null; then
      rmaxmem_h=$(bytes_to_human "$rmaxmem")
      menu_status_line 3 "Redis" OK "running  ${rmaxmem_h}  ${rpolicy}"
    else
      menu_status_line 3 "Redis" WARN "running — maxmemory not set"
    fi
    local ext_parts=""
    for ver in $(get_php_versions); do
      if php${ver} -r "echo extension_loaded('redis') ? 1 : 0;" 2>/dev/null | grep -q "^1$"; then
        ext_parts+="${ver} ✅  "
      else
        ext_parts+="${ver} ❌  "
      fi
    done
    [ -n "$ext_parts" ] && sub_line "     PHP redis ext" "$ext_parts"
  fi

  # 4) Fail2ban
  if ! command -v fail2ban-client &>/dev/null; then
    menu_status_line 4 "Fail2ban" ERR "not installed"
  elif ! systemctl is-active --quiet fail2ban 2>/dev/null; then
    menu_status_line 4 "Fail2ban" WARN "installed but not running"
  elif fail2ban-client status wordpress &>/dev/null; then
    menu_status_line 4 "Fail2ban" OK "running  WP jail active"
  else
    menu_status_line 4 "Fail2ban" WARN "running — no WordPress jail"
  fi

  # 5) Maldet
  if [ ! -f /usr/local/maldetect/maldet ]; then
    menu_status_line 5 "Maldet" ERR "not installed"
  else
    local last_scan
    last_scan=$(grep "scan completed" /usr/local/maldetect/logs/event_log 2>/dev/null | tail -1 | awk '{print $1, $2}')
    if [ -n "$last_scan" ]; then
      menu_status_line 5 "Maldet" OK "last scan: $last_scan"
    else
      menu_status_line 5 "Maldet" WARN "installed — no scan on record"
    fi
  fi

  # 6) Netdata
  if ! command -v netdata &>/dev/null; then
    menu_status_line 6 "Netdata" ERR "not installed"
  elif systemctl is-active --quiet netdata 2>/dev/null; then
    menu_status_line 6 "Netdata" OK "running  port 19999"
  else
    menu_status_line 6 "Netdata" WARN "installed but not running"
  fi

  # 7) Security (swap + SSH + unattended upgrades summary)
  local ssh_pw; ssh_pw=$(grep "^PasswordAuthentication" /etc/ssh/sshd_config 2>/dev/null | awk '{print $2}')
  local swap_ok; swap_ok=$(swapon --show --noheadings 2>/dev/null | awk '{print $3}')
  local uu_installed; uu_installed=$(dpkg -l unattended-upgrades 2>/dev/null | grep -c "^ii")
  local uu_scope; uu_scope=$(_uu_scope)
  local sec_issues=0 sec_level="OK"
  [ -z "$swap_ok" ]      && { ((sec_issues++)); sec_level="WARN"; }
  [ "$ssh_pw" != "no" ]  && { ((sec_issues++)); sec_level="WARN"; }
  [ "$uu_installed" -eq 0 ] && { ((sec_issues++)); sec_level="ERR"; }
  [ "$uu_scope" = "all" ]   && { ((sec_issues++)); sec_level="ERR"; }
  local sec_detail=""
  [ -n "$swap_ok" ]         && sec_detail+="swap ✓  "    || sec_detail+="swap ✗  "
  [ "$ssh_pw" = "no" ]      && sec_detail+="SSH key-only ✓  " || sec_detail+="SSH pw ⚠  "
  if [ "$uu_installed" -eq 0 ]; then
    sec_detail+="auto-updates ✗"
  elif [ "$uu_scope" = "all" ]; then
    sec_detail+="auto-updates: all pkgs ✗"
  else
    sec_detail+="auto-updates ✓"
  fi
  menu_status_line 7 "Security" "$sec_level" "$sec_detail"

  # 8) SMTP
  local relay; relay=$(postconf -h relayhost 2>/dev/null)
  if echo "$relay" | grep -q "smtp.resend.com"; then
    menu_status_line 8 "SMTP relay" OK "Resend"
  elif [ -n "$relay" ] && [ "$relay" != "[]" ] && [ "$relay" != "" ]; then
    menu_status_line 8 "SMTP relay" WARN "$relay"
  else
    menu_status_line 8 "SMTP relay" ERR "no relay configured"
  fi

  # --- Performance ---
  section "Performance"

  # 9) PHP-FPM
  local fpm_ok=0 fpm_err=0
  local tpl_dir="/usr/local/hestia/data/templates/web/php-fpm"
  local profiles=("production" "standard" "staging" "small")
  for ver in $(get_php_versions); do
    local ver_tag="${ver//./_}"
    for p in "${profiles[@]}"; do
      if [ -f "$tpl_dir/${p}-PHP-${ver_tag}.tpl" ]; then ((fpm_ok++)); else ((fpm_err++)); fi
    done
  done
  if [ "$fpm_ok" -eq 0 ] && [ "$fpm_err" -eq 0 ]; then
    menu_status_line 9 "PHP-FPM profiles" ERR "no PHP versions found"
  elif [ "$fpm_err" -eq 0 ]; then
    menu_status_line 9 "PHP-FPM profiles" OK "${fpm_ok} profiles installed"
  elif [ "$fpm_ok" -eq 0 ]; then
    menu_status_line 9 "PHP-FPM profiles" ERR "no profiles installed"
  else
    menu_status_line 9 "PHP-FPM profiles" WARN "${fpm_ok} installed, ${fpm_err} missing"
  fi

  # 10) OpCache (per PHP version as sub-lines)
  local oc_any=false oc_ok=0 oc_err=0
  for ver in $(get_php_versions); do
    local ini="/etc/php/${ver}/fpm/php.ini"
    [ -f "$ini" ] || continue
    oc_any=true
    local enabled mem
    enabled=$(grep -oP "(?<=opcache\.enable=)\d" "$ini" 2>/dev/null | head -1)
    mem=$(grep -oP "(?<=opcache\.memory_consumption=)\d+" "$ini" 2>/dev/null | head -1)
    if [ "${enabled:-0}" = "1" ] && [ "${mem:-0}" -ge 256 ] 2>/dev/null; then ((oc_ok++)); else ((oc_err++)); fi
  done
  if ! $oc_any; then
    menu_status_line 10 "OpCache" ERR "no PHP versions found"
  elif [ "$oc_err" -eq 0 ]; then
    menu_status_line 10 "OpCache" OK "${oc_ok} versions configured"
  elif [ "$oc_ok" -eq 0 ]; then
    menu_status_line 10 "OpCache" ERR "not configured"
  else
    menu_status_line 10 "OpCache" WARN "${oc_ok} ok, ${oc_err} need attention"
  fi

  # 11) MariaDB
  local cnf="/etc/mysql/mariadb.conf.d/50-server.cnf"
  if [ ! -f "$cnf" ]; then
    menu_status_line 11 "MariaDB" ERR "config file not found"
  else
    local bval; bval=$(grep -oP "(?<=innodb_buffer_pool_size\s=\s)\S+" "$cnf" 2>/dev/null | head -1)
    if [ -n "$bval" ]; then
      menu_status_line 11 "MariaDB" OK "buffer pool: ${bval}"
    else
      menu_status_line 11 "MariaDB" WARN "buffer pool not set"
    fi
  fi

  # --- Nginx Templates ---
  section "Nginx Templates"
  local ntpl_dir="/usr/local/hestia/data/templates/web/nginx/php-fpm"

  if [ -f "$ntpl_dir/wp-secure.tpl" ] && [ -f "$ntpl_dir/wp-secure.stpl" ]; then
    menu_status_line 12 "wp-secure" OK "installed"
  else
    menu_status_line 12 "wp-secure" ERR "not installed"
  fi

  if [ -f "$ntpl_dir/wp-rocket.tpl" ] && [ -f "$ntpl_dir/wp-rocket.stpl" ]; then
    menu_status_line 13 "wp-rocket" OK "installed"
  else
    menu_status_line 13 "wp-rocket" ERR "not installed"
  fi

  # --- Maintenance ---
  section "Maintenance"

  # 14) System updates / HestiaCP
  local installed latest hestia_ok=true
  installed=$(grep -oP "(?<=VERSION=')[^']+" /usr/local/hestia/conf/hestia.conf 2>/dev/null | head -1)
  if [ -z "$installed" ]; then
    menu_status_line 14 "System updates" ERR "HestiaCP version unknown"
  else
    latest=$(curl -sf --max-time 4 "https://api.github.com/repos/hestiacp/hestiacp/releases/latest" \
      | grep '"tag_name"' | cut -d'"' -f4 | tr -d 'v')
    if [ -n "$latest" ] && [ "$installed" != "$latest" ]; then
      menu_status_line 14 "System updates" WARN "HestiaCP ${installed} → ${latest} available"
    else
      menu_status_line 14 "System updates" OK "HestiaCP ${installed}${latest:+  (up to date)}"
    fi
  fi

  # 15) Disk
  local used_pct disk_info
  used_pct=$(df / | awk 'NR==2 {gsub(/%/,"",$5); print $5}')
  disk_info=$(df -h / | awk 'NR==2 {print $3 " / " $2 " (" $5 ")"}')
  if [ "${used_pct:-0}" -ge 90 ]; then
    menu_status_line 15 "Disk" ERR "$disk_info"
  elif [ "${used_pct:-0}" -ge 75 ]; then
    menu_status_line 15 "Disk" WARN "$disk_info"
  else
    menu_status_line 15 "Disk" OK "$disk_info"
  fi

  echo ""
  echo "   0) Exit"
  echo ""
  echo "$DIV"
}
