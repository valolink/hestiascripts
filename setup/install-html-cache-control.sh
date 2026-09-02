#!/bin/bash
# Install the http-level HTML Cache-Control drop-in into /etc/nginx/conf.d/.
#
# Unlike its neighbours in setup/, this file is EXECUTED, not sourced -- it is
# called both from install-scripts.sh (so v-hestiascripts-update rolls it out
# across the fleet unattended) and from run.sh -> 12 -> 2. It therefore carries
# its own root check and no dependency on setup/common.sh.
#
# See templates/nginx/valolink-html-cache-control.conf for what it installs and
# why. Idempotent: identical content is a no-op and skips the reload, which
# matters because v-hestiascripts-update runs this on every update.
#
# Exit: 0 installed or already current, 1 failed (config reverted).

set -u

REPO_DIR=$(cd "$(dirname "$(realpath "$0")")/.." && pwd)
SRC="$REPO_DIR/templates/nginx/valolink-html-cache-control.conf"
DST="/etc/nginx/conf.d/valolink-html-cache-control.conf"

if [ "$EUID" -ne 0 ]; then
  echo "  ERROR: must run as root."
  exit 1
fi

if [ ! -f "$SRC" ]; then
  echo "  Source not found: $SRC — skipping."
  exit 1
fi

if ! command -v nginx >/dev/null 2>&1 || [ ! -d "/etc/nginx/conf.d" ]; then
  echo "  No nginx on this box (binary or /etc/nginx/conf.d missing) — skipping."
  exit 0
fi

if cmp -s "$SRC" "$DST"; then
  echo "  Already current: $DST"
  exit 0
fi

# This lands at http level and applies to every vhost, so a broken file here
# takes the whole box down at the next reload — whether that reload is ours or
# a certbot renewal three weeks from now. Never leave an untested config in
# place: keep the previous version and put it back if nginx -t complains.
BACKUP=""
if [ -f "$DST" ]; then
  BACKUP="${DST}.prev"
  cp "$DST" "$BACKUP"
fi

cp "$SRC" "$DST"

if ! nginx -t >/dev/null 2>&1; then
  echo "  ERROR: 'nginx -t' failed with the new drop-in — reverting."
  if [ -n "$BACKUP" ]; then mv "$BACKUP" "$DST"; else rm -f "$DST"; fi
  nginx -t 2>&1 | sed 's/^/      /'
  exit 1
fi

[ -n "$BACKUP" ] && rm -f "$BACKUP"

if systemctl reload nginx >/dev/null 2>&1; then
  echo "  Installed $DST and reloaded nginx."
  echo "  HTML responses that set no Cache-Control of their own now get:"
  echo "      Cache-Control: no-cache, must-revalidate, max-age=0"
else
  echo "  Installed $DST, but 'systemctl reload nginx' failed."
  echo "  Config tests clean — reload manually to activate."
  exit 1
fi

exit 0
