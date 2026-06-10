#!/bin/bash
# v-wp-revisions-clean — remove excess post revisions using WP-CLI (no raw SQL)

if [ "$EUID" -ne 0 ]; then
  echo "ERROR: Please run as root."
  exit 1
fi

export PATH=$PATH:/usr/local/hestia/bin

show_help() {
  cat <<'EOF'
USAGE: v-wp-revisions-clean [OPTIONS]

Remove excess post revisions from a WordPress database.
Uses WP-CLI only — no raw SQL queries.

Keeps the N most recent revisions per post/page. Posts with N or fewer
revisions are not touched. Autosaves count toward the limit.

By default reads WP_POST_REVISIONS from wp-config as the keep count.

OPTIONS:
  --user=USER       HestiaCP user who owns the site
  --domain=DOMAIN   Domain name
  --keep=N          Revisions to keep per post (0 = delete all)
  --dry-run         Show counts and what would be deleted, no changes
  -h, --help        Show this help

EXAMPLES:
  v-wp-revisions-clean --user=client1 --domain=example.com
  v-wp-revisions-clean --user=client1 --domain=example.com --keep=5
  v-wp-revisions-clean --user=client1 --domain=example.com --keep=0 --dry-run
EOF
}

WEB_USER=""
DOMAIN=""
KEEP=""
DRY_RUN=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --user=*)   WEB_USER="${1#*=}" ;;
    --domain=*) DOMAIN="${1#*=}" ;;
    --keep=*)   KEEP="${1#*=}" ;;
    --dry-run)  DRY_RUN=true ;;
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

_wp() { sudo -u "$WEB_USER" wp --path="$WEB_DIR" --skip-plugins --skip-themes "$@" 2>/dev/null; }

# --- Determine keep count ---
if [ -z "$KEEP" ]; then
  cfg_val=$(_wp config get WP_POST_REVISIONS --type=constant 2>/dev/null)
  # WP_POST_REVISIONS = false means revisions disabled (keep 0 new ones, but
  # existing ones are not auto-cleaned — treat as keep=0 here)
  if [ "$cfg_val" = "false" ]; then
    KEEP=0
    echo ""
    echo "  WP_POST_REVISIONS = false (revisions disabled) → keeping 0"
  elif [[ "$cfg_val" =~ ^[0-9]+$ ]]; then
    KEEP="$cfg_val"
    echo ""
    echo "  WP_POST_REVISIONS = $KEEP (read from wp-config)"
  else
    echo ""
    read -r -p "  Revisions to keep per post [5]: " _k
    KEEP="${_k:-5}"
  fi
fi

if ! [[ "$KEEP" =~ ^[0-9]+$ ]]; then
  echo "ERROR: --keep must be a non-negative integer."
  exit 1
fi

echo ""
echo "==================================================="
echo "  Revision Cleanup — $DOMAIN"
echo "==================================================="
echo "  Keep per post: $KEEP"
[ "$DRY_RUN" = true ] && echo "  Mode:          dry run"
echo ""

# --- Count total revisions ---
echo "  Fetching revision list..."
total=$(_wp post list --post_type=revision --format=count)

if [ "${total:-0}" -eq 0 ]; then
  echo "  No revisions found. Nothing to do."
  exit 0
fi

echo "  Total revisions: $total"
echo ""

# --- Find which revisions to delete ---
# Fetch all revision IDs with their parent post ID, sorted by ID descending
# (higher ID = newer revision, since IDs are auto-incrementing).
# Tracking count per parent as we go gives us "keep the N most recent" logic
# without needing a separate WP-CLI call per post.
echo "  Analysing..."
mapfile -t rev_lines < <(_wp post list \
  --post_type=revision \
  --fields=ID,post_parent \
  --format=csv \
  --orderby=ID \
  --order=DESC \
  --no-paging | tail -n +2 | grep -v '^$')

to_delete=()
declare -A parent_count

for line in "${rev_lines[@]}"; do
  IFS=',' read -r rev_id parent_id <<< "$line"
  parent_count[$parent_id]=$(( ${parent_count[$parent_id]:-0} + 1 ))
  if [ "${parent_count[$parent_id]}" -gt "$KEEP" ]; then
    to_delete+=("$rev_id")
  fi
done

keep_count=$(( total - ${#to_delete[@]} ))
echo "  Revisions to keep:   $keep_count"
echo "  Revisions to delete: ${#to_delete[@]}"
echo ""

if [ "${#to_delete[@]}" -eq 0 ]; then
  echo "  All posts already have $KEEP or fewer revisions. Nothing to do."
  exit 0
fi

if [ "$DRY_RUN" = true ]; then
  echo "  Dry run — revision IDs that would be deleted:"
  printf '  %s\n' "${to_delete[@]}" | head -20
  [ "${#to_delete[@]}" -gt 20 ] && echo "  ... and $(( ${#to_delete[@]} - 20 )) more"
  echo ""
  echo "  Run without --dry-run to apply."
  exit 0
fi

read -r -p "  Delete ${#to_delete[@]} revisions? [y/N] " _ans
[[ "$_ans" =~ ^[Yy]$ ]] || { echo "  Cancelled."; exit 0; }

echo ""
echo "  Deleting..."

# Batch in groups of 50 to avoid argument list limits
batch=()
deleted=0
for rev_id in "${to_delete[@]}"; do
  batch+=("$rev_id")
  if [ "${#batch[@]}" -ge 50 ]; then
    _wp post delete "${batch[@]}" --force --quiet
    deleted=$(( deleted + ${#batch[@]} ))
    batch=()
    printf "\r  Progress: %d / %d" "$deleted" "${#to_delete[@]}"
  fi
done
if [ "${#batch[@]}" -gt 0 ]; then
  _wp post delete "${batch[@]}" --force --quiet
  deleted=$(( deleted + ${#batch[@]} ))
fi
printf "\r  Progress: %d / %d\n" "$deleted" "${#to_delete[@]}"

echo ""
echo "  Flushing cache..."
_wp cache flush --quiet

echo ""
echo "  Done. $deleted revisions deleted."
echo ""
echo "  Tip: run 'wp db optimize' to reclaim DB space freed by deletions."
