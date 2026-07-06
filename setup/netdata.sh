#!/bin/bash
# Netdata monitoring

NETDATA_CONF="/etc/netdata/netdata.conf"
NETDATA_TUNED_MARK="hestiascripts-tuned"

_netdata_is_tuned() {
  grep -q "$NETDATA_TUNED_MARK" "$NETDATA_CONF" 2>/dev/null
}

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
      if _netdata_is_tuned; then
        sub_line "  Profile" "✅ low-footprint (ML off, 2s collection)"
      else
        sub_line "  Profile" "⚠️  stock defaults (ML on — periodic CPU/RAM spikes)"
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
    echo "  3) Apply low-footprint tuning (ML off, 2s collection, capped cache)"
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice

    case "$choice" in
      1) _netdata_install ;;
      2) _netdata_firewall ;;
      3) _netdata_tune ;;
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

# Low-footprint profile for small shared-hosting boxes. Stock Netdata is
# sized for dedicated observability hosts: per-second collection, ML anomaly
# training (periodic CPU/RAM spikes — the web1 incident, 2026-07-06) and a
# generous dbengine cache. This halves collection frequency, turns ML off,
# and caps memory — while KEEPING the health engine, which EngineLink's
# alarm polling depends on. The previous netdata.conf is backed up next to
# it; delete the tuned file and restart to go back to stock.
_netdata_tune() {
  if ! command -v netdata &>/dev/null; then
    echo "  Netdata is not installed. Use option 1 first."
    press_enter; return
  fi

  local tmp="/tmp/netdata.conf.tuned.$$"
  cat > "$tmp" <<EOF
# ${NETDATA_TUNED_MARK} — low-footprint profile written by hestiascripts
# (setup/netdata.sh). Stock config backed up as netdata.conf.pre-tune.
# Health engine stays ON: EngineLink's alarm polling reads it.

[global]
    update every = 2

[ml]
    enabled = no

[db]
    mode = dbengine
    storage tiers = 1
    dbengine page cache size MB = 32
    dbengine multihost disk space MB = 512

[health]
    enabled = yes
EOF

  run_action "Apply low-footprint Netdata profile (backs up current config)" \
    "cp -a $NETDATA_CONF ${NETDATA_CONF}.pre-tune 2>/dev/null || true" \
    "install -m 0644 $tmp $NETDATA_CONF" \
    "rm -f $tmp" \
    "systemctl restart netdata" \
    "sleep 3" \
    "systemctl is-active netdata"
}
