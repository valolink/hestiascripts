# Security hardening for the Hestia fleet

Written 2026-09-25, after the May 2026 compromise of three boxes. This is a plan, not a record of
what is deployed: each item says why it matters (with the evidence from this incident), what to
change, and where in this repo it belongs. Incident details: `~/valolink/hzdemolink-incident-2026-09-25.md`.

## What happened, in one paragraph

Between 2026-05-20 and 05-26, ten IP addresses opened 62 unauthenticated root shells through the
Hestia **web terminal** (`hestia-web-terminal` 1.0.2, `/_shell/` on the panel port) on hzdemolink,
alavus and hzweb1. The 1.0.2 `server.js` read the PHP session file with a string split, so a session
that had only loaded `/login/` was accepted as root (upstream fixed it in hestiacp commit `854d71b3c`).
Automated tooling then grepped the disk for cloud and API keys, read `/root/.my.cnf`, installed a
**Realm C2 agent** (`ulibd`, callback `195.72.61.165:8000`), a second agent disguised as a systemd
helper, a C++ pivot/rootkit tool, an XMRig miner, and dropped a password-gated PHP webshell into every
web root. Nothing noticed for four months. On two boxes the web terminal was still installed and
running on 2026-09-25, and all three boxes held the operator's unencrypted personal SSH key, which also
opened root on most of the fleet and push access to this repository on GitHub.

## What would have changed the outcome

Ranked by how much each one would have limited this specific incident.

| Control | Effect on this incident |
|---|---|
| Panel (and its web terminal) reachable only from the admin network | The bug is unreachable; nothing else follows. |
| Web terminal off unless in use | Same, on every box, even with the panel public. |
| Outbound traffic limited to known ports | The C2 (:8000, :50051) and the mining pool (:8444) never connect. |
| No personal private keys on servers | The breach stays on three boxes instead of reaching the whole fleet and GitHub. |
| Persistence / unknown-binary checks in `hs check`, with alerts | Detection within a day instead of four months. |
| Logs kept 12 months, ideally off-box | The first shells (May) and every download could have been reconstructed. |
| No dumps or archives in web roots | Two customer databases and a site backup not downloadable by scanners. |

## Interim: watch the boxes until they are rebuilt

The rebuilds (see *Later*) wait until they can be tested properly, which may take a while. Until
then the boxes keep running, so something has to notice a return without anyone remembering to look.

### `v-server-ioc-watch.sh`

A read-only watch that runs from cron on **every box**, stays silent while nothing changes, and
alerts on anything new. It repeats, automatically, the checks that found this incident by hand on
2026-09-25. The `v-server-` prefix gets it symlinked by `install-scripts.sh` and allows it through
the streamer, so EngineLink can also call it. These checks become `hs check` items later (§8); the
script exists so the fleet is covered now.

**Contract**

- Read-only; every probe bounded with `timeout`; never kills, moves or deletes anything.
- State in `/var/lib/hestia-ioc-watch/` (root, 700): `baseline.txt` (accepted findings) and
  `last.txt`. A run compares its findings with the baseline and **reports only what is new**.
- `--baseline` records the current findings as accepted (run once after reviewing them).
  `--full` prints everything, including accepted findings. `--quiet` prints only new findings.
- Exit `0` nothing new · `1` new warning · `2` new critical. The final stdout line is one line of
  JSON (`{"new":[…],"critical":N,"warn":N,"checkedAt":"…"}`), the same contract as
  `v-server-health`, so EngineLink can parse the last `data:` frame.
- Cron: `/etc/cron.d/hestia-ioc-watch` (a drop-in we own, like `hestia-restic`), fast checks every
  15 minutes, the filesystem sweeps hourly (`--sweep`). `flock` against overlapping runs.
- Alerts: on exit ≥ 1 from cron, mail the new findings to **tuotanto@valolink.fi** (decided
  2026-09-25; the address is the default in `/etc/hestia-ioc-watch.conf`, overridable per box). Send
  through the box's configured SMTP relay, and treat a failed send as a finding in the next run's
  output, so a broken relay does not silence the watch. Re-alert on the same finding at most once a day.

**Checks — critical** (each one was true on at least one box this year)

| Check | How | Incident evidence |
|---|---|---|
| Web terminal back on | `WEB_TERMINAL='true'` in `hestia.conf`, or `hestia-web-terminal` active, or anything listening on :8085 | the entry point on all three boxes |
| C2 block missing | no `DROP` for `195.72.61.165` in `iptables -S OUTPUT`, on a box whose `custom.sh` carries it | the block is the only thing stopping a leftover agent |
| Connections to known-bad hosts | `ss -tnp` to `195.72.61.165`, `161.97.132.209`, `74.48.66.73` | implant SYN-SENT to :8000 on hzweb1 |
| Known implant files or names | paths `/usr/lib/systemd/systemd-ulibd`, `/usr/lib/systemd/systemd-{journal-upload,rfkill-watcher,resolved-helper}`, `/usr/lib/__root/`, `/usr/lib/__hesti/`, `/usr/{s,}bin/chrony2*`, `/var/tmp/system-id`, `/usr/lib/x86_64-linux-gnu/.nss_cache.init`; processes with those names | all of them, on 1–3 boxes |
| Known hashes (sha256) | see the list below the table | — |
| `/etc/ld.so.preload` exists | file present at all | rootkit preload, 2026-05-29 |
| Webshell in a web root | the marker string `bc764c3a318cf1fae267a7276e7c92ba219a651f337d39274ea6462f08d6a3f7` in any `.php` under `/home/*/web/*/public_html` and `/var/www` (hourly) | 49 copies incl. `/var/www/html` |
| New uid-0 account | any account other than root with uid 0 | — |

Known sha256 hashes (not secret; also in the quarantine manifests on each box):

```
24cddd5f50e695176762a30e0069709eda51dc1182c092f8683e4ac622850b95  ulibd (Realm imix agent)
407df667ea2df5e859b03cdda629016485775b1ac5bb22e851833daa2eb9a131  second agent (systemd-journal-upload / -rfkill-watcher / -resolved-helper)
663a8088faca8e126a11648f5671fcf4eaf96435e10daf63a1d1b17d8385154c  __root (pivot / rootkit tool)
82d072ab03d90c0131b06a576eadf2c986a51f0de986d50db4e49be01bf125cb  __hesti (same family, earlier build)
4baecd2b1ef9045b62bc212989e44ceabe5c7a88cde2baf8f53e385952ee0e19  chrony2 (XMRig), alavus/hzweb1/hzdemolink /usr/bin
b0e1ae6d73d656b203514f498b59cbcf29f067edf6fbd3803a3de7d21960848d  chrony2 (XMRig), hzdemolink /usr/sbin
2658e8fde41f119deaec1977c14343c2fc6472f6a8e6149f70f95c8f3fd3ffb4  PHP webshell (xcbin.php and its disguised copies)
```

**Checks — warning**

| Check | How | Why |
|---|---|---|
| Unpackaged binaries | ELF executables and `.so` files under `/usr /etc /opt /var /root /boot /srv /tmp /var/tmp /dev/shm` that no package owns — merged-/usr aware (try the `/lib`↔`/usr/lib`, `/bin`↔`/usr/bin`, `/sbin`↔`/usr/sbin` aliases before `dpkg -S` says "unowned") — hourly | found `__hesti` and the second miner after name-based checks missed them |
| Unpackaged systemd units | `.service`/`.timer` under `/etc/systemd/system` and `/usr/lib/systemd/system` that no package owns | the agents installed as fake systemd units |
| Processes from temp dirs | `/proc/*/exe` pointing into `/tmp`, `/var/tmp`, `/dev/shm`, or unowned; sustained CPU > 50 % from an unowned binary (sample twice, 10 s apart) | `/tmp/a.elf`; the miner |
| Unusual outbound ports | established or SYN-SENT connections to remote ports outside 22, 23, 25, 53, 80, 123, 443, 465, 587, 993, 995 (plus loopback) | C2 on :8000/:50051, pool on :8444 |
| New `authorized_keys` lines | fingerprints of every line in `/root/.ssh` and `/home/*/.ssh` `authorized_keys*`, compared with the baseline | persistence the attackers did not use this time, but the obvious next one |
| Private keys on the box | files under `/root/.ssh` and `/home/*/.ssh` that start with `-----BEGIN … PRIVATE KEY` or `openssh-key-v1`, other than allowlisted automation keys | `id_valolink` on all three compromised boxes |
| Root-owned PHP in web roots | `find /home/*/web/*/public_html -name '*.php' -user root` (hourly) | every webshell copy was root-owned |
| New cron entries | files in `/etc/cron.d` and lines in `/var/spool/cron/crontabs/*` not in the baseline, ignoring Hestia's own `v-*` commands | — |

**Known-good, never reported** (starting allowlist, extend per box): `/usr/bin/rclone`, the `hs`
binary and `/root/hs-v2-test/*`, `hestia-streamer`, `hestia-streamer.service`, the `hs` units and
`/etc/cron.d/hs-logcap`, `/etc/cron.d/hestia-restic`, Postfix's chroot copies under
`/var/spool/postfix/lib/`, `/root/.ssh/storagebox`, deleted-binary processes that are only stale
after an apt upgrade (`agetty`, `dhcpcd` on hzdocker).

**Not in the watch:** the disk-wide API-key grep (too slow for cron; run by hand when needed) and
WordPress integrity (`wp core verify-checksums` calls out to api.wordpress.org; belongs in `hs check`
per site, §8).

**Rollout:** build and test on hzdemolink (a clean run after `--baseline` must report nothing; place
a harmless file named like an IOC, e.g. `/usr/lib/__test/x`, and confirm a critical), then deploy with
`install-scripts.sh` and run `--full` + `--baseline` on each box after reviewing its findings.

## Now (this week)

### 1. Keep the web terminal off, and check it stays off

- Done 2026-09-25 on alavus and hzweb1 (`v-delete-sys-web-terminal`), hzdemolink earlier. The other
  boxes never had the package.
- **hs check `hestia.web-terminal`:** `Fail` when `WEB_TERMINAL='true'` or `hestia-web-terminal` is
  active; `Fail` (not Warn) when the installed package version is below 1.0.3. `Fix`:
  `v-delete-sys-web-terminal`. A Hestia upgrade must not be able to bring it back unnoticed.
- If a browser terminal is ever needed again: enable, use, disable in the same session.

### 2. Panel, SSH and streamer only from the admin network

The panel on :8083 is open to the internet on every box today, and so is SSH (brute-force noise in
every auth log). The next panel bug is the same story as the web terminal.

- **Recommended: Tailscale on every box** (the operator already uses it). Allow :8083, :22 and :8091
  only on `tailscale0` plus the EngineLink host, drop them on the public interface.
- **Fallback:** Hestia firewall rules allowing :8083/:22 only from the admin IP ranges.
- Keep a documented break-glass path (the provider's web console) in the same doc as the rule, since
  a Tailscale outage must not lock everyone out.
- **hs check `panel.exposure`:** warn when :8083 or :22 accept from `0.0.0.0/0` in `iptables -S`.
- Where: `hs op panel-restrict` (writes the Hestia firewall rules, shows the break-glass note first).

### 3. SSH: key-only, no personal keys on servers

- `PasswordAuthentication no` everywhere. hzdemolink still had `yes`; `hs op ssh-keys-only` already
  exists and refuses unless a key login is proven. Add `PermitRootLogin prohibit-password`,
  `KbdInteractiveAuthentication no`, `MaxAuthTries 3`, `AllowAgentForwarding no` to the same drop-in.
- **Never copy a personal private key to a server.** `id_valolink` was on hzdemolink (for an rsync),
  hzweb1 (a migration from web1) and twice on alavus. For server-to-server copies, generate a
  throwaway key on the *destination*, add its public half to the source's `authorized_keys` with
  `from="<dest-ip>",restrict`, and remove both when done. `v-wp-migrate-site.sh` and the migration
  docs should say so.
- **hs check `ssh.private-keys`:** list private key files under `/root/.ssh` and `/home/*/.ssh`;
  `Fail` for any key whose comment or fingerprint is not in an allowlist of automation keys
  (`/root/.ssh/storagebox`).
- **hs check `ssh.authorized-keys`:** fingerprints of every `authorized_keys` line, compared with the
  previous run; any new line is a finding with the file, fingerprint and comment. Keys are per-server
  now (`ssh-keyswap`, 2026-09-25), so the expected set per box is small and known.

### 4. Outbound traffic: allow known ports, log the rest

Every connection the attackers needed went to non-standard ports: C2 on :8000 and :50051, the mining
pool on :8444, tools from :18888. A port allowlist on `OUTPUT` would have cut all of it, without the
maintenance cost of a destination allowlist (WordPress plugins call too many APIs for that).

- Allow: loopback, established/related, DNS 53, NTP 123/udp, HTTP 80, HTTPS 443, SMTP submission
  587/465 (relay), Storage Box 23, SSH 22 (git and migrations), the MariaDB/Redis ports on
  localhost only. Log and drop everything else.
- Roll out in two steps: a week of `LOG` only (`hs check egress` reports what would have been
  dropped), then `DROP`.
- Where: the same `/usr/local/hestia/data/firewall/custom.sh` mechanism already carrying the C2 block
  (it survives `v-update-firewall` and reboots). Generate it from a template in `templates/firewall/`
  so every box gets the same rules; the C2 block becomes one line of it.
- Limitation: the Realm agent can also tunnel over DNS and ICMP. The port filter does not stop those;
  the detection in §8 has to.

### 5. Secrets never on a command line

`setup-restic-backup.sh --password <value>` put the `u626683-sub3` password into hzdemolink's
`/root/.bash_history`, which the attackers had root on.

- `setup-restic-backup.sh`: make `--password` take no value and read it with `read -rs` (or from
  `HS_SECRET_STORAGEBOX`), and refuse a value on argv with a message that says why.
- Every box: `HISTCONTROL=ignoreboth` in `/etc/profile.d/`, so a command typed with a leading space
  is not saved.
- `install-scripts.sh` prints the streamer token to stdout; when `v-hestiascripts-update` runs it
  under `systemd-run`, that output lands in the journal. Print only on a TTY.

### 6. Streamer: fail closed

- `main.go` skips the token check when `HESTIA_STREAMER_TOKEN` is unset. Refuse to start instead
  (log why), so a missing env file is an outage, not an open root API.
- Listen on a configured address (the Tailscale IP or the interface facing EngineLink) instead of
  `:8091` on every interface. The firewall rule is then the second layer, not the only one.
- `v-hestiascripts-update` is on the allowlist and runs `git pull` + `install-scripts.sh` as root:
  whoever can push to `valolink/hestiascripts` gets root on every box at the next update. Until
  commits are signed and verified (§13), remove it from the allowlist and update over SSH.
- Once `hs serve` replaces the streamer, carry these three over, not the old defaults.

### 7. Nothing downloadable that should not be

On 2026-09-25, 23 dumps/archives on hzweb1 and 15 on hzdemolink were moved out of `public_html`.
Scanners had already fetched `staging.valolink.fi/db.sql` (the valolink.fi database with admin
hashes) and several 5–54 MB `debug.log` files.

- `templates/nginx/wp-secure-snippet.conf` denies `.sql` and `.log` but not archives. Extend the
  extension rule with `gz|tgz|tar|zip|7z|rar|wpress|sql\.gz|dump`, scoped so plugin downloads under
  `wp-content/uploads` still work where a site sells files (decide per site; default deny at the
  web root and directly under `wp-content/`).
- The rules only exist in domains on a hardened template. `debug.log` was served on domains that were
  not. The existing `web.exposure` finding needs an **alert**, not just a line in `hs check`.
- `hs fix web.exposure`: move each file to `<domain>/private/moved-from-public-<date>/` with a
  `MANIFEST.tsv`, as done by hand today. Remember nginx's `open_file_cache_valid 60s`: recheck the URL
  after a minute.
- `WP_DEBUG_LOG` → a path under `private/` when debugging, never the default `wp-content/debug.log`.
- Staging and demo domains: HTTP basic auth by default (`v-wp-staging-create.sh`), so scanners get a
  401 instead of the site.

## Next (this month)

### 8. Detection: `hs check` looks for what we found by hand

Everything below is read-only and bounded, so it fits the existing check model, and each one has a
real positive from this incident. They only help if a new `Fail` reaches a person: EngineLink should
notify on any `security` finding that was not there on the previous run.

| Check ID | Looks for | Would have caught |
|---|---|---|
| `persist.unowned-binaries` | ELF executables and `.so` files under `/usr`, `/etc`, `/opt`, `/var`, `/root`, `/tmp`, `/dev/shm` that no package owns (merged-/usr aware), minus an allowlist (`rclone`, `hs`, `hestia-streamer`) | `ulibd`, the second agent, `__root`, `__hesti`, `chrony2` |
| `persist.units` | unit files and timers under `/etc/systemd/system` and `/usr/lib/systemd/system` not owned by a package, minus our own (`hestia-streamer`, `hs`) | `ulibd.service`, `systemd-journal-upload.service` and its renamed copies |
| `persist.preload` | `/etc/ld.so.preload` exists at all | the rootkit preload (2026-05-29) |
| `persist.processes` | processes whose binary is deleted, lives in `/tmp`, `/var/tmp` or `/dev/shm`, or is unowned; sustained CPU > 50% from an unknown binary | `/tmp/a.elf`, the miner |
| `persist.outbound` | established or pending connections to ports outside the §4 allowlist | the implant's SYNs to :8000 |
| `persist.accounts` | uid-0 accounts other than root, new login-shell users, sudoers drop-ins not from packages or Hestia | — (clean here, cheap to keep clean) |
| `web.root-owned` | files owned by root inside any `public_html` | every webshell copy was root-owned |
| `web.webshell` | known incident hashes and markers, plus PHP with `eval`/`assert` on request data outside `wp-includes`/`wp-admin` | the `_sauth` webshell (maldet found nothing) |

Also feed the incident hashes into maldet's custom signatures
(`/usr/local/maldetect/sigs/custom.md5.dat`): maldet scanned these boxes daily from 06-10 and found
nothing, because its signatures did not know this shell.

### 9. Package integrity

`dpkg --verify` (or `debsums -s`) weekly, as `hs check pkg.integrity`: any modified file from a
package, outside a dpkg run, is a finding. It would not have caught this incident (the attackers added
files rather than modifying packaged ones), but it is the check for the next rootkit that does.

### 10. Keep logs long enough to answer questions

Most boxes kept 2–6 weeks of journal and `auth.log`; the web logs rotate after five weeks. That is why
the May shells could only be reconstructed on the two boxes whose journal happened to reach back, and
why nobody can say who opened phpMyAdmin on hzdemolink on 06-07.

- journald: `Storage=persistent`, `SystemMaxUse=1G`, `MaxRetentionSec=1year` (drop-in in
  `templates/journald/`).
- logrotate: `/var/log/apache2/domains/*.log` and `/usr/local/hestia/log/*` to 26 weeks, compressed.
- Better: ship `auth`, `sshd`, `server.js`, Hestia `auth.log` and the panel access log off the box
  (to EngineLink or a small Loki), because root on the box can delete local logs. The attackers did
  not bother this time.

### 11. A small audit trail

`auditd` with a minimal rule set (not a full-disk watch): writes under `/etc/systemd/system`,
`/usr/lib/systemd/system`, `/etc/ld.so.preload`, `/etc/cron*`, `/var/spool/cron`, every
`authorized_keys`, `/etc/passwd` and `/etc/shadow`; `execve` from `/tmp`, `/var/tmp` and `/dev/shm`;
kernel module loads. That gives the "who ran what, when" that bash history only half-answered here.

### 12. Tripwires the attackers would have hit

Their first commands were `cat /root/.aws/credentials`, `env | grep KEY`, and a disk-wide grep for
`AKIA`, `sk-ant-`, `sk-proj-`, `ghp_`. A **canary** AWS key (canarytokens.org, free) in
`/root/.aws/credentials`, and a canary token in a `.env`, alert the moment someone uses what they
found. Cheap, no false positives, and it would have fired on 2026-05-20.

### 13. Signed code on the path to root

hestiascripts and valolink-plugin both deploy with root or site-level privileges, and no commit is
signed (146 + 44 since May, all unsigned, all by the operator — nothing foreign was found).

- Sign commits (SSH signing with a dedicated key, which fits the new per-purpose keys).
- `v-hestiascripts-update`: `git verify-commit HEAD` after the pull, against an allowed-signers file
  shipped in the repo; refuse to install on failure.
- GitHub: 2FA on the account, branch protection on `main` (no force-push), and read-only deploy keys
  on the boxes instead of a personal key.

### 14. Smaller attack surface on every box

- `/tmp` and `/dev/shm` as `tmpfs` with `noexec,nosuid,nodev`. The attackers ran `/tmp/a.elf` and
  `/tmp/rg`; automated tooling fails on `noexec`. Test first: apt and a few installers want to
  execute from `/tmp` (`APT::ExtractTemplates::TempDir` fixes apt).
- Turn off what a box does not serve: exim/dovecot/roundcube without mail domains, proftpd without
  FTP users, named when DNS is elsewhere, clamav when nothing uses it. `v-server-audit` already
  reports "resident services with nothing to serve"; give it an `hs fix`.
- phpMyAdmin answers at `/phpmyadmin/` on every site domain (checked 2026-09-25 on valolink.fi,
  rainset.fi, reservationat8ight.fi, alavusikkunat.fi — all 200), and its login is a database
  password prompt anyone can brute-force. Restrict the alias to the admin network (§2) or disable it.
  Roundcube lives on `webmail.<domain>` for mail domains only; restrict it the same way where used.
- PHP-FPM pools: `disable_functions = exec,passthru,shell_exec,system,proc_open,popen,pcntl_exec`
  where the site does not need them (a webshell loses most of its use), and confirm `open_basedir`
  is set per pool. Needs a per-site opt-out for the few plugins that shell out.
- Hestia panel users: 2FA for `admin`/`valolink`, remove unused panel users, per-user login IP
  lists where Hestia supports them. The API is already effectively off (`API_ALLOWED_IP=''` denies
  every address); keep it that way.

### 15. Backups an attacker cannot delete

With root on a box, the attackers also held its Storage Box credentials. Per-box subaccounts limit
the blast radius, but they can still delete that box's repository.

- Enable Storage Box snapshots on every subaccount (already a follow-up in `setup-restic-backup.sh`;
  make `hs check backups.snapshots` report it).
- Remember that every snapshot of the three boxes since May contains the implants and webshells. A
  restore needs the incident IOC list in hand, or it restores the backdoor with the site.

## Later (with the rebuilds)

- **Rebuild** hzdemolink, alavus and hzweb1 from scratch. The containment removed everything we
  know of; four months of root means we cannot prove there is nothing we do not know of.
- **One baseline, applied at install:** an `hs install` profile that applies §1–§7 and §10–§14 on a
  fresh Hestia box, so a new server starts hardened instead of being hardened later (or not).
- **External view:** a weekly port scan of every box from the EngineLink host, compared with the
  expected set (80, 443, and nothing else on the public interface once §2 is done).

## Findings in this repo's own code

| Where | Finding | Change |
|---|---|---|
| `main.go` | token check skipped when the env var is unset | refuse to start without a token (§6) |
| `main.go` | `ListenAndServe(":8091")` on every interface, as root | configurable listen address (§6) |
| `main.go` | `v-hestiascripts-update` allowlisted → root via git | remove until commits are verified (§6, §13) |
| `install-scripts.sh` | prints the token unconditionally | print only on a TTY (§5) |
| `install-scripts.sh` | EngineLink IP hardcoded (`NUXT_IP`); rule only added when no :8091 rule exists, so an IP change leaves the old one | read the IP from config; replace the rule when it differs |
| `setup-restic-backup.sh` | `--password <value>` ends up in shell history | prompt or env var only (§5) |
| `setup/security.sh` (run.sh) | edits `sshd_config` directly, which a `sshd_config.d/` drop-in silently overrides | superseded by `hs op ssh-keys-only`; retire the run.sh path |
| `templates/nginx/wp-secure-snippet.conf` | archives not denied | extend the extension rule (§7) |

## Open questions for the operator

- Tailscale on every box (and on the EngineLink host), or IP allowlists? Tailscale is the stronger
  and simpler option if the break-glass path is acceptable.
- Which sites legitimately serve archives from `wp-content/uploads` (downloadable products)? They need
  an exception to the archive deny rule.
- Where should off-box logs go: EngineLink, or a separate small log host?
