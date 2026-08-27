#!/bin/bash
# Shared utilities — sourced by all setup modules and run.sh

# Colors (only when stdout is a terminal)
if [ -t 1 ]; then
  BOLD='\033[1m'; DIM='\033[2m'
  RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'
  BLUE='\033[0;34m'; CYAN='\033[0;36m'; NC='\033[0m'
else
  BOLD=''; DIM=''; RED=''; YELLOW=''; GREEN=''; BLUE=''; CYAN=''; NC=''
fi

DIV="  $(printf '─%.0s' {1..52})"

# Print a status line: status_line "Label" OK|WARN|ERR "detail"
status_line() {
  local label="$1" level="$2" detail="$3" sym
  case "$level" in
    OK)   sym="✅ " ;;
    WARN) sym="⚠️  " ;;
    ERR)  sym="❌ " ;;
    *)    sym="    " ;;
  esac
  printf "  %-28s %s%s\n" "$label" "$sym" "$detail"
}

# Print a sub-item line (indented, no symbol column)
sub_line() {
  printf "  %-28s     %s\n" "$1" "$2"
}

# Numbered menu line with inline status: menu_status_line NUM LABEL LEVEL DETAIL
menu_status_line() {
  local num="$1" label="$2" level="$3" detail="$4" sym
  case "$level" in
    OK)   sym="✅ " ;;
    WARN) sym="⚠️  " ;;
    ERR)  sym="❌ " ;;
    *)    sym="    " ;;
  esac
  printf "  %2s) %-22s %s%s\n" "$num" "$label" "$sym" "$detail"
}

section() {
  echo ""
  echo -e "  ${BOLD}[ $1 ]${NC}"
}

# Ask yes/no, return 0 for yes
confirm() {
  local prompt="${1:-Proceed?}"
  echo ""
  read -r -p "  $prompt [y/N] " _ans
  [[ "$_ans" =~ ^[Yy]$ ]]
}

press_enter() {
  echo ""
  read -r -p "  Press Enter to continue... "
}

# Show commands and optionally run them after confirmation
# Usage: run_action "Title" "cmd1" "cmd2" ...
run_action() {
  local title="$1"; shift
  echo ""
  echo -e "  ${BOLD}$title${NC}"
  echo "$DIV"
  echo "  Commands to run:"
  local cmd
  for cmd in "$@"; do
    echo "    $cmd"
  done

  if ! confirm "Proceed?"; then
    echo "  Cancelled."
    return 1
  fi

  echo ""
  for cmd in "$@"; do
    echo -e "  ${CYAN}→${NC} $cmd"
    eval "$cmd"
    if [ $? -ne 0 ]; then
      echo -e "  ${RED}✗ Command failed${NC}"
      press_enter
      return 1
    fi
  done
  echo ""
  echo -e "  ${GREEN}✓ Done${NC}"
  press_enter
  return 0
}

# apt-get update that survives one broken third-party repository.
#
# A repo that fails to refresh is not a broken system: that source stops
# supplying updates while every other repo keeps working. Treating it as fatal
# blocks unrelated work (installing redis, say) for no gain — and the following
# `apt install` fails loudly on its own if the package really is unreachable,
# so nothing silently half-succeeds.
apt_update_safe() {
  local out rc broken
  out=$(apt-get update 2>&1); rc=$?
  echo "$out" | grep -vE '^(Hit|Get|Ign):' | sed 's/^/    /'
  [ $rc -eq 0 ] && return 0

  broken=$(echo "$out" | awk '/^Err:/ {print $2}' | awk -F/ '{print $3}' | sort -u | paste -sd' ')
  # Failed for a reason other than a specific repo (no network, locked dpkg) —
  # that one the caller should still treat as fatal.
  [ -z "$broken" ] && return $rc

  echo ""
  echo -e "  ${YELLOW}⚠️  Unreachable repositories: ${broken}${NC}"
  echo -e "  ${DIM}Everything else refreshed; continuing.${NC}"
  echo -e "  ${DIM}Diagnose with: Maintenance → Check apt repositories${NC}"
  return 0
}

# Detect installed PHP versions (e.g. 8.1 8.2 8.3)
get_php_versions() {
  find /etc/php -maxdepth 1 -mindepth 1 -type d -printf '%f\n' 2>/dev/null | sort -V
}

# Total RAM in MB
get_ram_mb() {
  awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo
}

# Convert bytes to human-readable MB/GB
bytes_to_human() {
  local bytes=$1
  if [ "$bytes" -ge $((1024*1024*1024)) ]; then
    echo "$((bytes / 1024 / 1024 / 1024))G"
  else
    echo "$((bytes / 1024 / 1024))M"
  fi
}
