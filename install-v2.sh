#!/bin/bash
# install-v2.sh — install hs on this box. Run as root from the checkout.
#
# Separate from install-scripts.sh (v1), which keeps deploying the v-scripts
# and the streamer unit exactly as before; v1 and v2 can both be run, in
# either order. See docs/hs-design.md.
#
#   bash install-v2.sh             hs on PATH, state + log dirs. Changes nothing else.
#   bash install-v2.sh --cron      also refresh saved check results every 15 min
#   bash install-v2.sh --serve     also run the streamer as `hs serve`
#   bash install-v2.sh --no-serve  put the streamer back on the old binary
#   bash install-v2.sh --no-cron   remove the refresh cron
#
# Every change is printed before it is made.
set -uo pipefail

[ "$EUID" -eq 0 ] || { echo "run as root"; exit 1; }

REPO=$(dirname "$(realpath "$0")")
HS="$REPO/hs"
UNIT=hestia-streamer
DROPIN_DIR=/etc/systemd/system/$UNIT.service.d
DROPIN=$DROPIN_DIR/hs-serve.conf
CRON=/etc/cron.d/hs

CRON_ON=0 CRON_OFF=0 SERVE_ON=0 SERVE_OFF=0
for a in "$@"; do
  case "$a" in
    --cron) CRON_ON=1 ;;
    --no-cron) CRON_OFF=1 ;;
    --serve) SERVE_ON=1 ;;
    --no-serve) SERVE_OFF=1 ;;
    -h|--help) sed -n '2,15p' "$0"; exit 0 ;;
    *) echo "unknown option $a (see --help)"; exit 2 ;;
  esac
done

run() { echo "  \$ $*"; "$@"; }
step() { echo; echo "# $*"; }

step "hs binary from this checkout"
[ -x "$HS" ] || chmod +x "$HS" 2>/dev/null
if ! out=$("$HS" version 2>&1); then
  echo "  ! $HS does not run: $out"
  echo "    (build it with ./build-hs.sh on a dev machine and commit it)"
  exit 1
fi
echo "  $out"

step "hs on PATH (a symlink, so every git pull updates it)"
run ln -sfn "$HS" /usr/local/bin/hs

step "state and action log (root-only)"
run install -d -m 700 /var/lib/hs /var/log/hs

if [ $CRON_ON -eq 1 ]; then
  step "refresh saved results every 15 min, so the dashboard opens on fresh data"
  echo "  writes $CRON:"
  line='*/15 * * * * root /usr/local/bin/hs check --sites --save >/dev/null 2>&1'
  echo "    $line"
  printf '# hs: keep /var/lib/hs/results.json fresh (install-v2.sh --cron)\n%s\n' "$line" > "$CRON"
  chmod 644 "$CRON"
  echo "  (restic and WordPress checksum probes keep their own minimum intervals)"
fi
if [ $CRON_OFF -eq 1 ] && [ -f "$CRON" ]; then
  step "remove the refresh cron"
  run rm -f "$CRON"
fi

streamer_answers() {
  local tok code
  tok=$(sed -n 's/^HESTIA_STREAMER_TOKEN=//p' /etc/hestia-streamer.env 2>/dev/null | tr -d "\"'")
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 -H "X-Streamer-Token: $tok" http://127.0.0.1:8091/netdata/alarms)
  # 200 = Netdata answered, 502 = streamer up but Netdata down; both prove the streamer.
  [ "$code" = 200 ] || [ "$code" = 502 ]
}

if [ $SERVE_ON -eq 1 ]; then
  step "run the streamer as 'hs serve' (same unit, port 8091 and token file)"
  systemctl cat $UNIT >/dev/null 2>&1 || { echo "  ! $UNIT is not installed — run install-scripts.sh first"; exit 1; }
  echo "  A systemd drop-in overrides only ExecStart, so install-scripts.sh (which"
  echo "  rewrites the unit file on every update) cannot silently undo it."
  echo "  writes $DROPIN:"
  printf '    [Service]\n    ExecStart=\n    ExecStart=/usr/local/bin/hs serve\n'
  install -d -m 755 "$DROPIN_DIR"
  printf '[Service]\nExecStart=\nExecStart=/usr/local/bin/hs serve\n' > "$DROPIN"
  run systemctl daemon-reload
  run systemctl restart $UNIT
  sleep 2
  if systemctl is-active --quiet $UNIT && streamer_answers; then
    echo "  ok: hs serve is answering with the token"
  else
    echo "  ! hs serve did not answer — rolling back to the old binary"
    run rm -f "$DROPIN"
    run systemctl daemon-reload
    run systemctl restart $UNIT
    journalctl -u $UNIT -n 20 --no-pager
    exit 1
  fi
fi
if [ $SERVE_OFF -eq 1 ]; then
  step "put the streamer back on the old binary"
  run rm -f "$DROPIN"
  run systemctl daemon-reload
  run systemctl restart $UNIT
  sleep 2
  streamer_answers && echo "  ok: the old streamer is answering" || echo "  ! the streamer does not answer — journalctl -u $UNIT"
fi

step "done"
echo "  hs          dashboard"
echo "  hs check    report in the terminal (hs help for the rest)"
[ -f "$DROPIN" ] && echo "  streamer:   hs serve" || echo "  streamer:   $(systemctl show $UNIT -p ExecStart --value 2>/dev/null | grep -o 'path=[^ ;]*' | head -1)"
[ -f "$CRON" ] && echo "  refresh:    every 15 min ($CRON)"
exit 0
