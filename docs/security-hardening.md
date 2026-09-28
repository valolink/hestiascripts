# Security hardening for the Hestia fleet

Written 2026-09-25, after the May 2026 compromise of three boxes, and built the same day. Every
control here is permanent: it is part of how a box is run, not a stopgap. Each item says why it
matters (with the evidence), what hs does about it — the check that reports it and the operation or
fix that applies it — and what is still open. Incident details: `~/valolink/hzdemolink-incident-2026-09-25.md`.

Applying it to a box: `git pull` in the checkout, `bash install-v2.sh --watch`, then work through
`hs check --section security` (every finding opens its operation on enter in the TUI). All
operations show their exact commands, and how to undo them, before anything runs.

## What happened, in one paragraph

Between 2026-05-20 and 05-26, ten IP addresses opened 62 unauthenticated root shells through the
Hestia **web terminal** (`hestia-web-terminal` 1.0.2, `/_shell/` on the panel port) on hzdemolink,
alavus and hzweb1. The 1.0.2 `server.js` read the PHP session file with a string split, so a session
that had only loaded `/login/` was accepted as root (upstream fixed it in hestiacp commit `854d71b3c`;
advisory GHSA-gh6f-9gpr-x9m2). Automated tooling then grepped the disk for cloud and API keys, read
`/root/.my.cnf`, installed a **Realm C2 agent** (`ulibd`, callback `195.72.61.165:8000`), a second
agent disguised as a systemd helper, a C++ pivot/rootkit tool, an XMRig miner, and dropped a
password-gated PHP webshell into every web root. Nothing noticed for four months. On two boxes the web
terminal was still installed and running on 2026-09-25, and all three boxes held the operator's
unencrypted personal SSH key, which also opened root on most of the fleet and push access to this
repository on GitHub. The same attack hit other Hestia operators: a forum report from 2026-05-22
shows the same `/login/` → `/_shell/` sequence from 45.86.230.242 and the same rootkit files
(`/usr/lib/__hesti`, `libnss_cache.so.2` via `/etc/ld.so.preload`).

## What would have changed the outcome

| Control | Effect on this incident | In hs |
|---|---|---|
| Panel (and its web terminal) reachable only from the admin network | The bug is unreachable; nothing else follows. | `panel-restrict` |
| Web terminal off unless in use | Same, on every box, even with the panel public. | check `hestia.web-terminal`, fix `web-terminal-off` |
| Outbound traffic limited to known ports | The C2 (:8000, :50051) and the mining pool (:8444) never connect. | `egress` |
| No personal private keys on servers | The breach stays on three boxes instead of reaching the fleet and GitHub. | check `ssh.private-keys` |
| Persistence / unknown-binary checks, with alerts | Detection within a day instead of four months. | `persist.*`, `hs watch` |
| Logs kept long, ideally off-box | The first shells and every download could have been reconstructed. | `log-retention`, `audit` |
| No dumps or archives in web roots | Two customer databases and a site backup not downloadable by scanners. | template deny rules, `dumps` fix |

## HestiaCP's own vulnerabilities (researched 2026-09-25)

GitHub lists 18 security advisories for HestiaCP, all published July–September 2026 (verified
against `api.github.com/repos/hestiacp/hestiacp/security-advisories`). Seven are critical; most
turn **any panel login** — even a low-privilege hosting user — into root. That changes what a panel
password is worth: every panel account is effectively a root credential until the box runs the
fixed release.

| Advisory | Published | Severity | Fixed in | What |
|---|---|---|---|---|
| GHSA-gh6f-9gpr-x9m2 | 07-17 | critical | 1.9.6 | unauthenticated RCE via the web terminal (the May 2026 entry point) |
| GHSA-5fpv-c8rg-x6r3 | 07-17 | critical | 1.9.5 | client → root via newline injection in v-add-cron-job |
| GHSA-w3mx-xq85-8qqc | 07-17 | critical | 1.9.5 | root RCE via double eval() in parse_object_kv_list() |
| GHSA-cr7q-frhq-xw4v | 07-17 | critical | 1.9.7 | low-privilege → root via web.conf path fields |
| GHSA-2xw3-7h62-v4gf | 07-30 | critical | 1.9.8 | low-privilege → root via the restic restore queue |
| GHSA-xffx-jj33-p2px | 08-05 | critical | 1.9.9 | low-privilege → root via v-update-user-backup-exclusions |
| GHSA-r9q4-pmcm-5qqf | 09-15 | critical | 1.10.5 | local privilege escalation to root via a config write |
| GHSA-47mf-74xr-f8x9 | 07-17 | high | 1.9.5 | second-order command injection in the queue → root |
| GHSA-fcq6-p8cj-xx3c | 07-17 | high | 1.9.7 | authenticated admin takeover |
| GHSA-8w7m-g9c2-9q9p | 07-17 | high | 1.9.7 | SQL injection in the database password |
| GHSA-73p3-rqpv-59wx | 07-17 | high | 1.9.4 | IP spoofing via CF-Connecting-IP (defeats IP-based limits) |
| GHSA-fg7j-gpvw-2m73 | 07-17 | high | 1.9.5 | XSS in the panel |
| GHSA-c69h-jgpw-h9cj | 07-30 | medium | 1.9.8 | any admin account can take over the root user |
| GHSA-3g4r-pfpf-8697 | 07-30 | medium | 1.9.8 | stored XSS in notifications |
| GHSA-pr4w-cfq5-99h2 | 08-24 | medium | 1.10.4 / 1.9.10 | restore another user's backup into your account |
| GHSA-8c5w-r7fj-qrqc, GHSA-2hxh-jhww-923x, GHSA-vp7q-fjv5-5vg8 | 09-15 | medium | 1.10.5 | suspended-user page leak, stale admin role, cron impersonation |

Older: CVE-2025-30007/30008 (DNS record command injection / XSS, fixed 1.9.5), CVE-2023-3479 (XSS,
1.7.8), CVE-2022-2550 (DokuWiki installer command injection, 1.6.5), CVE-2021-47871 (API file write,
after 1.3.2). No CISA KEV entry. The web-terminal advisory's CVE number differs between GitHub
(CVE-2026-84976, not resolvable at MITRE) and VulnCheck (CVE-2026-43633); the GHSA id is the
reference.

**In hs:** check `hestia.advisories` compares the installed version with the advisory list — fetched
from GitHub at most twice a day, with the verified list built in as a fallback — and fails while any
critical one applies (fix: `v-update-sys-hestia-all`, to **1.10.5 or later**). `hestia.2fa` reports
panel users without two-factor login (the root user and admins individually, as Fail); `hestia.api`
fails when the API is on and open to every address. The API check follows the source, not the
research summary: an empty `API_ALLOWED_IP` denies every address (`web/api/index.php` compares the
client with the list plus ""); only `allow-all` opens it.

## The watch

Something has to notice a return without anyone remembering to look. `hs watch` runs the security
checks from cron, stays silent while nothing changes, and reports — and mails — only what is new
since a person last accepted the state of the box.

- Every 15 minutes: web terminal, indicators, processes, outbound connections, accounts, cron,
  authorized and private SSH keys, IPv6 firewall, egress, Hestia advisories and API, exposed files,
  signed updates. Hourly (`--sweep`): unpackaged binaries and units, webshells, root-owned PHP, PHP in
  uploads, package integrity (weekly by its own interval). Both under `flock`.
- `hs watch --baseline` accepts the current findings after review (operation `watch-baseline`, which
  lists them first). New findings are mailed to `MAIL_TO` in `/etc/hs/watch.conf`
  (**tuotanto@valolink.fi** by default), at most once a day each while they last; a finding that goes
  away and comes back is mailed at once. A failed send is itself reported by the next run, so a
  broken relay cannot silence the watch.
- The last output line is one line of JSON (`{"new":[…],"critical":N,"warn":N,…}`), exit 2 new
  critical / 1 new warning / 0 nothing new. `v-server-ioc-watch` runs it for EngineLink over the
  streamer (report only: no mail, no baseline from there).
- Install: `bash install-v2.sh --watch`, or operation `watch-enable`.

The indicators (paths, process names, hosts, file hashes, the webshell marker) are one list,
`internal/ioc/indicators.txt`, built into hs; box-local additions go in `/etc/hs/indicators.local`.
The firewall block list reads the same hosts. A path that a Debian package owns is not reported.

## The controls

### 1. Web terminal off, and checked to stay off — built

Check `hestia.web-terminal`: Fail when `WEB_TERMINAL='true'`, the service is active or anything
listens on :8085; the fix `web-terminal-off` runs `v-delete-sys-web-terminal` and makes sure the unit
is down. hs's earlier fix that repaired and restarted a failing web-terminal unit is gone — a failed
unit now offers turning it off. Hestia's switch leaves the package installed; a build older than
1.0.3 left behind (alavus had 1.0.2) is a Warn with the fix `web-terminal-purge`. If a browser
terminal is ever needed: enable, use, disable in the same session.

**Found 2026-09-25:** hzdemolink still has `WEB_TERMINAL='true'` with package 1.0.3 installed. The
service is not running only because 1.0.3 shipped without `node-pty`; the pending upgrade to 1.0.5
would bring it back to life. Turn it off there (`hs fix web-terminal-off`).

### 2. Panel and SSH only from the admin network — built, needs a decision per box

Check `panel.exposure` warns while Hestia's rules open :8083 or :22 to `0.0.0.0/0`. Operation
`panel-restrict` rewrites those Hestia firewall rules to the listed networks (the first one in
place, one more rule per further network), refuses unless the current SSH session's address is
covered, and shows the break-glass path (the provider's web console) before asking. Tailscale is the
recommended source (100.64.0.0/10 once every admin machine is on it); fixed office or admin addresses
work the same way. EngineLink is unaffected (it uses the streamer, :8091, which has its own rule).

### 2b. IPv6 bypasses Hestia's firewall — built (found 2026-09-25)

Hestia's firewall is IPv4-only (its rules take an `IPV4_CIDR`) and leaves ip6tables at policy ACCEPT
with no rules. On a box with a global IPv6 address, everything listening on `[::]` answers the whole
IPv6 internet whatever the IPv4 rules say. Verified from outside on hzdemolink: the panel (302) and
the Netdata dashboard (200) were reachable over IPv6, although Netdata is closed in the IPv4 rules;
the streamer listened on all addresses too. The boxes are Hetzner machines with IPv6, so this is
likely fleet-wide.

Check `firewall.ipv6` (Fail, listing what is reachable). Operation `firewall-ipv6` makes IPv6 follow
Hestia's own rules: the ports Hestia opens to everyone stay open, everything it limits to addresses
is closed (plus optional admin IPv6 ranges); ICMPv6 and DHCPv6 stay allowed because IPv6 breaks
without them. Applied on hzdemolink: Netdata no longer answers over IPv6, SSH and sites unaffected.

How it is wired: `hs firewall apply` generates `/etc/hs/firewall.sh` (own chains, rebuilt on every
run, jumps kept at the top) from `/etc/hs/firewall.conf` and Hestia's `rules.conf`; one line in
Hestia's `data/firewall/custom.sh` runs it after every `v-update-firewall`, and `hs-firewall.service`
runs it at boot — Hestia restores only its saved IPv4 rules then, so without the unit IPv6 would be
open again after every reboot.

### 3. SSH: key-only, no personal keys on servers — built

- `ssh-keys-only` writes `/etc/ssh/sshd_config.d/00-hs-keys-only.conf` (read before any cloud-image
  drop-in): `PasswordAuthentication no`, `KbdInteractiveAuthentication no`, `PermitRootLogin
  prohibit-password`, `AllowAgentForwarding no`, `X11Forwarding no`. It refuses without an authorized
  key and a key login in the last 30 days, or when the current session came in with a password, and
  runs `sshd -t` before reloading. `MaxAuthTries` stays at sshd's default (6): an ssh agent offers each
  of its keys as one try, and 3 locks out a legitimate client carrying several keys (the operator's
  agent holds 19); key-only login and fail2ban already cover guessing. hzdemolink still accepted
  passwords on 2026-09-25.
- **Never copy a personal private key to a server.** For server-to-server copies, generate a throwaway
  key on the destination, add its public half to the source with `from="<dest-ip>",restrict`, remove
  both when done. Check `ssh.private-keys` fails on any private key under `/root/.ssh` or
  `/home/*/.ssh` that is not an allowed automation key (`/root/.ssh/storagebox`, plus
  `/etc/hs/ssh-private-keys.allowed`); fix `ssh-private-key-remove` shreds it (rotate it first — removal
  does not revoke it). **Found:** `id_valolink` is still on hzdemolink.
- Check `ssh.authorized-keys` asks for a review until the current keys are accepted
  (`ssh-keys-accept`, fingerprints in `/etc/hs/ssh-keys.accepted`); after that any key nobody accepted
  is a Fail. Hestia's file-manager keys (`filemanager.ssh.key`) are among them — accept them.

### 4. Outbound traffic: allow known ports, log the rest — built

Operation `egress`: known-bad hosts are always dropped (in and out); the filter allows DNS, NTP, HTTP,
HTTPS, mail submission 465/587, the Storage Box 23 and SSH 22 (git, migrations), plus extra ports per
box. Mode `log` logs everything else with the sending uid (prefix `hs-egress:`), mode `drop` rejects
it. Check `egress` summarises what the log caught in the last 7 days — what would break — so the
switch to drop is an informed one. Run log for a week, allow what is legitimate (a box delivering
mail itself needs 25), then drop. hzdemolink runs in log mode since 2026-09-25. The Realm agent can
also tunnel over DNS and ICMP; the port filter does not stop those, the detection has to.

### 5. Secrets never on a command line — built

- `setup-restic-backup.sh --password` takes no value; a word typed after it is refused without being
  echoed, with the instruction to change the password and clear the history line (`1564830`). The
  hs `restic-setup` form takes the password masked and passes it in `SB_PASS`. (The Storage Box
  password that reached hzdemolink's history this way was rotated on 2026-09-25.)
- Operation `shell-history`: `/etc/profile.d/hs-history.sh` — a command typed with a leading space is
  not saved, every entry gets a timestamp.
- `install-scripts.sh` prints the streamer token only to a terminal; under `v-hestiascripts-update`
  (systemd-run, stdout to the journal) it names the file instead.
- hs secret fields never reach argv, the action log or the plan (`HS_SECRET_*` environment).

### 6. Streamer: fail closed — built

- Both `main.go` (hestia-streamer) and `hs serve` refuse to start without `HESTIA_STREAMER_TOKEN`, and
  refuse every request if the token is somehow empty: a missing env file is an outage, not an open
  root API. The `streamer` check fails when no token is configured.
- Listen address from `HESTIA_STREAMER_ADDR` (e.g. the Tailscale IP:8091) instead of every interface;
  unset keeps :8091 on all addresses, with the firewall (IPv4 and now IPv6) as the gate.
- `v-hestiascripts-update` stays on the allowlist, because it now installs only signed code (§13).

### 7. Nothing downloadable that should not be — built

- The wp-secure snippet and both wp-rocket templates deny archives (`zip gz tgz tar bz2 xz 7z rar
  wpress dump`) at the web root and directly under `wp-content/`, backup-plugin folders whole
  (`ai1wm-backups`, `updraft`, `backups-dup-*`, `backup-db`, `wpvividbackups`, `backupwordpress*`,
  `uploads/backwpup*` — nginx serves them straight from disk, so the plugins' `.htaccess` never
  applies) and `.wpress` anywhere. `wp-content/uploads/` stays downloadable for shops that sell files.
  Verified on hzdemolink in an isolated nginx: all denied, an uploads zip still 200. Boxes pick it up
  with `web-templates` + `web-rebuild`.
- Fixes `dumps` / `debug-log` move files to the site's `private/hs-moved-<date>/` with a
  `MANIFEST.tsv` (original → new path). nginx may answer a moved URL from its open-file cache for up to
  a minute — recheck after that. `web.exposure` findings are part of the watch, so they alert.
- `WP_DEBUG_LOG` to a path under `private/` when debugging (fix `debug-keep`), never the default.
- **Staging sites are behind HTTP basic auth by default** (`v-wp-staging-create`, `--no-httpauth` to
  opt out; the hs staging form has the switch). Credentials in `/root/.hestia-staging-auth/<domain>`
  (root only, never printed), reused on every refresh so reviewers keep access; removed on teardown.
  Not yet run against a real staging build.

### 8. Detection — built

| Check | Looks for |
|---|---|
| `persist.iocs` | indicator paths (unless a package owns them), process names, connections to known-bad hosts |
| `persist.processes` | processes running from /tmp, /var/tmp, /dev/shm or memfd; deleted or unpackaged binaries (hash compared with the indicator list) |
| `persist.outbound` | established or pending outbound connections to ports outside the usual set |
| `persist.accounts` | uid-0 accounts besides root; sudoers drop-ins no package installed |
| `persist.cron` | cron entries (Hestia's own `v-*` jobs aside) nobody accepted (`cron-accept`) |
| `persist.unowned` | ELF executables and libraries no package owns (merged-/usr aware, diversions included), hashed; hourly |
| `persist.units` | systemd units no package installed (alias symlinks aside) |
| `web.webshell` | the webshell marker, and eval/assert on request data outside wp-admin/wp-includes; hourly |
| `web.root-owned` | root-owned PHP in sites — a whole root-owned site (a migration) is one finding with the permissions fix |

Known-good unpackaged files are built in (hs, the checkout the v-scripts link into, rclone, maldet,
Hestia's and cloud-init's sudoers files, Postfix's chroot copies); others go in
`/etc/hs/unowned.allowed` (operation `persist-allow`). On hzdemolink the only remaining findings are
two cron entries to review, `/usr/bin/restic` and the root-owned boostwith sites. The panel's
nginx.conf is known good (Hestia writes the box's DNS resolvers into it). `/usr/bin/restic` differs
from Debian's package on every box set up for restic: Hestia's installer and `v-add-backup-host-restic`
run `restic self-update`. On hzdemolink the binary was verified byte-identical to the official 0.19.1
release (2026-09-28); accept it per box with `persist-allow` after such a check.

Operation `maldet-signatures` adds the webshell marker to maldet's `custom.hex.dat` (maldet takes
custom signatures as MD5 or hex only). maldet scanned these boxes daily from June and found nothing.

### 9. Package integrity — built

Check `pkg.integrity`: `dpkg --verify` weekly; modified files from packages, configuration files
excluded, are a finding. It would not have caught this incident (the attackers added files), but it
is the check for the next rootkit that replaces one.

### 10. Logs long enough to answer questions — built

Operation `log-retention`: a journald drop-in (persistent, a size cap never below current use,
`MaxRetentionSec=1year`) and `rotate 26` (weekly) in the nginx, apache2 and hestia logrotate files —
edited in place, since a second logrotate file for the same logs is an error. Applied on hzdemolink.
Shipping `auth`, `sshd`, Hestia's auth log and the panel access log off the box (EngineLink or a
small log host) is still open: root on the box can delete local logs.

### 11. A small audit trail — built

Operation `audit`: auditd with watches on systemd unit directories, `/etc/ld.so.preload`, cron,
accounts and sudoers, and every `.ssh` directory, plus kernel module loads (`ausearch -k hs_units`
etc.; in a script add `--input /var/log/audit/audit.log`, as ausearch reads stdin without a
terminal). Programs started from /tmp are not in it: both rule forms for that (`-F dir=/tmp` on
execve, `-w /tmp -p x`) loaded but recorded nothing on Debian 12 in testing, while a plain execve rule
did — /tmp execution is covered by §14 and `persist.processes` instead. Applied on hzdemolink.

### 12. Tripwires — manual

A canary AWS key (canarytokens.org) in `/root/.aws/credentials` and a canary token in a `.env`: the
May 2026 tooling's first commands were `cat /root/.aws/credentials`, `env | grep KEY` and a disk-wide
grep for `AKIA`, `sk-ant-`, `sk-proj-`, `ghp_`. Needs an account at canarytokens.org; not automated.

### 13. Signed code on the path to root — built, needs signing set up

`v-hestiascripts-update` fetches, then verifies the new commit with `git verify-commit` against
`/etc/hs/allowed_signers` — a file on the box, outside the repository, so a push cannot change who is
trusted — **before** merging anything. No signers file, or no valid signature: nothing changes and
it says why. `--allow-unsigned` works only from a terminal on the box. Check `updates.signing` warns
until the signers file exists, and warns if the installed checkout's HEAD is not signed by a trusted
key. Until commits are signed, EngineLink's update button is refused.

Setting it up (dev machine): `git config --global gpg.format ssh`, `git config --global
user.signingkey ~/.ssh/<signing key>.pub`, `git config --global commit.gpgsign true`. On each box:
`install -d -m 700 /etc/hs && echo "reima.kokko@valolink.fi ssh-ed25519 AAAA…" > /etc/hs/allowed_signers`.
GitHub: 2FA, branch protection on `main` (no force-push), read-only deploy keys instead of personal
keys.

### 14. Smaller attack surface — built

- Operation `tmp-noexec`: /dev/shm remounted `noexec,nosuid,nodev` now, /tmp a RAM-backed tmpfs with
  the same options from the next boot, apt pointed at /var/tmp. Check `tmp.noexec`.
- Operation `remove-service`: Dovecot, ClamAV, SpamAssassin, vsftpd, and now ProFTPD (only when no
  domain has an FTP user) and BIND (only when the box hosts no DNS zone); `services.idle` reports the
  unused ones.
- Operation `pma-restrict`: phpMyAdmin (answering at `/phpmyadmin/` on every site domain) limited to
  admin networks in both its nginx and Apache includes, between `hs:pma` markers; config tests before
  each reload. Check `web.phpmyadmin`.
- Check `php.functions` / operation `php-functions`: exec, system, passthru, shell_exec, proc_open,
  popen and pcntl_exec disabled for every PHP-FPM version (Hestia's installer already does this for the
  versions it installs; the check keeps it so).
- Panel users: two-factor login for every account that logs in, login disabled for accounts nobody
  uses (`hestia.2fa`); the API off or limited (`hestia.api`).

### 15. Backups an attacker cannot delete — manual

Storage Box snapshots on every subaccount (Hetzner console; not visible from the box, so not
checked). Every snapshot of the three boxes since May contains the implants and webshells: a restore
needs the indicator list in hand (`hs check --section security` right after restoring).

## The three compromised boxes

hzdemolink, alavus and hzweb1 had four months of root access by someone else. The containment removed
everything known; nothing can prove there is nothing unknown. Rebuilding them from scratch is the
operator's decision; everything in this document applies to them the same way whether or not they
are rebuilt, and the watch is how an unknown leftover would show itself.

## Findings in this repo's own code

| Where | Finding | Status |
|---|---|---|
| `main.go` | token check skipped when the env var is unset | fails closed |
| `main.go` | `ListenAndServe(":8091")` on every interface, as root | `HESTIA_STREAMER_ADDR` |
| `main.go` | `v-hestiascripts-update` allowlisted → root via git | updates verified against box-local signers |
| `install-scripts.sh` | prints the token unconditionally | only to a terminal |
| `install-scripts.sh` | EngineLink IP hardcoded (`NUXT_IP`); the rule is only added when no :8091 rule exists, so an IP change leaves the old one | open |
| `setup-restic-backup.sh` | a password typed after `--password` ended up in shell history and was echoed | refused without echo (`1564830`) |
| `setup/security.sh` (run.sh) | edits `sshd_config` directly, which a `sshd_config.d/` drop-in silently overrides | superseded by `ssh-keys-only` |
| `templates/nginx/*` | archives not denied | denied |
| run.sh `setup/nginx-templates.sh` | Apache wp-secure written where php-fpm boxes never read it, HTTPS copy without rules | fixed in `hs templates install` |
| hs fix `web-terminal-update` | repaired and restarted the web terminal | replaced by `web-terminal-off` |

## Open questions for the operator

- Tailscale on every box (and on the EngineLink host), or address allowlists, for `panel-restrict` and
  `pma-restrict`?
- Which sites legitimately serve archives directly from the web root or `wp-content/` (outside
  uploads)? They need an exception to the archive rule.
- Where should off-box logs go: EngineLink, or a separate small log host?
- Signing key for §13: a dedicated SSH key for commit signing, and who else commits.
