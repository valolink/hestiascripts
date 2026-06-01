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

# --- 2. Register Go Binary Service ---
STREAMER_BIN="$GIT_DIR/hestia-streamer"

if [ -f "$STREAMER_BIN" ]; then
  echo "---------------------------------------------------"
  echo "⚙️ Configuring Go streaming service..."

  # Ensure the compiled binary is executable
  chmod +x "$STREAMER_BIN"

  # Create the systemd service file dynamically
  cat <<EOF >"$SYSTEMD_DIR/hestia-streamer.service"
[Unit]
Description=HestiaCP Go Realtime Streaming API
After=network.target

[Service]
Type=simple
User=root
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
    echo "  🚀 Success! Go Streamer is running on port 8084."
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

# Check if port 8090 is already allowed in Hestia
if ! v-list-firewall plain | grep -q "8090"; then
  echo "  🛡️ Opening port 8090 for Nuxt IP ($NUXT_IP)..."

  # Hestia command: v-add-firewall-rule ACTION IP PORT PROTOCOL [COMMENT]
  v-add-firewall-rule ACCEPT "$NUXT_IP" 8090 TCP "Nuxt Streamer Endpoint"

  if [ $? -eq 0 ]; then
    echo "  ✅ Firewall rule added successfully."
  else
    echo "  ❌ ERROR: Failed to add firewall rule."
  fi
else
  echo "  ✅ Firewall rule for port 8090 already exists."
fi

echo "==================================================="
echo "🎉 Deployment completely finished!"
echo "==================================================="
