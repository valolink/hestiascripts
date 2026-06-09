#!/bin/bash
# Linux Malware Detect (Maldet)

menu_maldet() {
  while true; do
    clear
    echo ""
    echo -e "  ${BOLD}Maldet — Linux Malware Detect${NC}"
    echo "$DIV"

    if [ -f /usr/local/maldetect/maldet ]; then
      local last
      last=$(grep "scan completed" /usr/local/maldetect/logs/event_log 2>/dev/null | tail -1 | awk '{print $1, $2}')
      status_line "Maldet" OK "installed"
      sub_line "  Last scan" "${last:-no scans on record}"
      local email
      email=$(grep "^email_addr=" /usr/local/maldetect/conf.maldet 2>/dev/null | cut -d'"' -f2)
      sub_line "  Alert email" "${email:-(not set)}"
    else
      status_line "Maldet" ERR "not installed"
    fi

    echo ""
    echo "  1) Install Maldet"
    echo "  2) Configure (alert email, quarantine)"
    echo "  3) Run scan now  (/home/*/web/*/public_html/)"
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice

    case "$choice" in
      1) _maldet_install ;;
      2) _maldet_configure ;;
      3) _maldet_scan ;;
      0) return ;;
    esac
  done
}

_maldet_install() {
  run_action "Install Maldet" \
    "cd /usr/local/src && wget -q http://www.rfxn.com/downloads/maldetect-current.tar.gz" \
    "cd /usr/local/src && tar -xzf maldetect-current.tar.gz" \
    "cd \$(find /usr/local/src -maxdepth 1 -name 'maldetect-[0-9]*' -type d | head -1) && bash install.sh" \
    "rm -rf /usr/local/src/maldetect-* /usr/local/src/maldetect-current.tar.gz"
}

_maldet_configure() {
  if [ ! -f /usr/local/maldetect/maldet ]; then
    echo "  Maldet is not installed. Use option 1 first."; press_enter; return
  fi

  local conf="/usr/local/maldetect/conf.maldet"
  echo ""
  read -r -p "  Alert email address: " email
  if [ -z "$email" ]; then echo "  No email entered, skipping."; press_enter; return; fi

  echo ""
  echo "  Will configure:"
  echo "    email_alert=1"
  echo "    email_addr=\"$email\""
  echo "    quarantine_hits=1"
  echo "    quarantine_clean=1"
  echo ""

  if ! confirm "Apply?"; then echo "  Cancelled."; return; fi

  sed -i "s/^email_alert=.*/email_alert=\"1\"/" "$conf"
  sed -i "s/^email_addr=.*/email_addr=\"$email\"/" "$conf"
  sed -i "s/^quarantine_hits=.*/quarantine_hits=\"1\"/" "$conf"
  sed -i "s/^quarantine_clean=.*/quarantine_clean=\"1\"/" "$conf"

  echo -e "  ${GREEN}✓ Configuration updated${NC}"
  press_enter
}

_maldet_scan() {
  if [ ! -f /usr/local/maldetect/maldet ]; then
    echo "  Maldet is not installed."; press_enter; return
  fi
  echo ""
  echo "  This will scan all sites under /home/*/web/*/public_html/"
  echo "  This can take several minutes on a server with many sites."
  if ! confirm "Run scan now?"; then return; fi
  echo ""
  maldet -a /home/*/web/*/public_html/
  press_enter
}
