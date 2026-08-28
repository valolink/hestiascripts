#!/bin/bash
# Audit reports — thin wrappers around the standalone v-server-* audit scripts.
#
# The logic deliberately lives in v-server-audit.sh / v-server-memory.sh rather
# than in this module: install-scripts.sh symlinks every v-* script into
# /usr/local/hestia/bin, so the same report is reachable three ways — this menu,
# a plain SSH call, and (v-server-* being allowlisted in main.go) EngineLink
# through the streamer. A setup/ module would only be reachable from run.sh.

menu_audit() {
  while true; do
    clear
    echo ""
    echo -e "  ${BOLD}Audit Reports${NC}"
    echo "$DIV"
    echo "  Read-only. Nothing is installed, changed or restarted."
    echo ""
    echo "  These report what is still outstanding rather than what is fine —"
    echo "  a check that passes prints nothing at all."
    echo ""
    echo "  1) Full server audit    (what still needs doing, by severity)"
    echo "  2) Memory audit         (where the RAM goes, and whether it matters)"
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice

    case "$choice" in
      1) clear; bash "$SCRIPT_DIR/v-server-audit.sh"; press_enter ;;
      2) clear; bash "$SCRIPT_DIR/v-server-memory.sh"; press_enter ;;
      0) return ;;
    esac
  done
}
