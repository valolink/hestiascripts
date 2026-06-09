#!/bin/bash
# SMTP relay via Resend

_SASL_PASSWD="/etc/postfix/sasl_passwd"
_GENERIC_MAP="/etc/postfix/generic"
_POSTFIX_CF="/etc/postfix/main.cf"

menu_smtp() {
  while true; do
    clear
    echo ""
    echo -e "  ${BOLD}SMTP Relay — Resend${NC}"
    echo "$DIV"

    # Dependencies
    _smtp_dep_line "postfix"
    _smtp_dep_line "libsasl2-modules"
    _smtp_dep_line "mailutils"

    # Relay config
    echo ""
    if [ -f "$_POSTFIX_CF" ]; then
      local relay
      relay=$(postconf -h relayhost 2>/dev/null)
      if echo "$relay" | grep -q "smtp.resend.com"; then
        status_line "SMTP relay" OK "smtp.resend.com:587"
        if postconf -h smtp_generic_maps 2>/dev/null | grep -q "generic"; then
          local rewrite_to
          rewrite_to=$(grep -v '^#' "$_GENERIC_MAP" 2>/dev/null | awk 'NR==1{print $2}')
          sub_line "  Sender rewrite" "${rewrite_to:-(configured)}"
        else
          sub_line "  Sender rewrite" "not configured"
        fi
      elif [ -n "$relay" ] && [ "$relay" != "[]" ] && [ "$relay" != "" ]; then
        status_line "SMTP relay" WARN "relayhost = $relay  (not Resend)"
      else
        status_line "SMTP relay" ERR "no relay configured"
      fi
    else
      status_line "SMTP relay" ERR "Postfix not configured  (run option 1)"
    fi

    # Notification recipients summary
    echo ""
    _smtp_recipient_summary

    echo ""
    echo "  1) Install / fix dependencies"
    echo "  2) Configure Resend relay"
    echo "  3) Configure notification recipients"
    echo "  4) Send test email"
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice

    case "$choice" in
      1) _smtp_install_deps ;;
      2) _smtp_configure ;;
      3) _smtp_recipients ;;
      4) _smtp_test ;;
      0) return ;;
    esac
  done
}

_smtp_dep_line() {
  local pkg="$1"
  if ! dpkg -l "$pkg" 2>/dev/null | grep -q "^ii"; then
    status_line "$pkg" ERR "not installed"
  elif [ "$pkg" = "postfix" ] && [ ! -f "$_POSTFIX_CF" ]; then
    status_line "$pkg" WARN "installed but not configured"
  else
    status_line "$pkg" OK "installed"
  fi
}

_smtp_recipient_summary() {
  local root_addr maldet_addr f2b_addr uu_addr hestia_addr

  root_addr=$(grep "^root:" /etc/aliases 2>/dev/null | awk '{$1=""; print $0}' | xargs)
  [ -f /usr/local/maldetect/conf.maldet ] && \
    maldet_addr=$(grep "^email_addr=" /usr/local/maldetect/conf.maldet | cut -d'"' -f2)
  [ -f /etc/fail2ban/jail.local ] && \
    f2b_addr=$(grep "^destemail" /etc/fail2ban/jail.local | awk '{print $3}')
  [ -f /etc/apt/apt.conf.d/50unattended-upgrades ] && \
    uu_addr=$(grep -oP '(?<=^Unattended-Upgrade::Mail ")[^"]+' \
      /etc/apt/apt.conf.d/50unattended-upgrades 2>/dev/null | head -1)
  [ -f /usr/local/hestia/data/users/admin/user.conf ] && \
    hestia_addr=$(grep "^CONTACT=" /usr/local/hestia/data/users/admin/user.conf \
      | cut -d"'" -f2)

  sub_line "  System/cron (root alias)" "${root_addr:-(not set)}"
  [ -f /usr/local/maldetect/conf.maldet ]              && sub_line "  Maldet"              "${maldet_addr:-(not set)}"
  [ -f /etc/fail2ban/jail.local ]                      && sub_line "  Fail2ban"            "${f2b_addr:-(not set)}"
  [ -f /etc/apt/apt.conf.d/50unattended-upgrades ]     && sub_line "  Unattended upgrades" "${uu_addr:-(not set)}"
  [ -f /usr/local/hestia/data/users/admin/user.conf ]  && sub_line "  HestiaCP admin"      "${hestia_addr:-(not set)}"
}

_smtp_recipients() {
  clear
  echo ""
  echo -e "  ${BOLD}Notification Recipients${NC}"
  echo "$DIV"
  echo "  Current settings:"
  echo ""
  _smtp_recipient_summary

  echo ""
  read -r -p "  Notification email address: " email
  [ -z "$email" ] && { echo "  Cancelled."; press_enter; return; }

  # Build list of what will be changed
  echo ""
  echo "  Will configure → $email for:"
  echo "    • System/cron mail  (root alias in /etc/aliases)"
  [ -f /usr/local/maldetect/conf.maldet ]             && echo "    • Maldet"
  [ -f /etc/fail2ban/jail.d/wordpress.conf ] || \
  [ -f /etc/fail2ban/jail.local ]                     && echo "    • Fail2ban  (destemail + action_mwl)"
  [ -f /etc/apt/apt.conf.d/50unattended-upgrades ]    && echo "    • Unattended upgrades"
  command -v v-change-user-contact &>/dev/null        && echo "    • HestiaCP admin contact"

  if ! confirm "Apply?"; then return; fi

  echo ""

  # --- System/cron: root alias ---
  echo -e "  ${CYAN}→${NC} /etc/aliases  (root → $email)"
  if grep -q "^root:" /etc/aliases 2>/dev/null; then
    sed -i "s|^root:.*|root:     $email|" /etc/aliases
  else
    echo "root:     $email" >> /etc/aliases
  fi
  newaliases

  # --- Maldet ---
  if [ -f /usr/local/maldetect/conf.maldet ]; then
    echo -e "  ${CYAN}→${NC} Maldet"
    sed -i 's/^email_alert=.*/email_alert="1"/' /usr/local/maldetect/conf.maldet
    sed -i "s|^email_addr=.*|email_addr=\"$email\"|" /usr/local/maldetect/conf.maldet
  fi

  # --- Fail2ban ---
  local f2b_conf="/etc/fail2ban/jail.local"
  if [ -f "$f2b_conf" ] || command -v fail2ban-client &>/dev/null; then
    echo -e "  ${CYAN}→${NC} Fail2ban"
    if [ ! -f "$f2b_conf" ]; then
      printf '[DEFAULT]\ndestemail = %s\naction = %%(action_mwl)s\n' "$email" > "$f2b_conf"
    else
      # destemail
      if grep -q "^destemail" "$f2b_conf"; then
        sed -i "s|^destemail.*|destemail = $email|" "$f2b_conf"
      else
        _ini_insert "$f2b_conf" "DEFAULT" "destemail = $email"
      fi
      # action — set to action_mwl (ban + email with whois + log)
      if grep -q "^action\b" "$f2b_conf"; then
        sed -i 's|^action\b.*|action = %(action_mwl)s|' "$f2b_conf"
      else
        _ini_insert "$f2b_conf" "DEFAULT" "action = %(action_mwl)s"
      fi
    fi
    systemctl is-active --quiet fail2ban && systemctl reload fail2ban
  fi

  # --- Unattended upgrades ---
  if [ -f /etc/apt/apt.conf.d/50unattended-upgrades ]; then
    echo -e "  ${CYAN}→${NC} Unattended upgrades"
    local uu_file="/etc/apt/apt.conf.d/50unattended-upgrades"
    if grep -q 'Unattended-Upgrade::Mail ' "$uu_file"; then
      sed -i "s|.*Unattended-Upgrade::Mail \".*\"|Unattended-Upgrade::Mail \"$email\";|" "$uu_file"
    else
      echo "Unattended-Upgrade::Mail \"$email\";" >> "$uu_file"
    fi
    # Also enable MailReport if not set
    if ! grep -q '^Unattended-Upgrade::MailReport' "$uu_file"; then
      echo 'Unattended-Upgrade::MailReport "on-change";' >> "$uu_file"
    fi
  fi

  # --- HestiaCP admin contact ---
  if command -v v-change-user-contact &>/dev/null; then
    echo -e "  ${CYAN}→${NC} HestiaCP admin contact"
    v-change-user-contact admin "$email"
  fi

  echo ""
  echo -e "  ${GREEN}✓ Done${NC}"
  press_enter
}

# Insert a line after a [SECTION] header in an ini-style file
_ini_insert() {
  local file="$1" section="$2" line="$3"
  if grep -q "^\[${section}\]" "$file"; then
    sed -i "/^\[${section}\]/a ${line}" "$file"
  else
    printf '\n[%s]\n%s\n' "$section" "$line" >> "$file"
  fi
}

_smtp_install_deps() {
  local missing=()
  local needs_configure=false

  for pkg in postfix libsasl2-modules mailutils; do
    dpkg -l "$pkg" 2>/dev/null | grep -q "^ii" || missing+=("$pkg")
  done

  if dpkg -l postfix 2>/dev/null | grep -q "^ii" && [ ! -f "$_POSTFIX_CF" ]; then
    needs_configure=true
  fi

  if [ ${#missing[@]} -eq 0 ] && [ "$needs_configure" = false ]; then
    echo ""
    echo "  All dependencies are already installed."
    press_enter; return
  fi

  echo ""
  [ ${#missing[@]} -gt 0 ] && echo "  Missing packages: ${missing[*]}"
  [ "$needs_configure" = true ] && \
    echo "  Postfix is installed but has no main.cf — will reconfigure."

  if ! confirm "Proceed?"; then return; fi

  echo ""

  if [ ${#missing[@]} -gt 0 ]; then
    echo -e "  ${CYAN}→${NC} apt-get install -y ${missing[*]}"
    apt-get install -y "${missing[@]}"
    if [ $? -ne 0 ]; then
      echo -e "  ${RED}✗ Install failed${NC}"; press_enter; return
    fi
  fi

  if [ "$needs_configure" = true ] || { [[ " ${missing[*]} " =~ " postfix " ]] && [ ! -f "$_POSTFIX_CF" ]; }; then
    echo -e "  ${CYAN}→${NC} Configuring Postfix (non-interactive)..."
    local fqdn
    fqdn=$(hostname -f)
    echo "postfix postfix/mailname string $fqdn" | debconf-set-selections
    echo "postfix postfix/main_mailer_type string 'Internet Site'" | debconf-set-selections
    DEBIAN_FRONTEND=noninteractive dpkg-reconfigure postfix
    if [ ! -f "$_POSTFIX_CF" ]; then
      echo -e "  ${RED}✗ main.cf still missing — try manually: dpkg-reconfigure postfix${NC}"
      press_enter; return
    fi
  fi

  echo ""
  echo -e "  ${GREEN}✓ Done${NC}"
  press_enter
}

_smtp_configure() {
  if ! command -v postconf &>/dev/null; then
    echo "  Postfix is not installed. Use option 1 first."
    press_enter; return
  fi
  if [ ! -f "$_POSTFIX_CF" ]; then
    echo "  Postfix has no main.cf. Use option 1 first."
    press_enter; return
  fi
  if ! dpkg -l libsasl2-modules 2>/dev/null | grep -q "^ii"; then
    echo "  libsasl2-modules is not installed. Use option 1 first."
    press_enter; return
  fi

  echo ""
  echo -e "  ${BOLD}Configure Resend SMTP relay${NC}"
  echo "$DIV"
  echo "  Resend SMTP:  smtp.resend.com:587  (STARTTLS)"
  echo "  Username:     resend"
  echo "  Password:     your Resend API key"
  echo ""
  echo "  You need a verified sending domain in your Resend account."
  echo "  API keys: resend.com/api-keys"
  echo ""

  read -r -s -p "  Resend API key (re_...): " api_key
  echo ""
  if [ -z "$api_key" ]; then echo "  Cancelled."; press_enter; return; fi

  echo ""
  echo "  Sender rewriting maps system mail (root@hostname, cron, fail2ban, etc.)"
  echo "  to a real address on your verified Resend domain."
  echo "  Leave blank to skip (but system emails may bounce)."
  echo ""
  read -r -p "  Sender address for rewriting (e.g. noreply@yourdomain.fi): " sender_email

  echo ""

  if [ ! -f "${_POSTFIX_CF}.bak" ]; then
    cp "$_POSTFIX_CF" "${_POSTFIX_CF}.bak"
    echo -e "  ${CYAN}→${NC} Backed up main.cf → main.cf.bak"
  fi

  echo -e "  ${CYAN}→${NC} Writing $_SASL_PASSWD"
  printf '[smtp.resend.com]:587 resend:%s\n' "$api_key" > "$_SASL_PASSWD"
  postmap "$_SASL_PASSWD"
  chmod 600 "$_SASL_PASSWD" "${_SASL_PASSWD}.db"

  echo -e "  ${CYAN}→${NC} Updating Postfix relay settings"
  postconf -e \
    "relayhost = [smtp.resend.com]:587" \
    "smtp_sasl_auth_enable = yes" \
    "smtp_sasl_password_maps = hash:$_SASL_PASSWD" \
    "smtp_sasl_security_options = noanonymous" \
    "smtp_tls_security_level = encrypt" \
    "smtp_tls_CAfile = /etc/ssl/certs/ca-certificates.crt"

  if [ -n "$sender_email" ]; then
    echo -e "  ${CYAN}→${NC} Setting up sender rewriting → $sender_email"
    local fqdn
    fqdn=$(hostname -f)
    {
      echo "root              $sender_email"
      echo "www-data          $sender_email"
      echo "@${fqdn}          $sender_email"
      echo "@localhost        $sender_email"
    } > "$_GENERIC_MAP"
    postmap "$_GENERIC_MAP"
    postconf -e "smtp_generic_maps = hash:$_GENERIC_MAP"
  fi

  echo -e "  ${CYAN}→${NC} Reloading Postfix"
  systemctl reload postfix 2>/dev/null || systemctl restart postfix

  echo ""
  echo -e "  ${GREEN}✓ Done${NC}"
  echo ""
  echo "  Use option 4 to send a test email."
  press_enter
}

_smtp_test() {
  echo ""
  read -r -p "  Send test email to: " to_addr
  [ -z "$to_addr" ] && { echo "  Cancelled."; press_enter; return; }

  echo ""
  echo -e "  ${CYAN}→${NC} Sending..."

  local subject="HestiaCP SMTP test — $(hostname) — $(date '+%Y-%m-%d %H:%M')"
  local body="Test email from $(hostname -f) via Resend SMTP relay."

  if command -v mail &>/dev/null; then
    echo "$body" | mail -s "$subject" "$to_addr"
  else
    printf 'Subject: %s\n\n%s\n' "$subject" "$body" | sendmail "$to_addr"
  fi

  if [ $? -eq 0 ]; then
    echo -e "  ${GREEN}✓ Accepted by Postfix${NC}"
    echo "  Check your inbox (and spam folder)."
    echo ""
    echo "  Postfix log:"
    journalctl -u postfix --since "30 seconds ago" --no-pager 2>/dev/null | \
      grep -v "^-- " | tail -10 | sed 's/^/     /'
  else
    echo -e "  ${RED}✗ Error — check logs:${NC}"
    echo "    journalctl -u postfix --since '1 min ago'"
  fi
  press_enter
}
