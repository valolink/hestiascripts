#!/bin/bash
# info: read-only server audit — reports what still needs doing
# options: [--brief]
#
# example: v-server-audit
#          v-server-audit --brief
#
# Prints findings only. A check that passes prints nothing: the point of this
# script is the to-do list, not reassurance. run.sh's dashboard answers "is it
# installed" — this answers "does it actually work", which is a different
# question and the one that has bitten us.
#
# Every check is bounded with `timeout` so a wedged daemon cannot hang the run,
# and nothing is ever written. Exit code: 2 = critical findings, 1 = warnings
# only, 0 = clean. That makes a fleet sweep scriptable:
#
#   for h in box1 box2 box3; do ssh $h v-server-audit --brief; done

BRIEF=0
[ "$1" = "--brief" ] && BRIEF=1

if [ -t 1 ] && [ "$BRIEF" -eq 0 ]; then
  BOLD='\033[1m'; DIM='\033[2m'
  RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; CYAN='\033[0;36m'; NC='\033[0m'
else
  BOLD=''; DIM=''; RED=''; YELLOW=''; GREEN=''; CYAN=''; NC=''
fi

US=$'\x1f'
FINDINGS=()

# finding LEVEL "what is wrong" "why it matters" "how to fix"
finding() { FINDINGS+=("$1${US}$2${US}$3${US}$4"); }

count_level() {
  local lvl="$1" n=0 f
  for f in "${FINDINGS[@]}"; do [ "${f%%$US*}" = "$lvl" ] && n=$((n + 1)); done
  echo "$n"
}

have() { command -v "$1" >/dev/null 2>&1; }

# ---------------------------------------------------------------- firewall ---
# The check that started this script. On 2026-08-28 kuumalahde had fail2ban
# reporting 7 healthy jails while every single ban failed: the iptables binary
# was gone, so the firewall held zero rules and nothing said so anywhere.
audit_firewall() {
  if ! have iptables; then
    finding CRITICAL "iptables is not installed" \
      "Hestia's firewall cannot load any rules and fail2ban cannot ban anything — the box is open on every port." \
      "apt install -y iptables && systemctl start hestia-iptables && systemctl restart fail2ban"
    return
  fi

  local rules
  rules=$(timeout 10 iptables -S 2>/dev/null | wc -l)
  if [ "${rules:-0}" -lt 5 ]; then
    finding CRITICAL "Firewall is loaded but empty ($rules rules)" \
      "Hestia's rules.conf is not applied, so every port is reachable regardless of what the panel shows." \
      "systemctl start hestia-iptables   # or: v-update-firewall"
  fi

  if systemctl list-unit-files hestia-iptables.service &>/dev/null; then
    if ! timeout 5 systemctl is-active hestia-iptables &>/dev/null; then
      local why
      why=$(timeout 5 systemctl show hestia-iptables -p Result --value 2>/dev/null)
      finding CRITICAL "hestia-iptables.service is not active (result: ${why:-unknown})" \
        "Firewall rules are not reapplied at boot, so a reboot silently drops the firewall." \
        "systemctl status hestia-iptables"
    fi
  fi
}

# ---------------------------------------------------------------- fail2ban ---
# "running" is not the same as "able to ban" — check the action, not the daemon.
audit_fail2ban() {
  have fail2ban-client || return 0
  if ! timeout 5 systemctl is-active fail2ban &>/dev/null; then
    finding CRITICAL "fail2ban is not running" \
      "Brute-force attempts against SSH, WordPress and FTP go unthrottled." \
      "systemctl start fail2ban"
    return
  fi

  # Only failures since the daemon last started count. The log keeps the whole
  # history, so counting all of it would keep reporting a fault for weeks after
  # it was fixed — the same false signal this script exists to remove.
  local started errs
  started=$(timeout 5 systemctl show fail2ban -p ActiveEnterTimestamp --value 2>/dev/null)
  started=$(date -d "$started" '+%Y-%m-%d %H:%M:%S' 2>/dev/null)
  [ -n "$started" ] || started="0000-00-00 00:00:00"

  errs=$(awk -v since="$started" '
    /Failed to execute ban/ { ts = $1 " " substr($2, 1, 8); if (ts >= since) n++ }
    END { print n + 0 }' /var/log/fail2ban.log 2>/dev/null)

  if [ "${errs:-0}" -gt 0 ]; then
    finding CRITICAL "fail2ban is running but $errs ban action(s) failed since it started" \
      "The jails detect attacks and then silently fail to block them — the daemon still reports itself healthy." \
      "grep 'Failed to execute ban' /var/log/fail2ban.log | tail -3"
  fi
}

# ------------------------------------------------------------ systemd units ---
audit_units() {
  local failed
  failed=$(timeout 10 systemctl --failed --no-legend --plain 2>/dev/null | awk '{print $1}' | paste -sd' ')
  [ -n "$failed" ] && finding CRITICAL "Failed systemd units: $failed" \
    "Something the box is configured to run is not running, and nothing surfaces that outside this list." \
    "systemctl --failed  →  systemctl status <unit>"
}

# ----------------------------------------------------------- core services ---
audit_services() {
  local svc web php
  web=$(timeout 5 systemctl is-active nginx 2>/dev/null)
  [ "$web" != "active" ] && [ "$(timeout 5 systemctl is-active apache2 2>/dev/null)" != "active" ] &&
    finding CRITICAL "No web server is running" "Every hosted site is down." "systemctl status nginx apache2"

  for svc in hestia mariadb; do
    if systemctl list-unit-files "${svc}.service" &>/dev/null; then
      timeout 5 systemctl is-active "$svc" &>/dev/null ||
        finding CRITICAL "$svc is not running" "Core Hestia/database service is down." "systemctl status $svc"
    fi
  done

  php=$(find /etc/php -maxdepth 1 -mindepth 1 -type d -printf '%f\n' 2>/dev/null | sort -V | tail -1)
  if [ -n "$php" ] && systemctl list-unit-files "php${php}-fpm.service" &>/dev/null; then
    timeout 5 systemctl is-active "php${php}-fpm" &>/dev/null ||
      finding CRITICAL "php${php}-fpm is not running" "PHP sites return 502." "systemctl status php${php}-fpm"
  fi
}

# ----------------------------------------------------------------- backups ---
audit_backups() {
  local u home newest age stale=""
  [ -d /usr/local/hestia/data/users ] || return 0
  for u in $(ls /usr/local/hestia/data/users 2>/dev/null); do
    newest=$(find /backup -maxdepth 1 -name "${u}.*.tar" -printf '%T@\n' 2>/dev/null | sort -rn | head -1)
    if [ -z "$newest" ]; then
      home=$(find "/home/$u/backup" -maxdepth 1 -type f -printf '%T@\n' 2>/dev/null | sort -rn | head -1)
      newest="$home"
    fi
    if [ -z "$newest" ]; then
      stale="$stale $u(none)"
    else
      age=$(( ( $(date +%s) - ${newest%.*} ) / 3600 ))
      [ "$age" -gt 48 ] && stale="$stale $u(${age}h)"
    fi
  done
  [ -n "$stale" ] && finding CRITICAL "Backups stale or missing:$stale" \
    "A restore request would have nothing recent to restore from." \
    "v-backup-user <user>   # then check the schedule in /etc/cron.d/hestia"
}

# -------------------------------------------------------------------- disk ---
audit_disk() {
  local pct inode
  pct=$(df --output=pcent / 2>/dev/null | tail -1 | tr -dc '0-9')
  if [ "${pct:-0}" -ge 90 ]; then
    finding CRITICAL "Root filesystem is ${pct}% full" \
      "At 100% MariaDB stops writing and sites start erroring." "run.sh → 14 (Disk)"
  elif [ "${pct:-0}" -ge 80 ]; then
    finding WARNING "Root filesystem is ${pct}% full" "Headroom is getting thin." "run.sh → 14 (Disk)"
  fi

  # Inode exhaustion looks exactly like a full disk but df -h shows space free.
  inode=$(df --output=ipcent / 2>/dev/null | tail -1 | tr -dc '0-9')
  [ "${inode:-0}" -ge 85 ] && finding WARNING "Root filesystem is ${inode}% of inodes" \
    "Writes fail with 'No space left on device' while df -h still shows free space." \
    "df -i /  →  hunt small-file directories (sessions, cache)"
}

# ------------------------------------------------------------ memory / OOM ---
# The check that would have predicted the web1 OOM: PHP-FPM's configured ceiling
# is what the box tries to allocate under load, not what it uses at rest.
audit_memory() {
  local ram_mb children avg_kb worst_mb
  ram_mb=$(awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo)
  children=$(grep -rhE '^\s*pm.max_children' /etc/php/*/fpm/pool.d/ 2>/dev/null |
    grep -oE '[0-9]+$' | awk '{s+=$1} END {print s+0}')
  avg_kb=$(ps -eo rss,comm --no-headers 2>/dev/null | awk '$2 ~ /^php/ {s+=$1; n++} END {if (n) print int(s/n); else print 0}')

  [ "${children:-0}" -gt 0 ] && [ "${avg_kb:-0}" -gt 0 ] || return 0

  # Not children × RSS: every worker maps the same opcache SHM, so that counts
  # one shared block once per worker and can overstate the ceiling several-fold.
  # sum(PSS) = shared + N×private and RSS ≈ shared + private solves for both.
  local n pss private_kb shared_kb
  read -r n pss < <(
    for p in /proc/[0-9]*; do
      [ -r "$p/smaps_rollup" ] || continue
      case "$(cat "$p/comm" 2>/dev/null)" in php-fpm*|php*) ;; *) continue ;; esac
      awk '/^Pss:/ {s+=$2} END {print s+0}' "$p/smaps_rollup" 2>/dev/null
    done | awk '{c++; s+=$1} END {print c+0, s+0}')

  if [ "${n:-0}" -gt 1 ] && [ "${pss:-0}" -gt "$avg_kb" ]; then
    private_kb=$(( (pss - avg_kb) / (n - 1) ))
    shared_kb=$(( avg_kb - private_kb ))
    [ "$shared_kb" -lt 0 ] && shared_kb=0
  else
    private_kb=$avg_kb
    shared_kb=0
  fi
  [ "$private_kb" -lt 1 ] && private_kb=$avg_kb

  worst_mb=$(( (shared_kb + children * private_kb) / 1024 ))

  if [ "$worst_mb" -gt "$ram_mb" ]; then
    finding WARNING "PHP-FPM can request more RAM than the box has (${worst_mb}MB vs ${ram_mb}MB)" \
      "${children} max_children x $((private_kb / 1024))MB private each, plus $((shared_kb / 1024))MB shared opcache. A traffic spike OOM-kills MariaDB before PHP notices." \
      "run.sh → 9 (PHP-FPM) and pick a profile that fits, or lower pm.max_children"
  fi

  local swap_total swap_used
  swap_total=$(awk '/SwapTotal/ {print int($2/1024)}' /proc/meminfo)
  swap_used=$(awk '/SwapTotal/ {t=$2} /SwapFree/ {f=$2} END {print int((t-f)/1024)}' /proc/meminfo)
  [ "${swap_total:-0}" -eq 0 ] && finding WARNING "No swap configured" \
    "Without swap the kernel OOM-kills immediately instead of degrading." "run.sh → 7 (Security) → swap"
  [ "${swap_total:-0}" -gt 0 ] && [ "${swap_used:-0}" -gt $((swap_total * 50 / 100)) ] &&
    finding WARNING "Swap is ${swap_used}MB of ${swap_total}MB used" \
      "The box is under real memory pressure, not merely parked." "v-server-memory"
}

# --------------------------------------------------------- updates / reboot ---
audit_updates() {
  [ -f /var/run/reboot-required ] && finding WARNING "Reboot required" \
    "A kernel or core library was updated; the running system is still on the old one." \
    "safe-reboot   # staged, guarded reboot (SSH only, not a v-script)"

  local sec
  sec=$(timeout 20 apt-get -s upgrade 2>/dev/null | grep -c '^Inst.*security')
  [ "${sec:-0}" -gt 0 ] && finding WARNING "$sec pending security update(s)" \
    "unattended-upgrades is security-only, so anything left here needs a hand." \
    "apt-get upgrade   (review first: apt-get -s upgrade | grep ^Inst)"

  local cur latest
  cur=$(grep -oP "(?<=VERSION=')[^']+" /usr/local/hestia/conf/hestia.conf 2>/dev/null | head -1)
  latest=$(curl -sf --max-time 4 "https://api.github.com/repos/hestiacp/hestiacp/releases/latest" 2>/dev/null |
    grep '"tag_name"' | cut -d'"' -f4 | tr -d 'v')
  [ -n "$cur" ] && [ -n "$latest" ] && [ "$cur" != "$latest" ] &&
    finding WARNING "HestiaCP $cur is behind $latest" "Panel security fixes ship in point releases." \
      "v-update-sys-hestia-all"
}

# ------------------------------------------------------- hestia.conf drift ---
# A *_SYSTEM key naming a package that is gone makes Hestia email root a
# "<svc> restart failed" report on every mail-domain change and scheduled restart.
audit_drift() {
  local key values pkg cur v pkgs found
  while read -r key values pkgs; do
    cur=$(grep -m1 "^$key=" /usr/local/hestia/conf/hestia.conf 2>/dev/null | cut -d "'" -f2)
    [ -n "$cur" ] || continue
    for v in ${values//|/ }; do
      [ "$cur" = "$v" ] || continue
      found=0
      for pkg in $pkgs; do dpkg -l "$pkg" 2>/dev/null | grep -q "^ii" && found=1; done
      [ "$found" -eq 1 ] || finding WARNING "hestia.conf $key='$cur' but the package is gone" \
        "Hestia keeps trying to restart a service that no longer exists and emails root each time." \
        "v-change-sys-config-value $key \"\"   # or run.sh → 13 → 6"
      break
    done
  done <<'EOF'
IMAP_SYSTEM dovecot dovecot-core
ANTIVIRUS_SYSTEM clamav-daemon|clamav|clamd clamav-daemon clamav clamav-base
ANTISPAM_SYSTEM spamassassin|spamd spamassassin
FTP_SYSTEM vsftpd vsftpd
FTP_SYSTEM proftpd proftpd
MAIL_SYSTEM exim4 exim4
EOF
}

# ------------------------------------------------------------------ maldet ---
audit_maldet() {
  if [ ! -f /usr/local/maldetect/maldet ]; then
    finding WARNING "Maldet is not installed" \
      "Nothing scans customer sites for injected shells." "run.sh → 5 (Maldet) → 1"
    return
  fi
  timeout 5 systemctl is-active maldet &>/dev/null ||
    finding WARNING "maldet.service is not running" \
      "Monitor mode is off, so new files are not scanned as they land." \
      "systemctl status maldet   # missing deps are usually 'ed' or inotify-tools"

  local last last_ts age
  last=$(grep "scan completed" /usr/local/maldetect/logs/event_log 2>/dev/null | tail -1 | awk '{print $1, $2}')
  if [ -z "$last" ]; then
    finding WARNING "Maldet has never completed a scan" \
      "Installed but unproven — you have no baseline and no idea of its false-positive rate here." \
      "run.sh → 5 (Maldet) → 3"
  else
    last_ts=$(date -d "$last" +%s 2>/dev/null)
    if [ -n "$last_ts" ]; then
      age=$(( ( $(date +%s) - last_ts ) / 86400 ))
      [ "$age" -gt 7 ] && finding WARNING "Last maldet scan was ${age} days ago" \
        "The daily cron is not completing." "cat /etc/cron.daily/maldet ; maldet -a /home/*/web/*/public_html/"
    fi
  fi
}

# ---------------------------------------------------------------- security ---
audit_security() {
  local pw
  pw=$(sshd -T 2>/dev/null | awk '/^passwordauthentication/ {print $2}')
  [ "$pw" = "yes" ] && finding WARNING "SSH accepts password authentication" \
    "Every exposed box gets continuous SSH brute-force; keys remove the attack entirely." \
    "run.sh → 7 (Security)   # confirm your key works BEFORE disabling passwords"

  # Matches v-server-setup-status: security-only is the intended state, so it is
  # not a finding. Only "not installed" and "no config file" are.
  if [ "$(dpkg -l unattended-upgrades 2>/dev/null | grep -c '^ii')" -eq 0 ]; then
    finding WARNING "unattended-upgrades is not installed" \
      "Security patches wait for someone to remember." "run.sh → 7 (Security)"
  elif [ ! -f /etc/apt/apt.conf.d/50unattended-upgrades ]; then
    finding WARNING "unattended-upgrades has no config file" \
      "The package is installed but applies nothing." "run.sh → 7 (Security)"
  fi
}

# --------------------------------------------------------------------- ssl ---
audit_ssl() {
  local crt end end_ts days dom expiring=""
  for crt in /home/*/conf/web/*/ssl/*.crt; do
    [ -f "$crt" ] || continue
    end=$(timeout 3 openssl x509 -enddate -noout -in "$crt" 2>/dev/null | cut -d= -f2)
    [ -n "$end" ] || continue
    end_ts=$(date -d "$end" +%s 2>/dev/null) || continue
    days=$(( (end_ts - $(date +%s)) / 86400 ))
    dom=$(basename "$crt" .crt)
    [ "$days" -lt 14 ] && expiring="$expiring $dom(${days}d)"
  done
  [ -n "$expiring" ] && finding WARNING "SSL certificates expiring soon:$expiring" \
    "Let's Encrypt renewal is failing for these; browsers will hard-fail the site." \
    "v-add-letsencrypt-domain <user> <domain>"
}

# -------------------------------------------------------------------- mail ---
audit_mail() {
  local q
  if have mailq; then
    q=$(timeout 5 mailq 2>/dev/null | grep -cE '^[A-F0-9]{10,}')
  fi
  [ "${q:-0}" -gt 50 ] && finding WARNING "$q messages stuck in the mail queue" \
    "Alerts from cron, maldet and fail2ban are not reaching anyone." "mailq | tail ; run.sh → 8 (SMTP)"

  have postconf || return 0
  local relay
  relay=$(timeout 5 postconf -h relayhost 2>/dev/null)
  [ -z "$relay" ] && finding ADVISORY "No SMTP relay configured" \
    "Mail sent straight from the box lands in spam or is dropped." "run.sh → 8 (SMTP)"
}

# --------------------------------------------------------- wasted services ---
# Something big and resident that nothing on this box consumes.
audit_waste() {
  local mta=0
  dpkg -l exim4 2>/dev/null | grep -q "^ii" && mta=1
  dpkg -l dovecot-core 2>/dev/null | grep -q "^ii" && mta=1

  if [ "$mta" -eq 0 ] && timeout 5 systemctl is-active clamav-daemon &>/dev/null; then
    local mb
    mb=$(ps -eo rss,comm --no-headers 2>/dev/null | awk '$2=="clamd" {print int($1/1024)}' | head -1)
    finding WARNING "clamd is running (${mb:-?}MB) with no mail stack to serve" \
      "ClamAV under Hestia only scans inbound mail. With exim4 and dovecot gone it scans nothing." \
      "run.sh → 13 → 6 → 2 (Remove ClamAV)"
  fi

  local php v eol=""
  for v in $(find /etc/php -maxdepth 1 -mindepth 1 -type d -printf '%f\n' 2>/dev/null); do
    case "$v" in
      5.*|7.*|8.0|8.1) systemctl is-active "php${v}-fpm" &>/dev/null && eol="$eol $v" ;;
    esac
  done
  [ -n "$eol" ] && finding WARNING "End-of-life PHP still running:$eol" \
    "No security patches upstream; sites on it are the box's soft spot." \
    "Migrate the domains, then: run.sh → 9 (PHP-FPM)"
}

# ----------------------------------------------------------------- backups2 ---
audit_restic() {
  [ -f /usr/local/hestia/conf/restic.conf ] || {
    finding ADVISORY "restic incremental backups are not set up" \
      "Tarball backups are full copies — slower, larger, and kept on the same provider." \
      "setup-restic-backup.sh --host <storagebox> --user <u> --password   # SSH only"
    return
  }
  local u total keyed=0
  for u in $(ls /usr/local/hestia/data/users 2>/dev/null); do
    [ -f "/usr/local/hestia/data/users/$u/restic.conf" ] && keyed=$((keyed + 1))
  done
  total=$(ls /usr/local/hestia/data/users 2>/dev/null | wc -l)
  [ "$keyed" -lt "$total" ] && finding ADVISORY "restic: only $keyed of $total users have a per-user key" \
    "Users without a key silently fall back to tarballs." "setup-restic-backup.sh"
}

# ------------------------------------------------------ redis object cache ---
# kuumalahde 2026-08-30: nightly 504s for two days. The redis-cache plugin's
# flush_group() runs a Lua SCAN across the WHOLE keyspace on every call, gated
# only by WP_REDIS_DISABLE_GROUP_FLUSH — not by the prefix, the database, or
# WP_REDIS_SELECTIVE_FLUSH. With 406k keys and no TTL that was 173ms a call,
# 2.4 calls/sec, 41% of a core. Redis is single-threaded, so each scan blocked
# every other cache read until PHP timed out.
#
# Measure the symptom directly — what share of Redis's uptime went into EVAL —
# rather than guessing from key counts alone.
audit_redis_cache() {
  have redis-cli || return 0
  timeout 5 redis-cli ping >/dev/null 2>&1 || return 0

  local keys expires uptime eval_usec pct
  keys=$(timeout 5 redis-cli dbsize 2>/dev/null | tr -dc '0-9')
  expires=$(timeout 5 redis-cli info keyspace 2>/dev/null | grep -oE 'expires=[0-9]+' | head -1 | tr -dc '0-9')
  uptime=$(timeout 5 redis-cli info server 2>/dev/null | grep -oE 'uptime_in_seconds:[0-9]+' | tr -dc '0-9')
  eval_usec=$(timeout 5 redis-cli info commandstats 2>/dev/null |
    grep -oE '^cmdstat_eval:calls=[0-9]+,usec=[0-9]+' | grep -oE 'usec=[0-9]+' | tr -dc '0-9')

  if [ -n "$eval_usec" ] && [ "${uptime:-0}" -gt 3600 ]; then
    pct=$(awk -v u="$eval_usec" -v s="$uptime" 'BEGIN{printf "%.0f", (u/1000000)*100/s}')
    if [ "${pct:-0}" -ge 10 ]; then
      finding WARNING "Redis spends ${pct}% of its uptime running object-cache flush scans" \
        "flush_group() SCANs the entire keyspace on every call and Redis is single-threaded, so each scan stalls every other cache read — which surfaces as 504s under load." \
        "Add to wp-config.php: define('WP_REDIS_DISABLE_GROUP_FLUSH', true); and define('WP_REDIS_MAXTTL', 86400);"
    fi
  fi

  # Unbounded keyspace is what makes each scan expensive in the first place.
  if [ "${keys:-0}" -gt 50000 ]; then
    local ttl_pct
    ttl_pct=$(awk -v e="${expires:-0}" -v k="$keys" 'BEGIN{printf "%.0f", e*100/k}')
    [ "${ttl_pct:-100}" -lt 25 ] && finding WARNING "Redis holds ${keys} keys and only ${ttl_pct}% have a TTL" \
      "Nothing bounds the keyspace, so it grows until every flush scan is slow." \
      "define('WP_REDIS_MAXTTL', 86400); in wp-config.php, then: redis-cli flushdb"
  fi

  # Sites with Redis enabled but missing the guard constant.
  local f dom missing=""
  for f in /home/*/web/*/public_html/wp-config.php; do
    [ -f "$f" ] || continue
    [ -f "$(dirname "$f")/wp-content/object-cache.php" ] || continue
    grep -q "WP_REDIS_DISABLE_GROUP_FLUSH" "$f" 2>/dev/null && continue
    dom=$(echo "$f" | awk -F/ '{print $5}')
    missing="$missing $dom"
  done
  [ -n "$missing" ] && finding ADVISORY "Redis enabled without WP_REDIS_DISABLE_GROUP_FLUSH:$missing" \
    "Harmless until the keyspace grows — then every group flush scans all of it. v-wp-redis-install sets this on new installs." \
    "wp config set WP_REDIS_DISABLE_GROUP_FLUSH true --raw --type=constant --path=<docroot>"
}

# ------------------------------------------------------- exposed WP files ---
# Found on kuumalahde 2026-08-28: a 52MB wp-content/debug.log serving over HTTPS
# with a 200. The wp-secure snippet already denies *.log — it just had never
# actually been injected into the template, so nothing enforced it. Check for the
# file itself rather than trusting a rule to exist.
audit_wp_exposure() {
  local f size dom logs="" dumps=""

  for f in /home/*/web/*/public_html/wp-content/debug.log; do
    [ -f "$f" ] || continue
    size=$(du -h "$f" 2>/dev/null | cut -f1)
    dom=$(echo "$f" | awk -F/ '{print $5}')
    logs="$logs ${dom}(${size})"
  done
  [ -n "$logs" ] && finding WARNING "WordPress debug.log inside the web root:$logs" \
    "Debug logs carry filesystem paths, plugin internals and often tokens in stack traces, and are served unless a deny rule is actually deployed." \
    "Point WP_DEBUG_LOG at <domain>/private/wp-debug.log (inside open_basedir, outside the web root), or set WP_DEBUG false"

  # Credential-bearing leftovers. maxdepth 2 keeps this cheap and avoids the
  # plugin/theme zips that legitimately live deeper in wp-content.
  for f in $(timeout 20 find /home/*/web/*/public_html -maxdepth 2 \
      \( -name 'wp-config*.bak*' -o -name 'wp-config*.save' -o -name 'wp-config*.old' \
         -o -name '*.sql' -o -name '*.sql.gz' \) 2>/dev/null | head -20); do
    dom=$(echo "$f" | awk -F/ '{print $5}')
    dumps="$dumps ${dom}:$(basename "$f")"
  done
  [ -n "$dumps" ] && finding CRITICAL "Database dump or wp-config backup in the web root:$dumps" \
    "These hand over database credentials or the entire dataset to anyone who guesses the filename, and scanners guess constantly." \
    "Move them outside public_html (or delete them) — check before deleting"
}

# ---------------------------------------------------------- web templates ---
# A template can exist and contain nothing. The injector matched four spaces
# against a tab-indented Hestia template, so `cp` succeeded, no rules landed,
# and every file-exists check called the box hardened.
audit_templates() {
  local dir="/usr/local/hestia/data/templates/web/nginx"
  [ -f "$dir/wp-secure.tpl" ] || return 0
  grep -q "Valolink security rules" "$dir/wp-secure.tpl" 2>/dev/null && return 0
  finding WARNING "nginx wp-secure.tpl contains no security rules" \
    "It is a plain copy of default.tpl, so every domain using it is unhardened while reporting as protected." \
    "run.sh → 12 (Web Templates) → 1, then re-check the status line"
}

# ------------------------------------------------- is the template in USE? ---
# The layer nothing caught for months. wp-secure.tpl can exist AND contain the
# rules AND still protect nothing, because a template only applies to domains
# assigned to it. setup-status reporting wpSecure:true only ever meant "a file
# with that name exists" — on kuumalahde all three domains sat on PROXY='default'
# the whole time.
audit_template_usage() {
  local dir="/usr/local/hestia/data/templates/web/nginx"
  [ -f "$dir/wp-secure.tpl" ] || return 0
  # The "exists but empty" case belongs to audit_templates; don't report twice.
  grep -q "Valolink security rules" "$dir/wp-secure.tpl" 2>/dev/null || return 0

  local f line dom proxy total=0 covered=0 uncovered=""
  for f in /usr/local/hestia/data/users/*/web.conf; do
    [ -f "$f" ] || continue
    while IFS= read -r line; do
      dom=$(echo "$line" | grep -oE "DOMAIN='[^']*'" | cut -d"'" -f2)
      [ -n "$dom" ] || continue
      proxy=$(echo "$line" | grep -oE "PROXY='[^']*'" | cut -d"'" -f2)
      total=$((total + 1))
      if [ "$proxy" = "wp-secure" ]; then
        covered=$((covered + 1))
      else
        uncovered="$uncovered $dom"
      fi
    done < "$f"
  done

  [ "$total" -gt 0 ] || return 0

  if [ "$covered" -eq 0 ]; then
    finding WARNING "wp-secure template is built but no domain uses it" \
      "The rules exist in the file and apply to nothing — every domain is still on its default proxy template." \
      "v-change-web-domain-proxy-tpl <user> <domain> wp-secure   # test one small site first"
  elif [ -n "$uncovered" ]; then
    finding ADVISORY "Domains not on the wp-secure proxy template:$uncovered" \
      "They get no hardening rules. Some of this may be deliberate — wp-rocket sites and the panel domain are reasonable exceptions." \
      "v-change-web-domain-proxy-tpl <user> <domain> wp-secure"
  fi
}

# ------------------------------------------------------------------ netdata ---
audit_netdata() {
  timeout 5 systemctl is-active netdata &>/dev/null || return 0
  grep -q "hestiascripts-tuned" /etc/netdata/netdata.conf 2>/dev/null ||
    finding WARNING "Netdata is running on the stock profile" \
      "Stock Netdata's memory footprint was the trigger for the web1 OOM on 2026-07-06." \
      "run.sh → 6 (Netdata)   # applies the low-footprint profile"
}

# =============================================================== run + print ==
audit_firewall
audit_fail2ban
audit_units
audit_services
audit_backups
audit_disk
audit_memory
audit_updates
audit_drift
audit_maldet
audit_security
audit_ssl
audit_mail
audit_waste
audit_restic
audit_wp_exposure
audit_redis_cache
audit_templates
audit_template_usage
audit_netdata

CRIT_N=$(count_level CRITICAL)
WARN_N=$(count_level WARNING)
ADV_N=$(count_level ADVISORY)

if [ "$BRIEF" -eq 1 ]; then
  printf '%-24s %s critical, %s warning, %s advisory\n' "$(hostname -s)" "$CRIT_N" "$WARN_N" "$ADV_N"
  for f in "${FINDINGS[@]}"; do
    [ "${f%%$US*}" = "CRITICAL" ] || continue
    rest="${f#*$US}"
    printf '  ! %s\n' "${rest%%$US*}"
  done
else
  echo ""
  echo -e "  ${BOLD}Server audit — $(hostname -f)${NC}"
  echo -e "  ${DIM}$(date '+%Y-%m-%d %H:%M %Z') · read-only, nothing was changed${NC}"
  echo "  $(printf '─%.0s' {1..64})"

  print_group() {
    local want="$1" colour="$2" heading="$3" f rest what why fix shown=0
    for f in "${FINDINGS[@]}"; do
      [ "${f%%$US*}" = "$want" ] || continue
      if [ "$shown" -eq 0 ]; then
        echo ""
        echo -e "  ${colour}${BOLD}${heading}${NC}"
        shown=1
      fi
      rest="${f#*$US}"; what="${rest%%$US*}"
      rest="${rest#*$US}"; why="${rest%%$US*}"
      fix="${rest#*$US}"
      echo ""
      echo -e "  ${colour}●${NC} ${BOLD}${what}${NC}"
      echo -e "    ${DIM}${why}${NC}"
      echo -e "    ${CYAN}→${NC} ${fix}"
    done
  }

  print_group CRITICAL "$RED"    "FIX NOW"
  print_group WARNING  "$YELLOW" "SHOULD FIX"
  print_group ADVISORY "$DIM"    "WORTH CONSIDERING"

  echo ""
  echo "  $(printf '─%.0s' {1..64})"
  if [ "$((CRIT_N + WARN_N + ADV_N))" -eq 0 ]; then
    echo -e "  ${GREEN}Nothing outstanding.${NC} Every check this script knows about passed."
    echo -e "  ${DIM}That is not the same as 'the box is fine' — see CLAUDE.md for what is not covered.${NC}"
  else
    echo -e "  ${RED}${CRIT_N} to fix now${NC} · ${YELLOW}${WARN_N} should fix${NC} · ${DIM}${ADV_N} worth considering${NC}"
  fi
  echo ""
fi

[ "$CRIT_N" -gt 0 ] && exit 2
[ "$WARN_N" -gt 0 ] && exit 1
exit 0
