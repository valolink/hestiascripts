#!/bin/bash
# v-wp-db-export — dump a WordPress site's database to $BACKUP as .sql.gz
#
# Resolves the site's DB from wp-config (wp config get DB_NAME), then hands
# the dump to stock v-dump-database (runs as root, gzip, writes to $BACKUP,
# schedules its own 1h at-cleanup). Contract for EngineLink: the FINAL
# stdout line is the archive's basename — same shape as v-dump-site — which
# EngineLink then pulls via the streamer's /download endpoint.
#
# Exists because stock v-run-cli-cmd hardcodes wp to ~/.wp-cli/wp (per-user
# installs); our boxes install WP-CLI globally (setup/wpcli.sh).

if [ "$EUID" -ne 0 ]; then
  echo "ERROR: Please run as root."
  exit 1
fi

export PATH=$PATH:/usr/local/hestia/bin

show_help() {
  cat <<'EOF'
USAGE: v-wp-db-export --user=USER --domain=DOMAIN

Dump the WordPress database of a HestiaCP-hosted site into Hestia's backup
directory as gzipped SQL. Prints the archive basename as the last line.
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

if [ -z "$WEB_USER" ] || [ -z "$DOMAIN" ]; then
  echo "ERROR: --user and --domain are required (non-interactive script)."
  exit 1
fi

DOMAIN="${DOMAIN#www.}"
WEB_DIR="/home/$WEB_USER/web/$DOMAIN/public_html"

if [ ! -f "$WEB_DIR/wp-config.php" ]; then
  echo "ERROR: $WEB_DIR does not look like a WordPress install."
  exit 1
fi

echo "Resolving database from wp-config..."
DB_NAME=$(sudo -u "$WEB_USER" wp --path="$WEB_DIR" config get DB_NAME 2>/dev/null)
if [ -z "$DB_NAME" ]; then
  echo "ERROR: could not read DB_NAME via WP-CLI."
  exit 1
fi
echo "Database: $DB_NAME"

echo "Dumping (gzip)..."
DUMP_PATH=$(v-dump-database "$WEB_USER" "$DB_NAME" file gzip | tail -1)
if [ -z "$DUMP_PATH" ] || [ ! -f "$DUMP_PATH" ]; then
  echo "ERROR: v-dump-database did not produce a file."
  exit 1
fi

echo "Dump ready: $(du -h "$DUMP_PATH" | cut -f1 | tr -d ' ')"
basename "$DUMP_PATH"
