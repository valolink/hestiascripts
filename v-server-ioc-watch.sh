#!/bin/bash
# info: security watch — report findings that are new since the accepted state
# options: [--sweep] [--full]
#
# example: v-server-ioc-watch --sweep
#
# Runs `hs watch` (docs/security-hardening.md, "The watch"): the persistence,
# indicator, key, firewall and web-root checks, reporting only findings that
# are new since someone ran `hs watch --baseline`. Read-only on the box apart
# from hs's own state; never mails from here (cron does, with --mail).
# The v-server- prefix links it into Hestia's bin and lets EngineLink call it
# through the streamer. Final stdout line: one line of JSON
# {"new":[…],"critical":N,"warn":N,…}. Exit 0 nothing new, 1 new warning,
# 2 new critical, 3 hs missing.

HS=$(command -v hs || echo /usr/local/bin/hs)
if [ ! -x "$HS" ]; then
	echo "hs is not installed on this box (bash install-v2.sh in the hestiascripts checkout)"
	echo '{"new":[],"critical":0,"warn":0,"error":"hs not installed"}'
	exit 3
fi
args=()
for a in "$@"; do
	case "$a" in
	--sweep | --full) args+=("$a") ;;
	*)
		echo "unknown option: $a (allowed: --sweep --full)"
		exit 1
		;;
	esac
done
exec "$HS" watch "${args[@]}"
