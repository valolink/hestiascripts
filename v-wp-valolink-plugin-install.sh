#!/bin/bash

if [ "$EUID" -ne 0 ]; then
  echo "❌ ERROR: Please run as root."
  exit 1
fi

export PATH=$PATH:/usr/local/hestia/bin

_REPO="valolink/valolink-plugin"
_API="https://api.github.com/repos/${_REPO}/releases/latest"

show_help() {
  cat <<EOF
USAGE: v-wp-valolink-plugin-install [OPTIONS]

Install (or upgrade) the latest published release of valolink-plugin from
https://github.com/${_REPO} into a WordPress site managed by HestiaCP.

The latest release is resolved via GitHub's API. A release asset ending in
.zip is preferred; if the release publishes only auto-generated source
archives, the zipball is used as a fallback.

OPTIONS:
  --user=USER      HestiaCP user who owns the site
  --domain=DOMAIN  Domain to install into
  --no-activate    Install but don't activate the plugin
  --print-key      After install, ensure the EngineLink API key exists
                   (generating it if needed) and print it as
                   ENGINELINK_API_KEY=<key>. Used by EngineLink for
                   zero-touch enrollment; treat the output as secret.
  -h, --help       Show this help

EXAMPLES:
  # Interactive
  v-wp-valolink-plugin-install

  # Direct (e.g. via hestia-streamer)
  v-wp-valolink-plugin-install --user=admin --domain=mysite.fi
EOF
}

# --- Flag Parsing ---
HESTIA_USER=""
DOMAIN=""
ACTIVATE=true
PRINT_KEY=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --user=*)     HESTIA_USER="${1#*=}" ;;
    --domain=*)   DOMAIN="${1#*=}" ;;
    --no-activate) ACTIVATE=false ;;
    --print-key)  PRINT_KEY=true ;;
    -h|--help)    show_help; exit 0 ;;
    *) echo "❌ ERROR: Unknown option: $1"; exit 1 ;;
  esac
  shift
done

# --- Interactive prompts for missing values ---
if [ -z "$HESTIA_USER" ]; then
  mapfile -t USERS < <(v-list-users plain | cut -f1)
  PS3="Select user: "
  select HESTIA_USER in "${USERS[@]}"; do
    [ -n "$HESTIA_USER" ] && break
    echo "Invalid selection."
  done
fi

if [ -z "$DOMAIN" ]; then
  mapfile -t DOMAINS < <(v-list-web-domains "$HESTIA_USER" plain | cut -f1)
  if [ ${#DOMAINS[@]} -eq 0 ]; then
    echo "❌ ERROR: No web domains found for user $HESTIA_USER."
    exit 1
  fi
  PS3="Select domain: "
  select DOMAIN in "${DOMAINS[@]}"; do
    [ -n "$DOMAIN" ] && break
    echo "Invalid selection."
  done
fi

DOMAIN="${DOMAIN#www.}"
WP_PATH="/home/$HESTIA_USER/web/$DOMAIN/public_html"
WP="sudo -u $HESTIA_USER wp --path=$WP_PATH"

if [ ! -f "$WP_PATH/wp-config.php" ]; then
  echo "❌ ERROR: No WordPress installation found at $WP_PATH"
  exit 1
fi

if ! command -v wp &>/dev/null; then
  echo "❌ ERROR: WP-CLI is not installed."
  exit 1
fi

echo ""
echo "======================================================"
echo "  valolink-plugin install: $DOMAIN"
echo "======================================================"

# --- Resolve latest release ---
echo ""
echo "  Querying ${_API} ..."
RELEASE_JSON=$(curl -sf --max-time 15 "$_API")
if [ -z "$RELEASE_JSON" ]; then
  echo "❌ ERROR: Failed to query GitHub (no published release, network down,"
  echo "         or rate-limited). Try again or use a personal access token."
  exit 1
fi

VERSION=$(echo "$RELEASE_JSON" | grep -oP '"tag_name":\s*"\K[^"]+' | head -n1)
[ -z "$VERSION" ] && VERSION="(unknown)"

# Prefer a real release asset ending in .zip; fall back to the source zipball.
ASSET_URL=$(echo "$RELEASE_JSON" \
  | grep -oP '"browser_download_url":\s*"\K[^"]+\.zip"?' \
  | tr -d '"' \
  | head -n1)

if [ -n "$ASSET_URL" ]; then
  SOURCE_LABEL="release asset"
else
  ASSET_URL=$(echo "$RELEASE_JSON" | grep -oP '"zipball_url":\s*"\K[^"]+' | head -n1)
  SOURCE_LABEL="source zipball (no release asset published)"
fi

if [ -z "$ASSET_URL" ]; then
  echo "❌ ERROR: Could not determine a download URL from the latest release."
  exit 1
fi

echo "  Latest tag: $VERSION"
echo "  Source:     $SOURCE_LABEL"
echo "  URL:        $ASSET_URL"
echo ""

# --- Install via wp-cli ---
# --force overwrites an existing install, giving us idempotent upgrade behaviour.
INSTALL_FLAGS="--force"
$ACTIVATE && INSTALL_FLAGS="$INSTALL_FLAGS --activate"

echo "  Installing into $WP_PATH ..."
$WP plugin install "$ASSET_URL" $INSTALL_FLAGS
if [ $? -ne 0 ]; then
  echo "❌ ERROR: wp plugin install failed."
  exit 1
fi

# --- EngineLink key handout (zero-touch enrollment) ---
# Mirrors the plugin's own ensure_api_key(): the key lives in the
# non-autoloaded valolink_settings option under ["enginelink"]["api_key"]
# and is generated as bin2hex(random_bytes(24)) if missing. Works whether
# or not the plugin is active — it's plain option storage.
if $PRINT_KEY; then
  echo ""
  echo "  Ensuring EngineLink API key ..."
  EL_KEY=$($WP eval '
    $s = get_option("valolink_settings");
    if (!is_array($s)) { $s = []; }
    if (empty($s["enginelink"]["api_key"])) {
      $s["enginelink"]["api_key"] = bin2hex(random_bytes(24));
      update_option("valolink_settings", $s, "no");
    }
    echo $s["enginelink"]["api_key"];
  ' 2>/dev/null)
  if [ -n "$EL_KEY" ]; then
    echo "ENGINELINK_API_KEY=$EL_KEY"
  else
    echo "⚠️  WARNING: Could not read or create the EngineLink API key."
  fi
fi

echo ""
echo "======================================================"
echo "  ✅ valolink-plugin $VERSION installed on $DOMAIN"
if $ACTIVATE; then
  echo "     Activated."
else
  echo "     Not activated (--no-activate)."
fi
echo "======================================================"
