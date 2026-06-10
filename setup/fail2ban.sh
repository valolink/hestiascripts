#!/bin/bash
# Fail2ban — WordPress brute-force protection

_F2B_FILTER="/etc/fail2ban/filter.d/wordpress.conf"
_F2B_JAIL="/etc/fail2ban/jail.d/wordpress.conf"

menu_fail2ban() {
  while true; do
    clear
    echo ""
    echo -e "  ${BOLD}Fail2ban${NC}"
    echo "$DIV"

    if ! command -v fail2ban-client &>/dev/null; then
      status_line "Fail2ban" ERR "not installed"
    elif ! systemctl is-active --quiet fail2ban; then
      status_line "Fail2ban" WARN "installed but not running"
    else
      status_line "Fail2ban" OK "running"
      if fail2ban-client status wordpress &>/dev/null; then
        local banned
        banned=$(fail2ban-client status wordpress 2>/dev/null | grep "Currently banned" | awk '{print $NF}')
        sub_line "  WP jail" "active  (currently banned: ${banned:-0})"
      else
        sub_line "  WP jail" "❌ not configured"
      fi
    fi

    echo ""
    echo "  1) Configure WordPress login protection jail"
    echo "  2) Restart Fail2ban"
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice

    case "$choice" in
      1) _fail2ban_wp_jail ;;
      2) _fail2ban_restart ;;
      0) return ;;
    esac
  done
}

_fail2ban_restart() {
  if ! command -v fail2ban-client &>/dev/null; then
    echo "  Fail2ban is not installed."
    press_enter; return
  fi

  echo ""

  if ! _fail2ban_fix_config; then
    press_enter; return
  fi

  echo -e "  ${CYAN}→${NC} Restarting Fail2ban..."
  systemctl restart fail2ban

  if systemctl is-active --quiet fail2ban; then
    echo -e "  ${GREEN}✓ Fail2ban is running${NC}"
    echo ""
    fail2ban-client status 2>/dev/null | grep -E "Number of jail|Jail list" | sed 's/^/     /'
  else
    echo -e "  ${RED}✗ Failed to start — check: journalctl -u fail2ban${NC}"
  fi
  press_enter
}

# Test config and auto-disable any jails that reference missing log files.
# Returns 0 if config is valid (after fixes), 1 if there are other errors.
_fail2ban_fix_config() {
  local test_out exit_code

  test_out=$(fail2ban-client -t 2>&1)
  exit_code=$?

  [ $exit_code -eq 0 ] && return 0

  # Find jails with missing log files
  local bad_jails
  bad_jails=$(echo "$test_out" | grep -oP "(?<=any log file for )\S+(?= jail)")

  if [ -z "$bad_jails" ]; then
    echo -e "  ${RED}✗ Config error:${NC}"
    echo "$test_out" | grep -v "WARNING" | sed 's/^/     /'
    return 1
  fi

  # Disable each offending jail via a jail.d override — the standard way to
  # turn off jails that reference services not present on this server.
  while IFS= read -r jail; do
    local override="/etc/fail2ban/jail.d/disable-${jail}.conf"
    echo -e "  ${CYAN}→${NC} Disabling '$jail' jail  (log files not found on this server)"
    printf '[%s]\nenabled = false\n' "$jail" > "$override"
  done <<< "$bad_jails"

  # Re-test after fixes
  test_out=$(fail2ban-client -t 2>&1)
  if [ $? -ne 0 ]; then
    echo -e "  ${RED}✗ Config still failing after auto-fix:${NC}"
    echo "$test_out" | grep -v "WARNING" | sed 's/^/     /'
    return 1
  fi

  return 0
}

_fail2ban_wp_jail() {
  if ! command -v fail2ban-client &>/dev/null; then
    echo "  Fail2ban is not installed."
    echo "  HestiaCP ships with fail2ban — check if it was skipped during install."
    press_enter; return
  fi

  echo ""
  echo "  This will create:"
  echo "    $_F2B_FILTER  — matches POST requests to wp-login.php in nginx logs"
  echo "    $_F2B_JAIL    — 5 attempts in 60s = 1h ban, watches /var/log/nginx/domains/*.log"
  echo ""

  if [ -f "$_F2B_FILTER" ] || [ -f "$_F2B_JAIL" ]; then
    echo "  ⚠️  Files already exist. Overwrite?"
    if ! confirm "Overwrite?"; then echo "  Cancelled."; press_enter; return; fi
  fi

  if ! confirm "Create WordPress fail2ban jail?"; then echo "  Cancelled."; return; fi

  cat > "$_F2B_FILTER" <<'EOF'
[Definition]
allowipv6 = auto
failregex = ^<HOST> .* "POST .*wp-login\.php
ignoreregex =
EOF

  cat > "$_F2B_JAIL" <<'EOF'
[wordpress]
enabled  = true
filter   = wordpress
logpath  = /var/log/nginx/domains/*.log
backend  = polling
maxretry = 5
findtime = 60
bantime  = 3600
EOF

  echo ""

  # fail2ban hard-fails if the glob matches zero files, even with backend=polling.
  # A permanent placeholder satisfies this; it stays empty so no false bans occur.
  # Real domain log files are picked up by the glob as nginx creates them.
  local log_dir="/var/log/nginx/domains"
  mkdir -p "$log_dir"
  touch "$log_dir/wordpress-watch.log"
  echo -e "  ${CYAN}→${NC} Ensured $log_dir/wordpress-watch.log exists"

  echo -e "  ${CYAN}→${NC} Testing config"
  if ! _fail2ban_fix_config; then
    press_enter; return
  fi

  echo -e "  ${CYAN}→${NC} Reloading Fail2ban"
  fail2ban-client reload &>/dev/null
  if fail2ban-client status wordpress &>/dev/null; then
    echo -e "  ${GREEN}✓ WordPress jail active${NC}"
  else
    echo -e "  ${RED}✗ Jail not active after reload — check: fail2ban-client status${NC}"
  fi
  press_enter
}
