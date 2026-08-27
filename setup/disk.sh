#!/bin/bash
# Disk usage, cleanup, and ncdu

# Minimum size (MB) for a file to show up in the log finder. Adjustable from
# the log menu — a box that has already been cleaned wants a lower floor.
LOG_MIN_MB="${LOG_MIN_MB:-1}"

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
    sub_line "  Log files (≥${LOG_MIN_MB}MB)" "$LOGS_SIZE_H"

    echo ""
    echo "  1) Clean APT cache"
    echo "  2) Vacuum journal logs  (keep 2 weeks)"
    echo "  3) Clean WP caches  (all sites)"
    echo "  4) Clean WP transients  (expired, all sites)"
    echo "  5) Clean PHP sessions  (older than 1 day)"
    echo "  6) Clean wp-cli cache"
    echo "  7) Log files  (find large, truncate)"
    if command -v ncdu &>/dev/null; then
      echo "  8) Browse with ncdu"
    else
      echo "  8) Install ncdu"
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
      7) _disk_manage_logs ;;
      8) if command -v ncdu &>/dev/null; then _disk_ncdu_browse; else _disk_install_ncdu; fi ;;
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

  b=$(_disk_find_logs | awk -F'\t' '!seen[$2]++ {t+=$1} END {print t+0}')
  LOGS_SIZE_H=$([ "${b:-0}" -gt 0 ] && bytes_to_human "$b" || echo "—")
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

# Emit "<bytes>\t<path>" for every log-ish file worth looking at.
# Name patterns are deliberately loose and the wp-config pass is separate:
# WP_DEBUG_LOG is often pointed at a custom filename, and plugins invent their
# own log names, so an assumed wp-content/debug.log finds almost nothing real.
_disk_find_logs() {
  local site_roots=() log_roots=() r

  for r in /home/*/web/*/public_html /home/*/web/*/private; do
    [ -d "$r" ] && site_roots+=("$r")
  done
  # Hestia's per-domain web logs. /home/<user>/web/<domain>/logs/ holds symlinks
  # into these, so scanning the real directories avoids double-counting.
  for r in /var/log/nginx/domains /var/log/apache2/domains; do
    [ -d "$r" ] && log_roots+=("$r")
  done

  # maxdepth 4 keeps the walk off the plugin/theme file mass while still
  # reaching wp-content/uploads/<dir>/*.log and wp-content/plugins/<x>/logs/*.
  [ ${#site_roots[@]} -gt 0 ] && find "${site_roots[@]}" -maxdepth 4 -xdev -type f \
    \( -name '*.log' -o -name '*.log.*' -o -name '*_log' -o -name '*-log' \
       -o -name 'error_log' -o -name 'php_errorlog' -o -name '*.err' \
       -o -path '*/logs/*' \) \
    -size +"${LOG_MIN_MB}"M -printf '%s\t%p\n' 2>/dev/null

  [ ${#log_roots[@]} -gt 0 ] && find "${log_roots[@]}" -maxdepth 1 -xdev -type f \
    -size +"${LOG_MIN_MB}"M -printf '%s\t%p\n' 2>/dev/null

  _disk_find_wp_debug_logs
}

# Resolve WP_DEBUG_LOG when it names a file rather than being a bare boolean.
_disk_find_wp_debug_logs() {
  local wp_config docroot val path
  for wp_config in /home/*/web/*/public_html/wp-config.php; do
    [ -f "$wp_config" ] || continue
    docroot=$(dirname "$wp_config")
    val=$(grep -oP "define\(\s*['\"]WP_DEBUG_LOG['\"]\s*,\s*['\"]\K[^'\"]+" \
            "$wp_config" 2>/dev/null | tail -1)
    [ -n "$val" ] || continue
    case "$val" in
      /*) path="$val" ;;
      *)  path="$docroot/$val" ;;
    esac
    [ -f "$path" ] && stat -c '%s	%n' "$path" 2>/dev/null
  done
}

# Shorten a log path to "<domain>: <relative>" for the picker.
_disk_log_label() {
  local p="$1" rest domain
  case "$p" in
    /home/*/web/*/public_html/*)
      rest="${p#*/web/}"; domain="${rest%%/*}"
      echo "${domain}: ${p#*/public_html/}" ;;
    /home/*/web/*/private/*)
      rest="${p#*/web/}"; domain="${rest%%/*}"
      echo "${domain}: private/${p#*/private/}" ;;
    /var/log/nginx/domains/*)   echo "nginx: ${p##*/}" ;;
    /var/log/apache2/domains/*) echo "apache: ${p##*/}" ;;
    *) echo "$p" ;;
  esac
}

# Rotated or compressed logs are safe to delete outright; a live one is not.
_disk_log_is_rotated() {
  case "$1" in
    *.gz|*.bz2|*.xz|*.zip|*.[0-9]|*.[0-9][0-9]) return 0 ;;
    *) return 1 ;;
  esac
}

_disk_manage_logs() {
  local rows=() line size path label i choice

  while true; do
    clear
    echo ""
    echo -e "  ${BOLD}Log files${NC}"
    echo "$DIV"
    echo -e "  ${DIM}Scanning site directories and domain logs...${NC}"

    mapfile -t rows < <(_disk_find_logs \
      | awk -F'\t' '!seen[$2]++' \
      | sort -rn -t$'\t' -k1,1 \
      | head -30)

    printf '\e[1A\e[2K'

    if [ ${#rows[@]} -eq 0 ]; then
      echo "  No log files ≥ ${LOG_MIN_MB}MB found."
      echo ""
      echo "  s) Change size threshold (currently ${LOG_MIN_MB}MB)"
      echo "  0) Back"
      echo ""
      read -r -p "  Select: " choice
      case "$choice" in
        s|S) _disk_log_set_threshold ;;
        *) return ;;
      esac
      continue
    fi

    echo ""
    i=1
    for line in "${rows[@]}"; do
      size="${line%%	*}"
      path="${line#*	}"
      label=$(_disk_log_label "$path")
      printf "  %2d) %8s  %s\n" "$i" "$(bytes_to_human "$size")" "$label"
      i=$((i + 1))
    done

    echo ""
    echo "  s) Change size threshold (currently ${LOG_MIN_MB}MB)"
    echo "  0) Back"
    echo ""
    read -r -p "  Select a file to vacuum: " choice

    case "$choice" in
      s|S) _disk_log_set_threshold; continue ;;
      ''|0) return ;;
    esac

    if ! [[ "$choice" =~ ^[0-9]+$ ]] || [ "$choice" -lt 1 ] || [ "$choice" -gt ${#rows[@]} ]; then
      continue
    fi

    line="${rows[$((choice - 1))]}"
    _disk_vacuum_log "${line#*	}"
  done
}

_disk_log_set_threshold() {
  local n
  echo ""
  read -r -p "  Minimum size in MB [${LOG_MIN_MB}]: " n
  [[ "$n" =~ ^[0-9]+$ ]] && LOG_MIN_MB="$n"
}

_disk_vacuum_log() {
  local f="$1" choice tmp

  [ -f "$f" ] || { echo "  Gone: $f"; press_enter; return; }

  clear
  echo ""
  echo -e "  ${BOLD}$(_disk_log_label "$f")${NC}"
  echo "$DIV"
  sub_line "  Path" "$f"
  sub_line "  Size" "$(bytes_to_human "$(stat -c %s "$f")")"
  sub_line "  Owner" "$(stat -c '%U:%G %a' "$f")"
  sub_line "  Modified" "$(stat -c '%y' "$f" | cut -d'.' -f1)"

  echo ""
  echo -e "  ${DIM}Last 3 lines:${NC}"
  tail -n 3 "$f" 2>/dev/null | cut -c1-150 | sed 's/^/    /'

  echo ""
  echo "  1) Truncate to empty"
  echo "  2) Keep last 500 lines"
  if _disk_log_is_rotated "$f"; then
    echo "  3) Delete file  (rotated/compressed)"
  fi
  echo "  0) Cancel"
  echo ""
  read -r -p "  Select: " choice

  # Truncating in place rather than deleting is the whole point: nginx and
  # php-fpm hold these files open, and an unlinked-but-open log frees no disk
  # space until the writer is reloaded.
  case "$choice" in
    1)
      : > "$f" && echo -e "  ${GREEN}✓ Truncated${NC}" || echo -e "  ${RED}✗ Failed${NC}"
      ;;
    2)
      tmp=$(mktemp) || return
      if tail -n 500 "$f" > "$tmp" && cat "$tmp" > "$f"; then
        echo -e "  ${GREEN}✓ Kept last 500 lines${NC}"
      else
        echo -e "  ${RED}✗ Failed${NC}"
      fi
      rm -f "$tmp"
      ;;
    3)
      _disk_log_is_rotated "$f" || return
      confirm "Delete $f?" || return
      rm -f "$f" && echo -e "  ${GREEN}✓ Deleted${NC}"
      ;;
    *) return ;;
  esac

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
