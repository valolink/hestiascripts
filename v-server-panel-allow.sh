#!/bin/bash
# info: let one address reach the Hestia panel and phpMyAdmin for a while
# options: IP
#
# example: v-server-panel-allow 203.0.113.7
#
# EngineLink's "Open panel" button calls this through the streamer with the
# visitor's address. Runs `hs panel allow IP`: the address goes into the
# hs-panel/hs-panel6 ipset for PANEL_TTL seconds (8 h by default; again resets
# the clock), and phpMyAdmin's Apache allow list follows. With the gate off
# (hs op panel-gate) the panel is open to all and nothing is added.
# The v-server- prefix links it into Hestia's bin and lets the streamer run
# it. Final stdout line: one line of JSON
# {"gate":"on","ip":…,"ttl":…,"host":…,"hostResolves":…,"port":…[,"error":…]}.
# Exit 0 allowed (or gate off), 1 refused/failed, 3 hs missing.

HS=$(command -v hs || echo /usr/local/bin/hs)
if [ ! -x "$HS" ]; then
	echo "hs is not installed on this box (bash install-v2.sh in the hestiascripts checkout)"
	echo '{"gate":"unknown","host":"","hostResolves":false,"port":"","error":"hs not installed"}'
	exit 3
fi
if [ $# -ne 1 ]; then
	echo "usage: v-server-panel-allow IP"
	exit 1
fi
exec "$HS" panel allow "$1"
