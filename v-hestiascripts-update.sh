#!/bin/bash
# info: update the hestiascripts checkout and redeploy scripts + streamer
# options: [--allow-unsigned]   (terminal only)
#
# example: v-hestiascripts-update
#
# The new commit must be signed by a key listed in /etc/hs/allowed_signers
# (on the box, not in the repo) — otherwise nothing is changed.
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

# This script is streamer-allowlisted, so EngineLink can fire it from the server
# Operate tab with no terminal attached. If git decides to ask for credentials it
# writes the prompt to /dev/tty and blocks on stdin forever — the SSE request
# hangs instead of failing, and there is no one there to answer. GitHub 401s
# unauthenticated HTTPS fetches from datacenter IPs often enough for this to be
# a real hazard even on a public repo (seen on hestia and hzenergiatuote
# 2026-09-02). Fail fast and legibly instead.
export GIT_TERMINAL_PROMPT=0
export GIT_SSH_COMMAND='ssh -o BatchMode=yes'

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

# install-scripts.sh chmod +x's the sources, which git would otherwise see
# as a permanent mode-bit diff and this script would "stash" on every run.
git config core.fileMode false

OLD_REV=$(git rev-parse --short HEAD)

# Local drift is unexpected (deploys are git-managed) — stash it rather than
# fail, but show exactly what was stashed so it never disappears silently.
if [ -n "$(git status --porcelain)" ]; then
	echo "WARNING: local changes detected — stashing:"
	git status --short
	git stash push -u -m "v-hestiascripts-update $(date -Is)" > /dev/null
	echo "(recover with: cd $REPO_DIR && git stash pop)"
fi

# Signed updates (docs/security-hardening.md §13). This script runs as root
# and can be fired over the network: whoever can push to the repository would
# otherwise get root on every box at the next update. So the new commit must
# carry a signature from a key in /etc/hs/allowed_signers — a file on the box,
# outside the repository, so a push cannot change who is trusted. It is
# verified BEFORE anything is merged into the checkout.
ALLOWED_SIGNERS=/etc/hs/allowed_signers
ALLOW_UNSIGNED=false
[ "${1:-}" = "--allow-unsigned" ] && ALLOW_UNSIGNED=true
if [ "$ALLOW_UNSIGNED" = true ] && ! { [ -t 0 ] && [ -t 1 ]; }; then
	echo "REFUSED: --allow-unsigned only works from a terminal on the box, never over the streamer."
	exit 1
fi

echo "Fetching..."
if ! git fetch --quiet origin 2>&1; then
	# Two very different causes, and the operator needs to know which. With
	# terminal prompts disabled above, an auth/rate-limit rejection surfaces as
	# "could not read Username" rather than hanging — name it explicitly.
	echo "ERROR: git fetch failed."
	echo "  If the message above mentions 'could not read Username', GitHub"
	echo "  rejected an unauthenticated fetch (it does this to datacenter IPs"
	echo "  even for public repos). Retrying usually works; switching origin to"
	echo "  SSH fixes it for good:"
	echo "    git -C $REPO_DIR remote set-url origin git@github.com:valolink/hestiascripts.git"
	exit 1
fi
UPSTREAM=$(git rev-parse '@{u}' 2> /dev/null) || {
	echo "ERROR: the checkout's branch has no upstream"
	exit 1
}

if [ "$ALLOW_UNSIGNED" = true ]; then
	echo "WARNING: --allow-unsigned — installing $(git rev-parse --short "$UPSTREAM") without checking its signature."
elif [ ! -s "$ALLOWED_SIGNERS" ]; then
	echo "REFUSED: signed updates are not set up on this box ($ALLOWED_SIGNERS is missing)."
	echo "  Nothing was changed. Put the trusted signing key(s) there, one per line:"
	echo "    reima@valolink.fi ssh-ed25519 AAAA…"
	echo "  and sign commits (git config commit.gpgsign true, gpg.format ssh)."
	echo "  To update anyway from a terminal on the box: v-hestiascripts-update --allow-unsigned"
	exit 1
elif ! git -c gpg.format=ssh -c gpg.ssh.allowedSignersFile="$ALLOWED_SIGNERS" verify-commit "$UPSTREAM" 2>&1; then
	echo "REFUSED: $(git rev-parse --short "$UPSTREAM") is not signed by a key in $ALLOWED_SIGNERS."
	echo "  Nothing was changed. Whoever pushed it is not trusted to run code as root here."
	exit 1
else
	echo "Signature OK: $(git rev-parse --short "$UPSTREAM") is signed by a trusted key."
fi

echo "Merging (fast-forward only)..."
if ! git merge --ff-only --quiet "$UPSTREAM" 2>&1; then
	echo "ERROR: the local history has diverged from origin; it needs a manual look."
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
