#!/bin/bash
# Disk usage, cleanup, and ncdu

menu_disk() {
  while true; do
    clear
    echo ""
    echo -e "  ${BOLD}Disk${NC}"
    echo "$DIV"

    local used_pct info
    used_pct=$(df / | awk 'NR==2 {gsub(/%/,"",$5); print $5}')
    info=$(df -h / | awk 'NR==2 {print $3 " / " $2 " used (" $5 ")"}')
    if [ "${used_pct:-0}" -ge 90 ]; then
      status_line "Disk (/)" ERR "$info"
    elif [ "${used_pct:-0}" -ge 75 ]; then
      status_line "Disk (/)" WARN "$info"
    else
      status_line "Disk (/)" OK "$info"
    fi

    echo ""
    echo -e "  ${DIM}Scanning cleanable items...${NC}"
    _disk_calc_sizes
    printf '\e[1A\e[2K'

    sub_line "  APT cache" "$APT_SIZE_H"
    sub_line "  Journal logs" "$JOURNAL_SIZE_H"
    sub_line "  WP caches (all sites)" "$WP_CACHE_SIZE_H"
    sub_line "  PHP sessions (>1d old)" "$PHP_SESS_H"
    sub_line "  wp-cli cache" "$WPCLI_CACHE_H"

    echo ""
    echo "  1) Clean APT cache"
    echo "  2) Vacuum journal logs  (keep 2 weeks)"
    echo "  3) Clean WP caches  (all sites)"
    echo "  4) Clean WP transients  (expired, all sites)"
    echo "  5) Clean PHP sessions  (older than 1 day)"
    echo "  6) Clean wp-cli cache"
    if command -v ncdu &>/dev/null; then
      echo "  7) Browse with ncdu"
    else
      echo "  7) Install ncdu"
    fi
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice

    case "$choice" in
      1) _disk_clean_apt ;;
      2) _disk_clean_journal ;;
      3) _disk_clean_wp_caches ;;
      4) _disk_clean_wp_transients ;;
      5) _disk_clean_php_sessions ;;
      6) _disk_clean_wpcli_cache ;;
      7) if command -v ncdu &>/dev/null; then _disk_ncdu_browse; else _disk_install_ncdu; fi ;;
      0) return ;;
    esac
  done
}

_disk_calc_sizes() {
  local b s

  b=$(du -sb /var/cache/apt/archives/ 2>/dev/null | awk '{print $1+0}')
  APT_SIZE_H=$([ "${b:-0}" -gt 0 ] && bytes_to_human "$b" || echo "—")

  b=$(du -sb /var/log/journal/ 2>/dev/null | awk '{print $1+0}')
  JOURNAL_SIZE_H=$([ "${b:-0}" -gt 0 ] && bytes_to_human "$b" || echo "—")

  b=0
  for d in /home/*/web/*/public_html/wp-content/cache/; do
    [ -d "$d" ] || continue
    s=$(du -sb "$d" 2>/dev/null | awk '{print $1+0}')
    b=$((b + s))
  done
  WP_CACHE_SIZE_H=$([ "$b" -gt 0 ] && bytes_to_human "$b" || echo "—")

  b=$(du -sb /var/lib/php/sessions/ 2>/dev/null | awk '{print $1+0}')
  PHP_SESS_H=$([ "${b:-0}" -gt 0 ] && bytes_to_human "$b" || echo "—")

  b=0
  for d in /root/.wp-cli/cache/ /home/*/.wp-cli/cache/; do
    [ -d "$d" ] || continue
    s=$(du -sb "$d" 2>/dev/null | awk '{print $1+0}')
    b=$((b + s))
  done
  WPCLI_CACHE_H=$([ "$b" -gt 0 ] && bytes_to_human "$b" || echo "—")
}

_disk_clean_apt() {
  run_action "Clean APT cache" \
    "apt-get clean" \
    "apt-get autoclean -y"
}

_disk_clean_journal() {
  run_action "Vacuum journal logs (keep 2 weeks)" \
    "journalctl --vacuum-time=2weeks"
}

_disk_clean_wp_caches() {
  local sites=()
  for wp_config in /home/*/web/*/public_html/wp-config.php; do
    [ -f "$wp_config" ] || continue
    local cache_dir
    cache_dir="$(dirname "$wp_config")/wp-content/cache"
    [ -d "$cache_dir" ] && sites+=("$cache_dir")
  done

  if [ ${#sites[@]} -eq 0 ]; then
    echo "  No WP cache directories found."
    press_enter; return
  fi

  echo ""
  echo "  Will delete contents of:"
  for d in "${sites[@]}"; do echo "    $d"; done

  if ! confirm "Proceed?"; then return; fi

  echo ""
  for d in "${sites[@]}"; do
    echo -e "  ${CYAN}→${NC} Clearing $d"
    find "$d" -mindepth 1 -delete 2>/dev/null
  done
  echo -e "  ${GREEN}✓ Done${NC}"
  press_enter
}

_disk_clean_wp_transients() {
  if ! command -v wp &>/dev/null; then
    echo "  WP-CLI is not installed."
    press_enter; return
  fi

  local sites=()
  for wp_config in /home/*/web/*/public_html/wp-config.php; do
    [ -f "$wp_config" ] || continue
    sites+=("$wp_config")
  done

  if [ ${#sites[@]} -eq 0 ]; then
    echo "  No WordPress installations found."
    press_enter; return
  fi

  echo ""
  echo "  Will delete expired transients on ${#sites[@]} site(s)."
  if ! confirm "Proceed?"; then return; fi

  echo ""
  for wp_config in "${sites[@]}"; do
    local wp_path user domain
    wp_path=$(dirname "$wp_config")
    user=$(echo "$wp_path" | cut -d'/' -f3)
    domain=$(echo "$wp_path" | cut -d'/' -f5)
    echo -e "  ${CYAN}→${NC} $domain"
    sudo -u "$user" wp transient delete --expired --all-users --path="$wp_path" 2>&1 | \
      sed 's/^/     /'
  done

  echo ""
  echo -e "  ${GREEN}✓ Done${NC}"
  press_enter
}

_disk_clean_php_sessions() {
  local count
  count=$(find /var/lib/php/sessions/ -maxdepth 2 -name 'sess_*' -mtime +1 2>/dev/null | wc -l)

  echo ""
  echo "  Found $count session file(s) older than 1 day."
  [ "$count" -eq 0 ] && { press_enter; return; }

  if ! confirm "Delete them?"; then return; fi

  find /var/lib/php/sessions/ -maxdepth 2 -name 'sess_*' -mtime +1 -delete 2>/dev/null
  echo -e "  ${GREEN}✓ Deleted $count file(s)${NC}"
  press_enter
}

_disk_clean_wpcli_cache() {
  local dirs=()
  for d in /root/.wp-cli/cache/ /home/*/.wp-cli/cache/; do
    [ -d "$d" ] && dirs+=("$d")
  done

  if [ ${#dirs[@]} -eq 0 ]; then
    echo "  No wp-cli cache directories found."
    press_enter; return
  fi

  echo ""
  echo "  Will clear:"
  for d in "${dirs[@]}"; do echo "    $d"; done

  if ! confirm "Proceed?"; then return; fi

  echo ""
  for d in "${dirs[@]}"; do
    echo -e "  ${CYAN}→${NC} Clearing $d"
    find "$d" -mindepth 1 -delete 2>/dev/null
  done
  echo -e "  ${GREEN}✓ Done${NC}"
  press_enter
}

_disk_ncdu_browse() {
  echo ""
  echo "  Browse which directory?"
  echo "  1) /          (entire filesystem)"
  echo "  2) /home      (user files and WP sites)"
  echo "  3) /backup    (HestiaCP backups)"
  echo "  4) /var/log   (logs)"
  echo "  5) Custom path"
  echo ""
  read -r -p "  Select: " d

  local path
  case "$d" in
    1) path="/" ;;
    2) path="/home" ;;
    3) path="/backup" ;;
    4) path="/var/log" ;;
    5) read -r -p "  Path: " path ;;
    *) return ;;
  esac

  [ -d "$path" ] || { echo "  Not a directory: $path"; press_enter; return; }
  ncdu "$path"
}

_disk_install_ncdu() {
  run_action "Install ncdu" \
    "apt-get install -y ncdu"
}
