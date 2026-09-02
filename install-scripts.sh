#!/bin/bash

# Ensure the script is run as root
if [ "$EUID" -ne 0 ]; then
  echo "❌ ERROR: Please run this script as root."
  exit 1
fi

# Dynamically get the directory where this install.sh script is located
GIT_DIR=$(dirname "$(realpath "$0")")
HESTIA_BIN="/usr/local/hestia/bin"
SYSTEMD_DIR="/etc/systemd/system"

echo "==================================================="
echo "  Deploying Custom Hestia Scripts & Streamer"
echo "  Source: $GIT_DIR"
echo "==================================================="

# Check if Hestia is actually installed on this server
if [ ! -d "$HESTIA_BIN" ]; then
  echo "❌ ERROR: HestiaCP bin directory ($HESTIA_BIN) not found."
  exit 1
fi

# --- 1. Symlink Bash Scripts ---
COUNT=0
echo "⚙️ Linking bash scripts..."

# Loop through all files starting with 'v-' in the Git directory
for SCRIPT in "$GIT_DIR"/v-*; do
  if [ -e "$SCRIPT" ]; then
    FILENAME=$(basename "$SCRIPT")

    # Make the source file executable
    chmod +x "$SCRIPT"

    # Strip the '.sh' extension for the symlink name (if it exists)
    DEST_NAME="${FILENAME%.sh}"
    DEST_PATH="$HESTIA_BIN/$DEST_NAME"

    # Create the symlink (-s for symlink, -f to force overwrite existing links)
    ln -sf "$SCRIPT" "$DEST_PATH"

    echo "  ✅ Linked: $FILENAME -> $DEST_NAME"
    ((COUNT++))
  fi
done

if [ "$COUNT" -eq 0 ]; then
  echo "  ⚠️ No 'v-' scripts found to deploy."
fi

# Remove symlinks in HESTIA_BIN that point into our repo but whose target no longer exists
STALE=0
for LINK in "$HESTIA_BIN"/v-*; do
  [ -L "$LINK" ] || continue
  TARGET=$(readlink "$LINK")
  if [[ "$TARGET" == "$GIT_DIR"* ]] && [ ! -e "$LINK" ]; then
    echo "  🗑️  Removed stale symlink: $(basename "$LINK")  (→ $TARGET)"
    rm "$LINK"
    ((STALE++))
  fi
done
[ "$STALE" -eq 0 ] && echo "  ✅ No stale symlinks found."

# --- 2. Register Go Binary Service ---
STREAMER_BIN="$GIT_DIR/hestia-streamer"

if [ -f "$STREAMER_BIN" ]; then
  echo "---------------------------------------------------"
  echo "⚙️ Configuring Go streaming service..."

  # Ensure the compiled binary is executable
  chmod +x "$STREAMER_BIN"

  # Shared-secret token: second factor on top of the firewall IP rule.
  # Generated once, persisted in /etc/hestia-streamer.env, loaded by the
  # unit below. Paste the printed token into EngineLink's server settings
  # (Streamer token field) — requests without it get a 403.
  STREAMER_ENV="/etc/hestia-streamer.env"
  if [ ! -f "$STREAMER_ENV" ]; then
    STREAMER_TOKEN=$(openssl rand -hex 24)
    echo "HESTIA_STREAMER_TOKEN=$STREAMER_TOKEN" > "$STREAMER_ENV"
    chmod 600 "$STREAMER_ENV"
    echo "  🔑 Generated new streamer token."
  else
    STREAMER_TOKEN=$(grep -oP '^HESTIA_STREAMER_TOKEN=\K.*' "$STREAMER_ENV")
    echo "  🔑 Using existing streamer token."
  fi
  echo "  ➡️  EngineLink server settings → Streamer token:"
  echo "      $STREAMER_TOKEN"

  # Create the systemd service file dynamically
  cat <<EOF >"$SYSTEMD_DIR/hestia-streamer.service"
[Unit]
Description=HestiaCP Go Realtime Streaming API
After=network.target

[Service]
Type=simple
User=root
EnvironmentFile=-$STREAMER_ENV
ExecStart=$STREAMER_BIN
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

  # Reload systemd configs, enable on boot, and restart
  systemctl daemon-reload
  systemctl enable hestia-streamer.service >/dev/null 2>&1
  systemctl restart hestia-streamer.service

  if systemctl is-active --quiet hestia-streamer; then
    echo "  🚀 Success! Go Streamer is running on port 8091."
  else
    echo "  ❌ ERROR: Go Streamer failed to start."
  fi
else
  echo "---------------------------------------------------"
  echo "⚠️ Go binary 'hestia-streamer' not found. Skipping service deployment."
fi

# --- 3. HestiaCP Local Firewall Configuration ---
echo "---------------------------------------------------"
echo "⚙️ Checking HestiaCP firewall rules..."

# IMPORTANT: Change this to your Nuxt Docker server's IP
NUXT_IP="135.181.26.201"

# Use absolute paths ($HESTIA_BIN) so bash never loses the command
if ! $HESTIA_BIN/v-list-firewall plain | grep -q "8091"; then
  echo "  🛡️ Opening port 8091 for Nuxt IP ($NUXT_IP)..."

  # Removed spaces from the comment to guarantee it passes Hestia's strict format validation
  $HESTIA_BIN/v-add-firewall-rule ACCEPT "$NUXT_IP" 8091 TCP "Nuxt_Streamer"

  if [ $? -eq 0 ]; then
    echo "  ✅ Firewall rule added successfully."
  else
    echo "  ❌ ERROR: Failed to add firewall rule. (Run manually in terminal to see the exact Hestia error)"
  fi
else
  echo "  ✅ Firewall rule for port 8091 already exists."
fi

# --- 4. Nginx HTML Cache-Control Drop-in ---
# Rolls out with every v-hestiascripts-update, so the fleet picks it up through
# the same channel as the scripts. Self-reverting on a failed nginx -t; a
# failure here is reported but never aborts the rest of the deployment.
echo "---------------------------------------------------"
echo "⚙️ Installing nginx HTML cache-control drop-in..."

CACHE_CONTROL_INSTALLER="$GIT_DIR/setup/install-html-cache-control.sh"

if [ -f "$CACHE_CONTROL_INSTALLER" ]; then
  if bash "$CACHE_CONTROL_INSTALLER"; then
    echo "  ✅ HTML cache-control drop-in is current."
  else
    echo "  ❌ HTML cache-control drop-in FAILED — see the output above."
  fi
else
  echo "  ⚠️ Installer not found: $CACHE_CONTROL_INSTALLER — skipping."
fi

echo "==================================================="
echo "🎉 Deployment completely finished!"
echo "==================================================="
