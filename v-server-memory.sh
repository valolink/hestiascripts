#!/bin/bash
# info: read-only memory and cache audit, in plain language
# options: none
#
# example: v-server-memory
#
# Explains where the box's RAM actually goes and whether any of it is a problem.
# Every section states a number and then what the number means — a reading of
# "4.9 GB used" is not actionable on its own, and neither is a buffer-pool hit
# rate without the threshold it should be compared against.
#
# Read-only: no config is touched, no service restarted. Takes ~5 seconds
# (a short vmstat sample is the only deliberate wait).

if [ -t 1 ]; then
  BOLD='\033[1m'; DIM='\033[2m'
  RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'; CYAN='\033[0;36m'; NC='\033[0m'
else
  BOLD=''; DIM=''; RED=''; YELLOW=''; GREEN=''; CYAN=''; NC=''
fi

TODO=()
note()  { echo -e "     ${DIM}$*${NC}"; }
ok()    { echo -e "  ${GREEN}✅${NC} $*"; }
warn()  { echo -e "  ${YELLOW}⚠️${NC}  $*"; }
bad()   { echo -e "  ${RED}❌${NC} $*"; }
todo()  { TODO+=("$1"); }

head2() {
  echo ""
  echo -e "  ${BOLD}$1${NC}"
  echo -e "  ${DIM}$(printf '─%.0s' $(seq 1 ${#1}))${NC}"
}

mariadb_q() { timeout 10 mariadb -N -B -e "$1" 2>/dev/null; }
have() { command -v "$1" >/dev/null 2>&1; }

echo ""
echo -e "  ${BOLD}Memory audit — $(hostname -f)${NC}"
echo -e "  ${DIM}$(date '+%Y-%m-%d %H:%M %Z') · read-only · $(uptime -p)${NC}"

# ================================================================== overall ==
head2 "THE HEADLINE"

read -r MEM_TOTAL MEM_AVAIL < <(awk '/MemTotal/{t=$2} /MemAvailable/{a=$2} END{print int(t/1024), int(a/1024)}' /proc/meminfo)
MEM_USED=$((MEM_TOTAL - MEM_AVAIL))
PCT=$((MEM_USED * 100 / MEM_TOTAL))

printf '  %s GB total · %s GB in use · %s GB available (%s%%)\n' \
  "$(awk -v m=$MEM_TOTAL 'BEGIN{printf "%.1f", m/1024}')" \
  "$(awk -v m=$MEM_USED  'BEGIN{printf "%.1f", m/1024}')" \
  "$(awk -v m=$MEM_AVAIL 'BEGIN{printf "%.1f", m/1024}')" "$PCT"

if [ "$PCT" -ge 90 ]; then
  bad "Very little headroom. The next traffic spike has nowhere to go."
  todo "Free memory now — see the ceiling section below for the usual cause."
elif [ "$PCT" -ge 75 ]; then
  warn "Getting tight, but not yet dangerous."
else
  ok "Comfortable."
fi
note "'Available' is the honest figure — it counts reclaimable cache, which 'free' does not."

# ===================================================================== swap ==
head2 "SWAP"

read -r SWAP_TOTAL SWAP_FREE < <(awk '/SwapTotal/{t=$2} /SwapFree/{f=$2} END{print int(t/1024), int(f/1024)}' /proc/meminfo)
SWAPPINESS=$(cat /proc/sys/vm/swappiness 2>/dev/null)

if [ "${SWAP_TOTAL:-0}" -eq 0 ]; then
  warn "No swap configured."
  note "Without swap the kernel OOM-kills a process outright instead of degrading first."
  todo "Add swap: run.sh → 7 (Security)."
else
  SWAP_USED=$((SWAP_TOTAL - SWAP_FREE))
  printf '  %s MB of %s MB used · vm.swappiness = %s\n' "$SWAP_USED" "$SWAP_TOTAL" "${SWAPPINESS:-?}"

  # si/so are the number that matters: pages *moving*, not pages parked.
  if have vmstat; then
    read -r SI SO < <(vmstat 1 3 2>/dev/null | tail -1 | awk '{print $7, $8}')
    if [ "${SI:-0}" -gt 0 ] || [ "${SO:-0}" -gt 0 ]; then
      bad "Actively swapping (in ${SI}/s, out ${SO}/s) — this is real pressure and it is slow."
      todo "The box is swapping under load; reduce the PHP-FPM ceiling or add RAM."
    elif [ "$SWAP_USED" -gt 0 ]; then
      ok "Swap holds ${SWAP_USED} MB but nothing is moving — merely parked, not thrashing."
      note "Pages put there during an earlier peak stay until something needs them. Harmless."
    else
      ok "Unused. No memory pressure has occurred since boot."
    fi
  fi
  [ "${SWAPPINESS:-60}" -gt 30 ] &&
    note "swappiness ${SWAPPINESS} is the Debian default; 10 suits a database box better."
fi

# ======================================================== where it all goes ==
head2 "WHERE THE MEMORY GOES"
note "PSS, not RSS — shared pages (opcache, copy-on-write forks) are split between"
note "the processes sharing them, so these numbers sum to something believable."

for p in /proc/[0-9]*; do
  [ -r "$p/smaps_rollup" ] || continue
  pss=$(awk '/^Pss:/ {s+=$2} END {print s+0}' "$p/smaps_rollup" 2>/dev/null) || continue
  comm=$(cat "$p/comm" 2>/dev/null) || continue
  [ -n "$comm" ] && echo "$pss $comm"
done | awk -v total="$MEM_TOTAL" '
  {a[$2]+=$1; n[$2]++}
  END {for (k in a) printf "%.0f\t%s\t%d\t%.0f\n", a[k]/1024, k, n[k], (a[k]/1024)*100/total}
' | sort -rn | head -10 | while IFS=$'\t' read -r mb name procs pct; do
  printf '  %6s MB  %-22s %s\n' "$mb" "$name" "$(
    [ "$procs" -gt 1 ] && printf '%s procs, ' "$procs"; printf '%s%% of RAM' "$pct")"
done

# --- anything big that nothing on this box consumes ---
MTA=0
dpkg -l exim4 2>/dev/null | grep -q "^ii" && MTA=1
dpkg -l dovecot-core 2>/dev/null | grep -q "^ii" && MTA=1
if [ "$MTA" -eq 0 ] && systemctl is-active clamav-daemon &>/dev/null; then
  CLAM_MB=$(ps -eo rss,comm --no-headers | awk '$2=="clamd" {print int($1/1024)}' | head -1)
  echo ""
  warn "clamd is holding ${CLAM_MB:-?} MB and has nothing to scan."
  note "Under Hestia, ClamAV only ever scans inbound mail. exim4 and dovecot are both gone."
  todo "Remove ClamAV: run.sh → 13 → 6 → 2. Frees roughly ${CLAM_MB:-1300} MB."
fi

# ================================================================== ceiling ==
head2 "THE CEILING — what PHP-FPM is allowed to ask for"

CHILDREN=$(grep -rhE '^\s*pm.max_children' /etc/php/*/fpm/pool.d/ 2>/dev/null |
  grep -oE '[0-9]+$' | awk '{s+=$1} END {print s+0}')
POOLS=$(ls /etc/php/*/fpm/pool.d/*.conf 2>/dev/null | wc -l)
AVG_KB=$(ps -eo rss,comm --no-headers 2>/dev/null |
  awk '$2 ~ /^php/ {s+=$1; n++} END {if (n) print int(s/n); else print 0}')
WORKERS_NOW=$(pgrep -c '^php-fpm' 2>/dev/null)

if [ "${CHILDREN:-0}" -eq 0 ] || [ "${AVG_KB:-0}" -eq 0 ]; then
  note "No PHP-FPM pools running — nothing to size."
else
  AVG_MB=$((AVG_KB / 1024))
  WORST_MB=$((CHILDREN * AVG_KB / 1024))
  printf '  %s workers allowed across %s pools × ~%s MB each = %s GB worst case\n' \
    "$CHILDREN" "$POOLS" "$AVG_MB" "$(awk -v m=$WORST_MB 'BEGIN{printf "%.1f", m/1024}')"
  printf '  Currently running: %s workers. Box has %s GB.\n' \
    "${WORKERS_NOW:-0}" "$(awk -v m=$MEM_TOTAL 'BEGIN{printf "%.1f", m/1024}')"

  RATIO=$(awk -v w=$WORST_MB -v t=$MEM_TOTAL 'BEGIN{printf "%.1f", w/t}')
  if [ "$WORST_MB" -gt "$MEM_TOTAL" ]; then
    bad "Overcommitted ${RATIO}×."
    note "Under a traffic spike PHP-FPM will keep forking until the kernel OOM-killer steps in,"
    note "and it picks the largest process — usually MariaDB. The whole box goes down, not just PHP."
    todo "Lower the PHP-FPM ceiling: run.sh → 9, pick a profile sized for ${MEM_TOTAL} MB."
  else
    ok "Fits in RAM even at full stretch."
  fi
fi

# ================================================================= database ==
head2 "DATABASE"

if ! have mariadb || ! mariadb_q "SELECT 1" >/dev/null; then
  note "MariaDB not reachable from this shell — skipping."
else
  POOL_MB=$(mariadb_q "SELECT ROUND(@@innodb_buffer_pool_size/1024/1024)")
  DATA_MB=$(mariadb_q "SELECT ROUND(SUM(data_length+index_length)/1024/1024) FROM information_schema.tables WHERE engine='InnoDB'")
  printf '  InnoDB dataset: %s MB · buffer pool: %s MB\n' "${DATA_MB:-0}" "${POOL_MB:-0}"

  # Deliberately not a verdict: a dataset bigger than the pool is only a problem
  # if the *working set* does not fit, and the hit rate below is what says so.
  if [ "${DATA_MB:-0}" -le "${POOL_MB:-0}" ]; then
    note "The whole dataset fits in the pool, so warm reads never touch disk."
  else
    note "Dataset exceeds the pool. That is fine as long as the working set fits —"
    note "the hit rate below is the number that decides, not this comparison."
  fi

  read -r UP_H HITPCT FREE_P < <(mariadb_q "
    SELECT ROUND(v.up/3600,1), ROUND(100*(1 - v.rd/NULLIF(v.rr,0)),3), v.pfree FROM (SELECT
      (SELECT VARIABLE_VALUE FROM information_schema.GLOBAL_STATUS WHERE VARIABLE_NAME='UPTIME') AS up,
      (SELECT VARIABLE_VALUE FROM information_schema.GLOBAL_STATUS WHERE VARIABLE_NAME='INNODB_BUFFER_POOL_READS') AS rd,
      (SELECT VARIABLE_VALUE FROM information_schema.GLOBAL_STATUS WHERE VARIABLE_NAME='INNODB_BUFFER_POOL_READ_REQUESTS') AS rr,
      (SELECT VARIABLE_VALUE FROM information_schema.GLOBAL_STATUS WHERE VARIABLE_NAME='INNODB_BUFFER_POOL_PAGES_FREE') AS pfree) v")

  if [ -n "$HITPCT" ] && [ "$HITPCT" != "NULL" ]; then
    printf '  Buffer pool hit rate: %s%% over %s h of uptime · %s pages still free\n' "$HITPCT" "${UP_H:-?}" "${FREE_P:-?}"
    if awk -v h="$HITPCT" 'BEGIN{exit !(h >= 99)}'; then
      ok "Comfortable. Anything above 99% means the pool is doing its job."
    else
      warn "Below 99% — the pool is too small for the working set."
      todo "Raise innodb_buffer_pool_size: run.sh → 11 (MariaDB)."
    fi
    awk -v u="${UP_H:-0}" 'BEGIN{exit !(u < 24)}' &&
      note "Uptime is under a day, so treat the hit rate as provisional."
  fi

  MYISAM=$(mariadb_q "SELECT COUNT(*) FROM information_schema.tables WHERE engine='MyISAM' AND table_schema NOT IN ('mysql','information_schema','performance_schema')")
  KEYBUF=$(mariadb_q "SELECT ROUND(@@key_buffer_size/1024/1024)")
  if [ "${MYISAM:-0}" -eq 0 ] && [ "${KEYBUF:-0}" -gt 32 ]; then
    warn "key_buffer_size is ${KEYBUF} MB but there are no MyISAM tables — that RAM is doing nothing."
    todo "Drop key_buffer_size to 16M — it only serves MyISAM, and you have none."
  fi

  echo ""
  echo -e "  ${DIM}Largest databases:${NC}"
  mariadb_q "SELECT table_schema, ROUND(SUM(data_length+index_length)/1024/1024)
             FROM information_schema.tables WHERE engine='InnoDB'
             GROUP BY table_schema ORDER BY 2 DESC LIMIT 5" |
    while IFS=$'\t' read -r db mb; do printf '    %6s MB  %s\n' "$mb" "$db"; done
fi

# ==================================================================== redis ==
head2 "REDIS"

if ! have redis-cli || ! timeout 5 redis-cli ping >/dev/null 2>&1; then
  note "Not installed or not responding — skipping."
else
  R_USED=$(timeout 5 redis-cli info memory 2>/dev/null | grep -oP 'used_memory_human:\K.*' | tr -d '\r')
  R_MAX=$(timeout 5 redis-cli info memory 2>/dev/null | grep -oP 'maxmemory_human:\K.*' | tr -d '\r')
  R_POL=$(timeout 5 redis-cli info memory 2>/dev/null | grep -oP 'maxmemory_policy:\K.*' | tr -d '\r')
  R_KEYS=$(timeout 5 redis-cli info keyspace 2>/dev/null | grep -c '^db')
  read -r R_HIT R_MISS R_EVICT < <(timeout 5 redis-cli info stats 2>/dev/null |
    awk -F: '/keyspace_hits/{h=$2} /keyspace_misses/{m=$2} /evicted_keys/{e=$2} END{print h+0, m+0, e+0}')

  printf '  Using %s of %s cap · policy %s\n' "${R_USED:-?}" "${R_MAX:-unlimited}" "${R_POL:-?}"

  if [ "${R_MAX:-0B}" = "0B" ]; then
    warn "No maxmemory cap — Redis will grow until the box runs out."
    todo "Cap Redis: run.sh → 3 (Redis)."
  fi

  if [ "${R_KEYS:-0}" -eq 0 ]; then
    warn "Capped and running, but the keyspace is empty — nothing is using it."
    note "Usually means the object-cache drop-in is missing on the sites."
    todo "Check object cache per site: v-wp-info --user <u> --domain <d>."
  else
    TOTAL_REQ=$((R_HIT + R_MISS))
    if [ "$TOTAL_REQ" -gt 0 ]; then
      HITRATE=$(awk -v h=$R_HIT -v t=$TOTAL_REQ 'BEGIN{printf "%.1f", 100*h/t}')
      printf '  Hit rate %s%% (%s hits / %s misses) · %s evictions\n' "$HITRATE" "$R_HIT" "$R_MISS" "$R_EVICT"

      if awk -v h="$HITRATE" 'BEGIN{exit !(h >= 80)}'; then
        ok "In use and effective."
      elif awk -v h="$HITRATE" 'BEGIN{exit !(h >= 50)}'; then
        warn "Hit rate is mediocre — more than a third of lookups miss."
        note "Usually short TTLs, or a cache flushed more often than it is filled."
      else
        warn "Hit rate is poor — most lookups miss, so Redis is adding a round-trip and saving little."
        note "Common causes: a WP_REDIS_PREFIX that changes between requests, a plugin"
        note "flushing the cache on every write, or cache keys that are never re-read."
        todo "Investigate the low Redis hit rate (${HITRATE}%) — it is currently near-useless work."
      fi
      [ "${R_EVICT:-0}" -gt 0 ] &&
        note "Evictions mean the cap is being hit; that is working as designed, not an error."
    else
      ok "In use."
    fi
  fi
fi

# ============================================================ wp autoloaded ==
head2 "WORDPRESS AUTOLOADED OPTIONS"
note "Every one of these bytes is read from the database on every single request."

if have mariadb && mariadb_q "SELECT 1" >/dev/null; then
  FOUND=0
  for t in $(mariadb_q "SELECT CONCAT('\`',table_schema,'\`.\`',table_name,'\`')
                        FROM information_schema.tables WHERE table_name LIKE '%options'
                          AND table_schema NOT IN ('mysql','information_schema','performance_schema','sys')"); do
    # WP 6.6 replaced autoload yes/no with on/off/auto-on/auto-off — exclude the
    # negatives rather than trying to match every positive.
    sz=$(mariadb_q "SELECT ROUND(SUM(LENGTH(option_value))/1024) FROM $t WHERE autoload NOT IN ('no','off','auto-off')")
    [ -z "$sz" ] || [ "$sz" = "NULL" ] && continue
    FOUND=1
    if [ "$sz" -gt 1024 ]; then
      printf '    %6s KB  %-38s %s\n' "$sz" "$(echo "$t" | tr -d '`')" "$(echo -e "${YELLOW}over 1 MB${NC}")"
      todo "Trim autoloaded options on $(echo "$t" | tr -d '`' | cut -d. -f1) (${sz} KB on every request)."
    else
      printf '    %6s KB  %s\n' "$sz" "$(echo "$t" | tr -d '`')"
    fi
  done
  [ "$FOUND" -eq 0 ] && note "No WordPress options tables found."
  echo ""
  note "Under ~800 KB is healthy. Over 1 MB usually means a plugin is caching into"
  note "the options table — often one that has since been deactivated but not cleaned up."
fi

# ================================================================== summary ==
echo ""
echo "  $(printf '─%.0s' {1..64})"
if [ "${#TODO[@]}" -eq 0 ]; then
  echo -e "  ${GREEN}Nothing to change.${NC} Memory is sized sensibly for this box."
else
  echo -e "  ${BOLD}Worth doing, in order:${NC}"
  i=1
  for t in "${TODO[@]}"; do
    echo -e "  ${CYAN}${i}.${NC} $t"
    i=$((i + 1))
  done
fi
echo ""

exit 0
