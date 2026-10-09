#!/bin/bash
# v-server-health — one-shot server health snapshot for EngineLink's daily
# Layer-2 monitoring: restic backup freshness per Hestia user, core service
# status, mail queue depth, root disk, and apt repository health.
#
# Contract with EngineLink (server/utils/serverMonitor.ts): the LAST non-empty
# stdout line MUST be a single-line JSON object; exit 0 on a completed run.
#
# Since 2026-10-09 this is a wrapper around `hs compat health` (hs-design.md,
# "compat"). The bash version joined the restic repo as ${REPO}${user} while
# Hestia >= 1.10.4 stores REPO without the trailing slash, so every restic
# probe failed and it silently reported the local tarball's age instead:
# kuumalahde (no tarballs) reported nothing, and boxes whose restic was broken
# (web1, viona, hzdemolink) looked fresh. Backups now count only when restic
# has a snapshot; every user is listed, with "ageHours": null and an "error"
# when it has none.

if [ "$EUID" -ne 0 ]; then
  echo "ERROR: Please run as root."
  exit 1
fi

HS=$(command -v hs 2>/dev/null || echo /usr/local/bin/hs)
if [ ! -x "$HS" ]; then
  echo "ERROR: hs is not installed on this box (bash install-v2.sh in /root/hestiascripts)."
  exit 1
fi
exec "$HS" compat health
