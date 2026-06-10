#!/bin/bash
# Security hardening — unattended upgrades, swap, SSH

_UU_FILE="/etc/apt/apt.conf.d/50unattended-upgrades"

menu_security() {
  while true; do
    clear
    echo ""
    echo -e "  ${BOLD}Security & System Hardening${NC}"
    echo "$DIV"

    # Unattended upgrades
    if dpkg -l unattended-upgrades 2>/dev/null | grep -q "^ii" && \
       systemctl is-active --quiet unattended-upgrades 2>/dev/null; then
      status_line "Unattended upgrades" OK "enabled"
      local scope
      scope=$(_uu_scope)
      if [ "$scope" = "security-only" ]; then
        sub_line "  Scope" "security updates only"
      elif [ "$scope" = "all" ]; then
        sub_line "  Scope" "⚠️  all packages  (option 1 → 2 to restrict)"
      fi
    else
      status_line "Unattended upgrades" ERR "not set up"
    fi

    # Swap
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

    # SSH password auth
    local pw_auth
    pw_auth=$(grep "^PasswordAuthentication" /etc/ssh/sshd_config 2>/dev/null | awk '{print $2}')
    if [ "$pw_auth" = "no" ]; then
      status_line "SSH password auth" OK "disabled (key-only)"
    elif [ -z "$pw_auth" ]; then
      status_line "SSH password auth" WARN "not explicitly set (default: enabled)"
    else
      status_line "SSH password auth" WARN "enabled — consider disabling"
    fi

    echo ""
    echo "  1) Unattended upgrades"
    echo "  2) Create swapfile"
    echo "  3) Disable SSH password authentication"
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice

    case "$choice" in
      1) menu_unattended ;;
      2) _security_swap ;;
      3) _security_ssh ;;
      0) return ;;
    esac
  done
}

# --- Unattended upgrades submenu ---

menu_unattended() {
  while true; do
    clear
    echo ""
    echo -e "  ${BOLD}Unattended Upgrades${NC}"
    echo "$DIV"

    if dpkg -l unattended-upgrades 2>/dev/null | grep -q "^ii" && \
       systemctl is-active --quiet unattended-upgrades 2>/dev/null; then
      status_line "Service" OK "running"
    else
      status_line "Service" ERR "not installed / not running"
    fi

    if [ -f "$_UU_FILE" ]; then
      echo ""
      local scope
      scope=$(_uu_scope)
      if [ "$scope" = "security-only" ]; then
        status_line "Update scope" OK "security updates only"
      else
        status_line "Update scope" WARN "all Debian packages  (includes non-security)"
      fi

      local mail_addr
      mail_addr=$(grep -oP '^Unattended-Upgrade::Mail "\K[^"]+' "$_UU_FILE" 2>/dev/null | head -1)
      if [ -n "$mail_addr" ]; then
        status_line "Notification mail" OK "$mail_addr"
      else
        status_line "Notification mail" WARN "not set"
      fi

      local reboot
      reboot=$(grep '^Unattended-Upgrade::Automatic-Reboot ' "$_UU_FILE" 2>/dev/null \
               | grep -oP '"[^"]+"' | tr -d '"')
      if [ "$reboot" = "true" ]; then
        local reboot_time
        reboot_time=$(grep '^Unattended-Upgrade::Automatic-Reboot-Time ' "$_UU_FILE" 2>/dev/null \
                      | grep -oP '"[^"]+"' | tr -d '"')
        status_line "Auto-reboot" WARN "enabled  (at ${reboot_time:-02:00})"
      else
        status_line "Auto-reboot" OK "disabled"
      fi
    fi

    echo ""
    echo "  1) Install / enable"
    echo "  2) Security updates only  (recommended)"
    echo "  3) Enable all Debian updates"
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice

    case "$choice" in
      1) _security_unattended ;;
      2) _uu_set_security_only ;;
      3) _uu_set_all_updates ;;
      0) return ;;
    esac
  done
}

_uu_scope() {
  [ -f "$_UU_FILE" ] || { echo "unknown"; return; }
  if grep -qE '^[[:space:]]*"origin=Debian[^"]*,label=Debian";' "$_UU_FILE" 2>/dev/null; then
    echo "all"
  else
    echo "security-only"
  fi
}

_uu_set_security_only() {
  if [ ! -f "$_UU_FILE" ]; then
    echo "  Config file not found. Use option 1 to install first."
    press_enter; return
  fi

  if [ "$(_uu_scope)" = "security-only" ]; then
    echo ""
    echo "  Already set to security updates only."
    press_enter; return
  fi

  echo ""
  echo "  Will comment out these non-security origin lines in $_UU_FILE:"
  echo ""
  grep -E '^[[:space:]]*"origin=Debian[^"]*,label=Debian";' "$_UU_FILE" | sed 's/^/    /'
  echo ""
  echo "  Only Debian security updates will be applied automatically."
  echo "  PHP, Nginx, MariaDB, HestiaCP etc. will still require manual updates."

  if ! confirm "Apply?"; then return; fi

  sed -i 's|^\([[:space:]]*"origin=Debian[^"]*,label=Debian";\)$|//\1|' "$_UU_FILE"

  echo ""
  echo -e "  ${GREEN}✓ Done${NC}"
  press_enter
}

_uu_set_all_updates() {
  if [ ! -f "$_UU_FILE" ]; then
    echo "  Config file not found. Use option 1 to install first."
    press_enter; return
  fi

  echo ""
  echo -e "  ${YELLOW}⚠${NC}  All Debian package updates will be applied automatically."
  echo "  This includes PHP, Nginx, MariaDB upgrades which can cause unplanned downtime."
  echo ""

  if ! confirm "Enable all Debian updates?"; then return; fi

  sed -i 's|^//\([[:space:]]*"origin=Debian[^"]*,label=Debian";\)$|\1|' "$_UU_FILE"

  echo ""
  echo -e "  ${GREEN}✓ Done${NC}"
  press_enter
}

# --- Install ---

_security_unattended() {
  run_action "Install unattended-upgrades" \
    "apt update" \
    "apt install unattended-upgrades apt-listchanges -y" \
    "dpkg-reconfigure -plow unattended-upgrades"
}

# --- Swap ---

_security_swap() {
  local swap_info
  swap_info=$(swapon --show --noheadings 2>/dev/null)
  if [ -n "$swap_info" ]; then
    echo ""
    echo "  Swap already exists:"
    swapon --show
    echo ""
    if ! confirm "Create another swapfile anyway?"; then return; fi
  fi

  local ram_mb suggested
  ram_mb=$(get_ram_mb)
  suggested=$([ "$ram_mb" -le 4096 ] && echo "2G" || echo "1G")

  echo ""
  echo "  Server RAM: ${ram_mb}MB"
  echo "  Recommended: ${suggested}"
  echo ""
  read -r -p "  Swapfile size (e.g. 2G, 512M) [${suggested}]: " size
  size="${size:-$suggested}"

  run_action "Create ${size} swapfile at /swapfile" \
    "fallocate -l ${size} /swapfile" \
    "chmod 600 /swapfile" \
    "mkswap /swapfile" \
    "swapon /swapfile" \
    "echo '/swapfile none swap sw 0 0' >> /etc/fstab"
}

# --- SSH ---

_security_ssh() {
  local current
  current=$(grep "^PasswordAuthentication" /etc/ssh/sshd_config 2>/dev/null | awk '{print $2}')
  if [ "$current" = "no" ]; then
    echo "  SSH password authentication is already disabled."
    press_enter; return
  fi

  echo ""
  echo -e "  ${YELLOW}⚠️  WARNING${NC}: Make sure you have SSH key access before doing this."
  echo "  If you disable password auth without a working key, you will be locked out."
  echo ""

  run_action "Disable SSH password authentication" \
    "sed -i 's/^#*PasswordAuthentication.*/PasswordAuthentication no/' /etc/ssh/sshd_config" \
    "systemctl reload sshd"
}
