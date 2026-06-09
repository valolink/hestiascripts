#!/bin/bash
# Netdata monitoring

menu_netdata() {
  while true; do
    clear
    echo ""
    echo -e "  ${BOLD}Netdata${NC}"
    echo "$DIV"

    if command -v netdata &>/dev/null; then
      if systemctl is-active --quiet netdata; then
        status_line "Netdata" OK "running  http://$(hostname -I | awk '{print $1}'):19999"
      else
        status_line "Netdata" WARN "installed but not running"
      fi
    else
      status_line "Netdata" ERR "not installed"
    fi

    local fw_ok=false
    if /usr/local/hestia/bin/v-list-firewall plain 2>/dev/null | grep -q "19999"; then
      fw_ok=true
      sub_line "  Firewall port 19999" "✅ rule exists"
    else
      sub_line "  Firewall port 19999" "❌ no rule"
    fi

    echo ""
    echo "  1) Install Netdata"
    echo "  2) Add firewall rule for port 19999 (restrict to an IP)"
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice

    case "$choice" in
      1) _netdata_install ;;
      2) _netdata_firewall ;;
      0) return ;;
    esac
  done
}

_netdata_install() {
  run_action "Install Netdata" \
    "wget -O /tmp/netdata-kickstart.sh https://get.netdata.cloud/kickstart.sh" \
    "sh /tmp/netdata-kickstart.sh --non-interactive" \
    "rm -f /tmp/netdata-kickstart.sh"
}

_netdata_firewall() {
  echo ""
  read -r -p "  Allow access from IP (leave blank for any): " ip
  local target="${ip:-0.0.0.0}"

  run_action "Add HestiaCP firewall rule: TCP port 19999 from ${target}" \
    "/usr/local/hestia/bin/v-add-firewall-rule ACCEPT '$target' 19999 TCP 'Netdata'"
}
