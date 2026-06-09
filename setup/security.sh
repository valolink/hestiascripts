#!/bin/bash
# Security hardening — unattended upgrades, swap, SSH

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
    else
      status_line "Unattended upgrades" ERR "not set up"
    fi

    # Swap
    local swap_info
    swap_info=$(swapon --show --noheadings 2>/dev/null | awk '{print $3}')
    if [ -n "$swap_info" ]; then
      status_line "Swap" OK "$swap_info"
    else
      local ram_mb
      ram_mb=$(get_ram_mb)
      local suggested=$([ "$ram_mb" -le 4096 ] && echo "2G" || echo "1G")
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
    echo "  1) Set up unattended security upgrades"
    echo "  2) Create swapfile"
    echo "  3) Disable SSH password authentication"
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice

    case "$choice" in
      1) _security_unattended ;;
      2) _security_swap ;;
      3) _security_ssh ;;
      0) return ;;
    esac
  done
}

_security_unattended() {
  run_action "Set up unattended security upgrades" \
    "apt update" \
    "apt install unattended-upgrades apt-listchanges -y" \
    "dpkg-reconfigure -plow unattended-upgrades"

  echo "  ℹ️  Only security updates are applied automatically."
  echo "     PHP, Nginx, MariaDB should still be updated manually."
  press_enter
}

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
