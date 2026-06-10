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
        if [ -f "$_F2B_JAIL" ]; then
          local maxretry findtime bantime
          maxretry=$(grep "^maxretry" "$_F2B_JAIL" | awk '{print $3}')
          findtime=$(grep "^findtime" "$_F2B_JAIL" | awk '{print $3}')
          bantime=$(grep  "^bantime"  "$_F2B_JAIL" | awk '{print $3}')
          sub_line "  Limits" "${maxretry:-?} attempts in ${findtime:-?}s → ban ${bantime:-?}s"
        fi
      else
        sub_line "  WP jail" "❌ not configured"
      fi
    fi

    echo ""
    echo "  1) Configure WordPress login protection jail"
    echo "  2) Restart Fail2ban"
    echo "  3) Adjust jail limits  (attempts / window / ban duration)"
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice

    case "$choice" in
      1) _fail2ban_wp_jail ;;
      2) _fail2ban_restart ;;
      3) _fail2ban_adjust_jail ;;
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

# Set key=value in a specific [section] of an ini-style file.
# Replaces existing key if present, otherwise appends it inside the section.
_ini_set_key() {
  local file="$1" section="$2" key="$3" value="$4"
  awk -v sec="$section" -v k="$key" -v v="$value" '
    BEGIN { in_sec=0; done=0 }
    /^\[/ {
      if (in_sec && !done) { print k " = " v; done=1 }
      in_sec = (substr($0, 2, length($0)-2) == sec)
    }
    in_sec && !done && $0 ~ "^"k"[[:space:]]*=" { print k " = " v; done=1; next }
    { print }
    END { if (in_sec && !done) print k " = " v }
  ' "$file" > "${file}.tmp" && mv "${file}.tmp" "$file"
}

# Test config and auto-disable any jails that reference missing log files.
# fail2ban reports only the first failing jail per run, so we loop until clean.
# jail.d/ overrides are read BEFORE jail.local, so jails defined in jail.local
# must be disabled there directly.
_fail2ban_fix_config() {
  local test_out jail iterations=0

  while true; do
    test_out=$(fail2ban-client -t 2>&1)
    [ $? -eq 0 ] && return 0
    ((iterations++))
    [ $iterations -gt 15 ] && break  # safety limit

    jail=$(echo "$test_out" | grep -oP "(?<=any log file for )\S+(?= jail)" | head -1)

    if [ -z "$jail" ]; then
      # Different kind of error — stop looping
      break
    fi

    echo -e "  ${CYAN}→${NC} Disabling '$jail' jail  (log files not found on this server)"
    if grep -q "^\[$jail\]" /etc/fail2ban/jail.local 2>/dev/null; then
      # jail.local is read last — must fix it there, jail.d overrides won't work
      _ini_set_key /etc/fail2ban/jail.local "$jail" "enabled" "false"
    else
      # Not in jail.local — a jail.d override is sufficient
      printf '[%s]\nenabled = false\n' "$jail" > "/etc/fail2ban/jail.d/zzz-disable-${jail}.conf"
    fi
    rm -f "/etc/fail2ban/jail.d/disable-${jail}.conf"
  done

  echo -e "  ${RED}✗ Config still failing:${NC}"
  echo "$test_out" | grep -v "WARNING" | sed 's/^/     /'
  return 1
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
  echo "    $_F2B_JAIL    — 10 attempts in 60s = 1h ban, watches /var/log/nginx/domains/*.log"
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
maxretry = 10
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

_fail2ban_adjust_jail() {
  if [ ! -f "$_F2B_JAIL" ]; then
    echo "  WordPress jail not configured yet. Use option 1 first."
    press_enter; return
  fi

  local maxretry findtime bantime
  maxretry=$(grep "^maxretry" "$_F2B_JAIL" | awk '{print $3}')
  findtime=$(grep "^findtime" "$_F2B_JAIL" | awk '{print $3}')
  bantime=$(grep  "^bantime"  "$_F2B_JAIL" | awk '{print $3}')

  echo ""
  echo -e "  ${BOLD}Adjust WordPress Jail Limits${NC}"
  echo "$DIV"
  echo "  Current settings:"
  echo "    maxretry = ${maxretry:-?}   (failed attempts before ban)"
  echo "    findtime = ${findtime:-?}s  (window in which attempts are counted)"
  echo "    bantime  = ${bantime:-?}s  (how long to ban the IP)"
  echo ""

  local new_maxretry new_findtime new_bantime

  read -r -p "  Max attempts before ban  [${maxretry:-5}]: " new_maxretry
  new_maxretry="${new_maxretry:-$maxretry}"
  if ! [[ "$new_maxretry" =~ ^[0-9]+$ ]]; then
    echo "  Invalid — must be a number."; press_enter; return
  fi

  read -r -p "  Window in seconds        [${findtime:-60}]: " new_findtime
  new_findtime="${new_findtime:-$findtime}"
  if ! [[ "$new_findtime" =~ ^[0-9]+$ ]]; then
    echo "  Invalid — must be a number."; press_enter; return
  fi

  read -r -p "  Ban duration in seconds  [${bantime:-3600}]: " new_bantime
  new_bantime="${new_bantime:-$bantime}"
  if ! [[ "$new_bantime" =~ ^[0-9]+$ ]]; then
    echo "  Invalid — must be a number."; press_enter; return
  fi

  echo ""
  echo "  Will set:"
  echo "    maxretry = $new_maxretry"
  echo "    findtime = ${new_findtime}s"
  echo "    bantime  = ${new_bantime}s"
  echo ""

  if ! confirm "Apply?"; then return; fi

  sed -i "s/^maxretry.*/maxretry = $new_maxretry/" "$_F2B_JAIL"
  sed -i "s/^findtime.*/findtime = $new_findtime/" "$_F2B_JAIL"
  sed -i "s/^bantime.*/bantime  = $new_bantime/"  "$_F2B_JAIL"

  echo ""
  echo -e "  ${CYAN}→${NC} Reloading Fail2ban..."
  fail2ban-client reload &>/dev/null
  echo -e "  ${GREEN}✓ Done${NC}"
  press_enter
}
