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
failregex = ^<HOST> .* "POST .*wp-login\.php
ignoreregex =
EOF

  cat > "$_F2B_JAIL" <<'EOF'
[wordpress]
enabled  = true
filter   = wordpress
logpath  = /var/log/nginx/domains/*.log
maxretry = 5
findtime = 60
bantime  = 3600
EOF

  echo ""
  echo -e "  ${CYAN}→${NC} systemctl reload fail2ban"
  systemctl reload fail2ban
  if [ $? -eq 0 ]; then
    echo -e "  ${GREEN}✓ WordPress jail active${NC}"
  else
    echo -e "  ${RED}✗ fail2ban reload failed — check: fail2ban-client -t${NC}"
  fi
  press_enter
}
