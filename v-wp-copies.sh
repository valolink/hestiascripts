#!/bin/bash
# v-wp-copies — list the copies of WordPress sites on this box (staging, clones)
#
# EngineLink runs this to learn about copies made on the server as well as the
# ones it made itself. Sources, in order:
#   1. The copy registry /root/.hestia-site-copies/<copy-domain>.conf, written
#      by v-wp-staging-create and v-wp-clone-site (and by --mark below).
#   2. v-wp-staging-create's older staged-URL store /root/.hestia-staging-urls/
#      <src-user>_<src-domain> (one staging copy per live site), for copies made
#      before the registry existed.
#   3. Any other WordPress whose wp-config.php sets WP_ENVIRONMENT_TYPE to
#      staging or development: a copy whose source is unknown — register it with
#      --mark so EngineLink can place it.
#
# Read-only unless --mark / --forget. Root-only files, as the staged-URL store:
# nothing here is readable from a site's PHP.

if [ "$EUID" -ne 0 ]; then
  echo "ERROR: Please run as root."
  exit 1
fi

REGISTRY="/root/.hestia-site-copies"
URL_STORE_DIR="/root/.hestia-staging-urls"
DOMAIN_RE='^[a-z0-9][a-z0-9.-]{2,250}$'
USER_RE='^[a-z0-9][a-z0-9_-]{0,31}$'

show_help() {
  cat <<'EOF'
USAGE: v-wp-copies [json]
       v-wp-copies --mark --domain=COPY --source-domain=LIVE [--kind=staging|clone]
       v-wp-copies --forget --domain=COPY
       v-wp-copies --auth --domain=COPY

Without options: lists every copy on this box, one line each; with the
argument "json" as one JSON document (EngineLink's format).

  --mark            Register an existing copy (made before the registry, or by
                    hand). Users are looked up from the domains. Default kind:
                    staging if the copy's WP_ENVIRONMENT_TYPE is staging, else clone.
  --forget          Remove a copy's registry entry (the site itself is untouched).
  --auth            The HTTP basic-auth pair of one copy this box lists, for
                    EngineLink's Credentials button: one line,
                    STAGING_AUTH=<user>:<password> (stored by
                    v-wp-staging-create), STAGING_AUTH_NONE (no password on the
                    domain) or STAGING_AUTH_UNKNOWN (protected, but the
                    password was set elsewhere and only its hash exists).
                    Treat the output as secret.
  -h, --help        Show this help
EOF
}

MODE="list"; FORMAT="text"; DOMAIN=""; SOURCE_DOMAIN=""; KIND=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    json)              FORMAT="json" ;;
    --mark)            MODE="mark" ;;
    --forget)          MODE="forget" ;;
    --auth)            MODE="auth" ;;
    --domain=*)        DOMAIN="${1#*=}" ;;
    --source-domain=*) SOURCE_DOMAIN="${1#*=}" ;;
    --kind=*)          KIND="${1#*=}" ;;
    -h|--help)         show_help; exit 0 ;;
    *) echo "ERROR: Unknown option: $1"; show_help; exit 1 ;;
  esac
  shift
done

# The Hestia user that owns a web domain, from its directory.
owner_of() {
  local d
  for d in /home/*/web/"$1"; do
    [ -d "$d" ] || continue
    d="${d#/home/}"; echo "${d%%/*}"; return 0
  done
  return 1
}

# WP_ENVIRONMENT_TYPE from a wp-config.php, without loading WordPress.
env_type() {
  grep -oP "define\\(\\s*['\"]WP_ENVIRONMENT_TYPE['\"]\\s*,\\s*['\"]\\K[a-z]+" "$1" 2>/dev/null | head -1
}

# One registry value; files are KEY=value lines written by our scripts only.
reg_get() {
  grep -m1 "^$2=" "$1" 2>/dev/null | cut -d= -f2- | tr -d '\r'
}

if [ "$MODE" = "auth" ]; then
  [[ "$DOMAIN" =~ $DOMAIN_RE ]] || { echo "ERROR: --domain is required and must be a hostname."; exit 1; }
  COPY_USER=$(owner_of "$DOMAIN") || { echo "ERROR: $DOMAIN is not a web domain on this box."; exit 1; }
  # Only copies: a registered one, or a WordPress whose wp-config says
  # staging/development — never an arbitrary live site's settings.
  if [ ! -f "$REGISTRY/$DOMAIN.conf" ]; then
    case "$(env_type "/home/$COPY_USER/web/$DOMAIN/public_html/wp-config.php")" in
      staging|development) ;;
      *) echo "ERROR: $DOMAIN is not a known copy on this box."; exit 1 ;;
    esac
  fi
  AUTH_FILE="/root/.hestia-staging-auth/$DOMAIN"
  if [ -s "$AUTH_FILE" ]; then
    read -r AUTH_U AUTH_P < "$AUTH_FILE"
    echo "STAGING_AUTH=${AUTH_U}:${AUTH_P}"
  elif grep "DOMAIN='$DOMAIN'" "/usr/local/hestia/data/users/$COPY_USER/web.conf" 2>/dev/null \
      | grep -q "AUTH_USER='[^']"; then
    echo "STAGING_AUTH_UNKNOWN"
  else
    echo "STAGING_AUTH_NONE"
  fi
  exit 0
fi

if [ "$MODE" = "mark" ] || [ "$MODE" = "forget" ]; then
  [[ "$DOMAIN" =~ $DOMAIN_RE ]] || { echo "ERROR: --domain is required and must be a hostname."; exit 1; }
  if [ "$MODE" = "forget" ]; then
    if rm -f "$REGISTRY/$DOMAIN.conf"; then echo "✅ Forgot $DOMAIN."; fi
    exit 0
  fi
  [[ "$SOURCE_DOMAIN" =~ $DOMAIN_RE ]] || { echo "ERROR: --source-domain is required and must be a hostname."; exit 1; }
  COPY_USER=$(owner_of "$DOMAIN") || { echo "ERROR: $DOMAIN is not a web domain on this box."; exit 1; }
  SOURCE_USER=$(owner_of "$SOURCE_DOMAIN") || { echo "ERROR: $SOURCE_DOMAIN is not a web domain on this box."; exit 1; }
  if [ -z "$KIND" ]; then
    [ "$(env_type "/home/$COPY_USER/web/$DOMAIN/public_html/wp-config.php")" = "staging" ] && KIND="staging" || KIND="clone"
  fi
  [[ "$KIND" =~ ^(staging|clone)$ ]] || { echo "ERROR: --kind must be staging or clone."; exit 1; }
  mkdir -p "$REGISTRY" && chmod 700 "$REGISTRY"
  cat > "$REGISTRY/$DOMAIN.conf" <<EOF
KIND=$KIND
DOMAIN=$DOMAIN
USER=$COPY_USER
SOURCE_DOMAIN=$SOURCE_DOMAIN
SOURCE_USER=$SOURCE_USER
CREATED=$(date -u -r "/home/$COPY_USER/web/$DOMAIN/public_html" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null)
SCRIPT=v-wp-copies --mark
EOF
  chmod 600 "$REGISTRY/$DOMAIN.conf"
  echo "✅ Registered $DOMAIN ($COPY_USER) as a $KIND of $SOURCE_DOMAIN ($SOURCE_USER)."
  exit 0
fi

# --- List ---
declare -A SEEN
ROWS=()   # kind|domain|user|source_domain|source_user|created|environment|exists|registered

add_row() { # kind domain user source_domain source_user created registered
  local dir="/home/$3/web/$2/public_html" exists=false env=""
  [ -d "$dir" ] && exists=true
  [ -f "$dir/wp-config.php" ] && env=$(env_type "$dir/wp-config.php")
  SEEN["$2"]=1
  ROWS+=("$1|$2|$3|$4|$5|$6|$env|$exists|$7")
}

# 1. Registry.
for f in "$REGISTRY"/*.conf; do
  [ -f "$f" ] || continue
  kind=$(reg_get "$f" KIND); domain=$(reg_get "$f" DOMAIN); user=$(reg_get "$f" USER)
  sdomain=$(reg_get "$f" SOURCE_DOMAIN); suser=$(reg_get "$f" SOURCE_USER); created=$(reg_get "$f" CREATED)
  [[ "$domain" =~ $DOMAIN_RE && "$user" =~ $USER_RE && "$kind" =~ ^(staging|clone)$ ]] || continue
  [[ "$sdomain" =~ $DOMAIN_RE ]] || sdomain=""
  [[ "$suser" =~ $USER_RE ]] || suser=""
  [[ "$created" =~ ^[0-9T:Z-]+$ ]] || created=""
  add_row "$kind" "$domain" "$user" "$sdomain" "$suser" "$created" true
done

# 2. The staged-URL store (staging copies made before the registry).
for f in "$URL_STORE_DIR"/*; do
  [ -f "$f" ] || continue
  name=$(basename "$f"); suser="${name%%_*}"; sdomain="${name#*_}"
  url=$(tr -d '[:space:]' < "$f"); domain="${url#https://}"; domain="${domain#http://}"; domain="${domain%%/*}"
  [[ "$domain" =~ $DOMAIN_RE && "$sdomain" =~ $DOMAIN_RE && "$suser" =~ $USER_RE ]] || continue
  [ -n "${SEEN[$domain]}" ] && continue
  user=$(owner_of "$domain") || user=""
  [[ "$user" =~ $USER_RE ]] || continue   # the copy is gone
  add_row staging "$domain" "$user" "$sdomain" "$suser" "$(date -u -r "$f" +%Y-%m-%dT%H:%M:%SZ)" false
done

# 3. Unregistered WordPress copies, by environment type.
for cfg in /home/*/web/*/public_html/wp-config.php; do
  [ -f "$cfg" ] || continue
  rest="${cfg#/home/}"; user="${rest%%/*}"; rest="${rest#*/web/}"; domain="${rest%%/*}"
  [ -n "${SEEN[$domain]}" ] && continue
  env=$(env_type "$cfg")
  [[ "$env" =~ ^(staging|development)$ ]] || continue
  [[ "$domain" =~ $DOMAIN_RE && "$user" =~ $USER_RE ]] || continue
  kind=clone; [ "$env" = "staging" ] && kind=staging
  add_row "$kind" "$domain" "$user" "" "" "" false
done

if [ "$FORMAT" = "json" ]; then
  out='{"copies":['; sep=""
  for r in "${ROWS[@]}"; do
    IFS='|' read -r kind domain user sdomain suser created env exists registered <<< "$r"
    out+="$sep{\"kind\":\"$kind\",\"domain\":\"$domain\",\"user\":\"$user\",\"sourceDomain\":\"$sdomain\",\"sourceUser\":\"$suser\",\"created\":\"$created\",\"environment\":\"$env\",\"exists\":$exists,\"registered\":$registered}"
    sep=","
  done
  echo "$out]}"
  exit 0
fi

if [ ${#ROWS[@]} -eq 0 ]; then echo "No copies on this box."; exit 0; fi
for r in "${ROWS[@]}"; do
  IFS='|' read -r kind domain user sdomain suser created env exists registered <<< "$r"
  printf '%-8s %-40s %-12s of %-30s %s%s%s\n' "$kind" "$domain" "($user)" "${sdomain:-?}" "${created:-}" \
    "$([ "$exists" = true ] || echo '  [site gone]')" "$([ "$registered" = true ] || echo '  [unregistered]')"
done
