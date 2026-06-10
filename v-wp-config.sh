#!/bin/bash
# v-wp-config — view and manage wp-config.php defines

if [ "$EUID" -ne 0 ]; then
  echo "ERROR: Please run as root."
  exit 1
fi

export PATH=$PATH:/usr/local/hestia/bin

if [ -t 1 ]; then
  BOLD='\033[1m'; DIM='\033[2m'; GREEN='\033[0;32m'
  YELLOW='\033[1;33m'; CYAN='\033[0;36m'; NC='\033[0m'
else
  BOLD=''; DIM=''; GREEN=''; YELLOW=''; CYAN=''; NC=''
fi

show_help() {
  cat <<'EOF'
USAGE: v-wp-config [OPTIONS]

View and manage wp-config.php defines for a WordPress site.
Shows current values and lets you add or update any of the
commonly useful constants.

OPTIONS:
  --user=USER       HestiaCP user who owns the site
  --domain=DOMAIN   Domain name
  -h, --help        Show this help
EOF
}

WEB_USER=""
DOMAIN=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --user=*)   WEB_USER="${1#*=}" ;;
    --domain=*) DOMAIN="${1#*=}" ;;
    -h|--help)  show_help; exit 0 ;;
    *) echo "ERROR: Unknown option: $1"; show_help; exit 1 ;;
  esac
  shift
done

# --- Interactive selection ---
if [ -z "$WEB_USER" ]; then
  mapfile -t _USERS < <(v-list-users plain 2>/dev/null | cut -f1)
  echo ""
  PS3="Select user: "
  select WEB_USER in "${_USERS[@]}"; do
    [ -n "$WEB_USER" ] && break; echo "Invalid."
  done
fi

if [ -z "$DOMAIN" ]; then
  echo ""
  mapfile -t _DOMAINS < <(v-list-web-domains "$WEB_USER" plain 2>/dev/null | cut -f1)
  [ ${#_DOMAINS[@]} -eq 0 ] && { echo "ERROR: No domains for $WEB_USER."; exit 1; }
  PS3="Select domain: "
  select DOMAIN in "${_DOMAINS[@]}"; do
    [ -n "$DOMAIN" ] && break; echo "Invalid."
  done
fi

DOMAIN="${DOMAIN#www.}"
WEB_DIR="/home/$WEB_USER/web/$DOMAIN/public_html"

[ -f "$WEB_DIR/wp-config.php" ] || { echo "ERROR: No wp-config.php in $WEB_DIR"; exit 1; }
command -v wp &>/dev/null         || { echo "ERROR: WP-CLI not installed."; exit 1; }

# --- Define registry ---
# Format: "=Section"  or  "NAME|type|default|recommended|description"
# Types:
#   bool          true / false  (stored as PHP literal, no quotes)
#   int           integer       (stored as PHP literal)
#   intfalse      integer or false
#   str           string        (stored quoted)
#   core_update   true / minor / false  (mixed: minor is a string, others are PHP literals)

_DEFS=(
  "=Performance"
  "WP_POST_REVISIONS|intfalse|unlimited|5|Revisions to keep per post — false disables, 5–10 keeps DB lean"
  "AUTOSAVE_INTERVAL|int|60|120|Autosave interval in seconds (default 60)"
  "WP_MEMORY_LIMIT|str|40M|256M|Frontend PHP memory limit"
  "WP_MAX_MEMORY_LIMIT|str|256M|512M|Admin panel and WP-CLI memory limit"
  "=Security"
  "DISALLOW_FILE_EDIT|bool|false|true|Disable theme/plugin code editor in admin"
  "DISALLOW_FILE_MODS|bool|false|false|Disable ALL file changes incl. updates — removes update UI"
  "FORCE_SSL_ADMIN|bool|false|true|Force HTTPS for wp-admin and wp-login.php"
  "WP_DEBUG|bool|false|false|Debug mode — keep false in production"
  "WP_DEBUG_LOG|bool|false|false|Write PHP errors to wp-content/debug.log"
  "WP_DEBUG_DISPLAY|bool|true|false|Show PHP errors on screen — keep false in production"
  "=Cron"
  "DISABLE_WP_CRON|bool|false|-|Disable built-in HTTP cron — set up a real system cron if enabling"
  "WP_CRON_LOCK_TIMEOUT|int|60|60|Seconds before a cron lock expires"
  "=Updates"
  "WP_AUTO_UPDATE_CORE|core_update|minor|minor|Core auto-updates: true=all  minor=security only  false=none"
  "AUTOMATIC_UPDATER_DISABLED|bool|false|false|Disable all automatic background updates"
  "=Cleanup"
  "EMPTY_TRASH_DAYS|int|30|14|Days before trash is permanently deleted (0 = disable trash)"
  "MEDIA_TRASH|bool|false|false|Send deleted media to trash instead of deleting immediately"
  "=Cache"
  "WP_CACHE|bool|false|true|Enable object caching (required for Redis object cache drop-in)"
  "WP_CACHE_KEY_SALT|str|-|-|Unique prefix for all cache keys — set automatically by v-wp-redis-install"
)

# --- Helpers ---

_wp() { sudo -u "$WEB_USER" wp --path="$WEB_DIR" "$@" 2>/dev/null; }

_cfg_get() {
  _wp config get "$1" --type=constant 2>/dev/null
}

_cfg_has() {
  _wp config has "$1" --type=constant 2>/dev/null
}

# Build index: ordered list of define names (skipping section headers)
_DEF_NAMES=()
for _d in "${_DEFS[@]}"; do
  [[ "$_d" == =* ]] && continue
  IFS='|' read -r _n _ <<< "$_d"
  _DEF_NAMES+=("$_n")
done

# Look up a field (1=name 2=type 3=default 4=rec 5=desc) for a given name
_def_field() {
  local name="$1" field="$2"
  for d in "${_DEFS[@]}"; do
    [[ "$d" == =* ]] && continue
    IFS='|' read -r n type default rec desc <<< "$d"
    if [ "$n" = "$name" ]; then
      case "$field" in
        type)    echo "$type" ;;
        default) echo "$default" ;;
        rec)     echo "$rec" ;;
        desc)    echo "$desc" ;;
      esac
      return
    fi
  done
}

_show_dashboard() {
  clear
  echo ""
  echo -e "  ${BOLD}=== wp-config.php — $DOMAIN ===${NC}"
  echo ""

  local num=0
  for d in "${_DEFS[@]}"; do
    if [[ "$d" == =* ]]; then
      echo ""
      echo -e "  ${BOLD}[ ${d#=} ]${NC}"
      continue
    fi
    ((num++))
    IFS='|' read -r name type default rec desc <<< "$d"
    local val
    val=$(_cfg_get "$name")
    if [ -n "$val" ]; then
      printf "  ${CYAN}%2d)${NC} %-30s ${GREEN}%s${NC}\n" "$num" "$name" "$val"
    else
      printf "  ${CYAN}%2d)${NC} %-30s ${DIM}not set  (default: %s)${NC}\n" "$num" "$name" "$default"
    fi
    printf "      ${DIM}%s${NC}\n" "$desc"
  done

  echo ""
  echo "   0) Exit"
  echo ""
  printf '  %.0s─' {1..54}; echo ""
}

# Result is returned via $_PROMPT_RESULT to avoid command substitution
# capturing display output along with the value.
_PROMPT_RESULT=""

_prompt_value() {
  local name="$1" type="$2" rec="$3"
  _PROMPT_RESULT=""
  local current; current=$(_cfg_get "$name")

  echo ""
  echo -e "  ${BOLD}$name${NC}"
  [ -n "$current" ] && echo "  Current:     $current" || echo "  Current:     (not set)"
  [ "$rec" != "-" ] && echo "  Recommended: $rec"
  echo ""

  local _v _c
  case "$type" in
    bool)
      echo "  1) true"
      echo "  2) false"
      echo "  0) Cancel"
      echo ""
      read -r -p "  Choice: " _c
      case "$_c" in
        1) _PROMPT_RESULT="true" ;;
        2) _PROMPT_RESULT="false" ;;
        *) return 1 ;;
      esac
      ;;
    int)
      read -r -p "  Enter integer value: " _v
      [[ "$_v" =~ ^[0-9]+$ ]] || { echo "  Invalid — must be a whole number."; return 1; }
      _PROMPT_RESULT="$_v"
      ;;
    intfalse)
      read -r -p "  Enter integer (or 'false' to disable): " _v
      if [[ "$_v" =~ ^[0-9]+$ ]] || [ "$_v" = "false" ]; then
        _PROMPT_RESULT="$_v"
      else
        echo "  Invalid — enter a number or 'false'."; return 1
      fi
      ;;
    str)
      read -r -p "  Enter value: " _v
      [ -z "$_v" ] && { echo "  Cancelled."; return 1; }
      _PROMPT_RESULT="$_v"
      ;;
    core_update)
      echo "  1) minor  — security and minor releases only  (recommended)"
      echo "  2) true   — all core updates"
      echo "  3) false  — no automatic core updates"
      echo "  0) Cancel"
      echo ""
      read -r -p "  Choice: " _c
      case "$_c" in
        1) _PROMPT_RESULT="minor" ;;
        2) _PROMPT_RESULT="true" ;;
        3) _PROMPT_RESULT="false" ;;
        *) return 1 ;;
      esac
      ;;
    *)
      read -r -p "  Enter value: " _v
      _PROMPT_RESULT="$_v"
      ;;
  esac
}

_cfg_set() {
  local name="$1" value="$2" type="$3"
  local raw_flag=""

  case "$type" in
    bool|int|intfalse) raw_flag="--raw" ;;
    core_update)
      # 'minor' is a PHP string, true/false are PHP literals
      [ "$value" = "minor" ] && raw_flag="" || raw_flag="--raw"
      ;;
  esac

  if _wp config set "$name" "$value" $raw_flag --type=constant --quiet; then
    echo -e "  ${GREEN}✓ $name = $value${NC}"
  else
    echo -e "  ERROR: wp config set failed."
    return 1
  fi
}

# --- Main loop ---
while true; do
  _show_dashboard
  read -r -p "  Select (number to edit, 0 to exit): " choice
  echo ""

  [ "$choice" = "0" ] && { echo "  Bye!"; exit 0; }

  if ! [[ "$choice" =~ ^[0-9]+$ ]] || [ "$choice" -lt 1 ] || [ "$choice" -gt "${#_DEF_NAMES[@]}" ]; then
    echo "  Invalid."; sleep 1; continue
  fi

  name="${_DEF_NAMES[$((choice-1))]}"
  type=$(_def_field "$name" type)
  rec=$(_def_field "$name" rec)

  _prompt_value "$name" "$type" "$rec"
  [ $? -ne 0 ] && { sleep 1; continue; }
  [ -z "$_PROMPT_RESULT" ] && continue

  echo ""
  _cfg_set "$name" "$_PROMPT_RESULT" "$type"
  sleep 1
done
