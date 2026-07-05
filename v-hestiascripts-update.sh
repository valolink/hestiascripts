#!/bin/bash
# info: update the hestiascripts checkout and redeploy scripts + streamer
# options: NONE
#
# example: v-hestiascripts-update
#
# Pulls the latest hestiascripts from git (fast-forward only) and re-runs
# install-scripts.sh. Designed to be driven from EngineLink over the
# streamer: install-scripts.sh restarts the hestia-streamer service, and a
# v-script is a child in the streamer's cgroup — the restart would kill the
# installer mid-flight. So the git work happens here (output streams back),
# and the installer is launched as a DETACHED systemd transient unit; this
# script then exits 0 and the streamer restarts a moment later.
#
# The repo is located by resolving this script's own symlink, so it works
# wherever the checkout lives (/root/hestiascripts, /hestiascripts, ...).

set -u

if [ "$EUID" -ne 0 ]; then
	echo "ERROR: must run as root"
	exit 1
fi

REPO_DIR=$(dirname "$(realpath "${BASH_SOURCE[0]}")")
cd "$REPO_DIR" || {
	echo "ERROR: cannot cd to $REPO_DIR"
	exit 1
}

if ! git rev-parse --is-inside-work-tree > /dev/null 2>&1; then
	echo "ERROR: $REPO_DIR is not a git checkout"
	exit 1
fi

echo "Repo: $REPO_DIR ($(git remote get-url origin 2> /dev/null || echo 'no remote'))"
OLD_REV=$(git rev-parse --short HEAD)

# Local drift is unexpected (deploys are git-managed) — stash it rather than
# fail, but show exactly what was stashed so it never disappears silently.
if [ -n "$(git status --porcelain)" ]; then
	echo "WARNING: local changes detected — stashing:"
	git status --short
	git stash push -u -m "v-hestiascripts-update $(date -Is)" > /dev/null
	echo "(recover with: cd $REPO_DIR && git stash pop)"
fi

echo "Pulling (fast-forward only)..."
if ! git pull --ff-only 2>&1; then
	echo "ERROR: git pull failed (diverged history needs a manual look)"
	exit 1
fi

NEW_REV=$(git rev-parse --short HEAD)
if [ "$OLD_REV" = "$NEW_REV" ]; then
	echo "Already up to date at $NEW_REV — redeploying anyway."
else
	echo "Updated $OLD_REV -> $NEW_REV:"
	git log --oneline "$OLD_REV..$NEW_REV" | head -20
fi

# Detach the installer so the streamer restart inside it can't kill it.
UNIT="hestiascripts-install-$$"
if ! systemd-run --collect --unit "$UNIT" bash "$REPO_DIR/install-scripts.sh" > /dev/null 2>&1; then
	echo "ERROR: failed to launch detached installer (systemd-run)"
	exit 1
fi

echo "Installer launched detached (journalctl -u $UNIT for its output)."
echo "The streamer restarts shortly — verify via the Setup tab in ~30s."
exit 0
