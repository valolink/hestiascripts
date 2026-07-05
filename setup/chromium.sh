#!/bin/bash
# Headless Chromium — on-box rendering (screenshots, render checks) so
# EngineLink-driven scripts can paint a page where the site actually lives
# instead of shipping a browser in the app image.

_chromium_bin() {
  command -v chromium 2>/dev/null || command -v chromium-browser 2>/dev/null
}

menu_chromium() {
  while true; do
    clear
    echo ""
    echo -e "  ${BOLD}Headless Chromium${NC}"
    echo "$DIV"

    local bin; bin=$(_chromium_bin)
    if [ -n "$bin" ]; then
      local ver; ver=$("$bin" --version 2>/dev/null | grep -oP '[0-9][0-9.]*' | head -1)
      status_line "Chromium" OK "${ver}  (${bin})"
    else
      status_line "Chromium" ERR "not installed"
    fi

    echo ""
    echo "  1) Install headless Chromium (+ fonts)"
    echo "  2) Update Chromium"
    echo "  3) Test render (screenshot example.com to /tmp)"
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice

    case "$choice" in
      1) _chromium_install ;;
      2) _chromium_update ;;
      3) _chromium_test ;;
      0) return ;;
    esac
  done
}

_chromium_install() {
  # fonts-liberation covers the metric-compatible web-font stand-ins;
  # noto-color-emoji keeps emoji from rendering as tofu in screenshots.
  run_action "Install Chromium + fonts" \
    "apt-get update" \
    "DEBIAN_FRONTEND=noninteractive apt-get install -y chromium fonts-liberation fonts-noto-color-emoji"
}

_chromium_update() {
  if [ -z "$(_chromium_bin)" ]; then
    echo "  Chromium is not installed. Use option 1 first."
    press_enter; return
  fi
  run_action "Update Chromium" \
    "apt-get update" \
    "DEBIAN_FRONTEND=noninteractive apt-get install -y --only-upgrade chromium"
}

_chromium_test() {
  local bin; bin=$(_chromium_bin)
  if [ -z "$bin" ]; then
    echo "  Chromium is not installed. Use option 1 first."
    press_enter; return
  fi
  # Root needs --no-sandbox; a fixed public page to /tmp is a plumbing
  # test, not a hardening hole.
  run_action "Headless render test (https://example.com)" \
    "$bin --headless=new --no-sandbox --disable-gpu --disable-dev-shm-usage --window-size=1280,800 --screenshot=/tmp/chromium-test.png https://example.com" \
    "ls -lh /tmp/chromium-test.png"
}
