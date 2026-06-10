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
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice

    case "$choice" in
      1) _fail2ban_wp_jail ;;
      0) return ;;
    esac
  done
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

  # fail2ban refuses to reload if the glob matches no files at all
  local log_dir="/var/log/nginx/domains"
  if ! ls "$log_dir"/*.log &>/dev/null; then
    echo -e "  ${YELLOW}⚠${NC}  Config written but not loaded yet."
    echo "  No nginx domain log files found in $log_dir/"
    echo "  fail2ban requires at least one matching log file to exist."
    echo ""
    echo "  The WordPress jail will become active automatically the next"
    echo "  time fail2ban restarts (e.g. after a server reboot), or you"
    echo "  can reload it manually once domains have been added:"
    echo "    fail2ban-client reload"
    press_enter; return
  fi

  echo -e "  ${CYAN}→${NC} Testing config"
  local test_out
  test_out=$(fail2ban-client -t 2>&1)
  if [ $? -ne 0 ]; then
    echo -e "  ${RED}✗ Config error — not reloading:${NC}"
    echo "$test_out" | sed 's/^/     /'
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
