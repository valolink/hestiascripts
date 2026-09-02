#!/bin/bash
# Nginx template management (wp-secure, wp-rocket)
#
# wp-rocket.tpl uses %proxy_port% + proxy_pass to %web_port% — these are
# HestiaCP proxy-mode placeholders. Proxy templates (nginx in front of Apache)
# live in nginx/, not nginx/php-fpm/. They show up in the UI as
# "Proxy Template (Nginx)". The php-fpm/ subdir is only relevant when the
# site has no Apache backend.
_NGINX_TPL_DIR="/usr/local/hestia/data/templates/web/nginx"

# The Apache backend template. wp-secure exists on both sides because they catch
# different traffic: nginx answers missing fonts/images at the proxy, Apache
# answers PHP probes and secret-hunting that the proxy passes through.
_APACHE_TPL_DIR="/usr/local/hestia/data/templates/web/apache2"

menu_nginx_templates() {
  while true; do
    clear
    echo ""
    echo -e "  ${BOLD}Web Templates${NC}"
    echo "$DIV"

    # wp-secure is only "installed" if the rules are actually in the file. The
    # file existing proves only that a cp ran.
    for name in "wp-secure" "wp-rocket"; do
      if [ ! -f "$_NGINX_TPL_DIR/${name}.tpl" ] || [ ! -f "$_NGINX_TPL_DIR/${name}.stpl" ]; then
        status_line "nginx $name" ERR "not installed"
      elif [ "$name" = "wp-secure" ] && ! grep -q "Valolink security rules" "$_NGINX_TPL_DIR/${name}.tpl"; then
        status_line "nginx $name" ERR "present but contains NO rules — reinstall"
      else
        status_line "nginx $name" OK "installed"
      fi
    done

    if [ ! -d "$_APACHE_TPL_DIR" ]; then
      status_line "apache2 wp-secure" "" "n/a — no Apache backend"
    elif [ -f "$_APACHE_TPL_DIR/wp-secure.tpl" ] && [ -f "$_APACHE_TPL_DIR/wp-secure.stpl" ]; then
      status_line "apache2 wp-secure" OK "installed"
    else
      status_line "apache2 wp-secure" ERR "not installed"
    fi

    # Compared by content, not existence — same lesson as wp-secure below. A
    # stale copy is the dangerous state: it looks installed and silently lacks
    # whatever the current version fixes.
    local cc_src="$SCRIPT_DIR/templates/nginx/valolink-cache-headers.conf"
    local cc_dst="/etc/nginx/conf.d/valolink-cache-headers.conf"
    if [ -f "/etc/nginx/conf.d/valolink-html-cache-control.conf" ]; then
      # Both files declare $vl_is_html — leaving the old one is a fatal
      # duplicate variable, so this is worse than "not installed".
      status_line "nginx cache headers" ERR "superseded file still present — reinstall"
    elif [ ! -f "$cc_dst" ]; then
      status_line "nginx cache headers" ERR "not installed"
    elif ! cmp -s "$cc_src" "$cc_dst"; then
      status_line "nginx cache headers" ERR "stale — reinstall"
    else
      status_line "nginx cache headers" OK "installed"
    fi

    echo ""
    echo "  1) Install / update all templates"
    echo "  2) Install / update cache-headers drop-in only"
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice

    case "$choice" in
      1) _nginx_install_profiles ;;
      2) echo ""; bash "$SCRIPT_DIR/setup/install-cache-headers.sh"; press_enter ;;
      0) return ;;
    esac
  done
}

_nginx_install_profiles() {
  local snippet="$SCRIPT_DIR/templates/nginx/wp-secure-snippet.conf"
  local rocket_tpl="$SCRIPT_DIR/templates/nginx/wp-rocket.tpl"
  local rocket_stpl="$SCRIPT_DIR/templates/nginx/wp-rocket.stpl"

  # Pre-flight: every source needs to exist before we touch anything.
  for f in "$snippet" "$rocket_tpl" "$rocket_stpl"; do
    if [ ! -f "$f" ]; then
      echo "  Source not found: $f"; press_enter; return
    fi
  done
  if [ ! -f "$_NGINX_TPL_DIR/default.tpl" ] || [ ! -f "$_NGINX_TPL_DIR/default.stpl" ]; then
    echo "  HestiaCP default templates not found in $_NGINX_TPL_DIR"
    press_enter; return
  fi

  echo ""
  echo "  This will install the proxy templates into $_NGINX_TPL_DIR:"
  echo ""
  echo "    wp-secure  — default.{tpl,stpl} + injected security rules"
  echo "                 (hidden files, sensitive extensions, xmlrpc, bad bots,"
  echo "                  and missing fonts/images answered without hitting PHP)"
  echo "    wp-rocket  — WP Rocket cache-proxy template"
  echo "                 (the stpl serves cached HTML from /wp-content/cache/wp-rocket/"
  echo "                  directly, bypassing PHP on cache hits)"
  echo ""
  echo "  …and into /etc/nginx/conf.d (http level, all vhosts, live on reload):"
  echo ""
  echo "    valolink-cache-headers.conf"
  echo "                 — Cache-Control for HTML that sets none of its own, and"
  echo "                   \$vl_asset_expires, which the templates below use to"
  echo "                   give css/js a long cache ONLY when the URL carries a"
  echo "                   ?ver= that can bust it."
  echo ""

  if ! confirm "Install both?"; then return; fi

  echo ""

  # --- cache-headers drop-in: MUST be first ---------------------------------
  # The templates below reference $vl_asset_expires. If the drop-in that
  # declares it is missing or stale when a domain is rebuilt onto one of these
  # templates, nginx fails to start on "unknown vl_asset_expires variable".
  # Install it first and refuse to write the templates if it did not land.
  echo -e "  ${CYAN}→${NC} Installing cache-headers drop-in (prerequisite)"
  if ! bash "$SCRIPT_DIR/setup/install-cache-headers.sh"; then
    echo ""
    echo -e "  ${RED}✗ Aborting — templates NOT installed.${NC}"
    echo "      They reference \$vl_asset_expires, which that drop-in declares."
    echo "      Writing them now would leave the box unable to reload nginx."
    press_enter; return
  fi

  echo ""

  # --- wp-secure: copy default.{tpl,stpl}, inject snippet -------------------
  for ext in tpl stpl; do
    local src="$_NGINX_TPL_DIR/default.${ext}"
    local dst="$_NGINX_TPL_DIR/wp-secure.${ext}"

    echo -e "  ${CYAN}→${NC} cp $src $dst"
    cp "$src" "$dst"

    # The SSL template roots its asset blocks at %sdocroot%, not %docroot%. On a
    # stock Hestia install public_shtml is a symlink to public_html so the two
    # resolve alike, but the stock .stpl uses %sdocroot% throughout and a site
    # with a genuinely separate SSL docroot would be served the wrong tree.
    # Match the template rather than lean on the symlink.
    local use_snippet="$snippet"
    if [ "$ext" = "stpl" ]; then
      use_snippet="$(mktemp)"
      sed 's/%docroot%/%sdocroot%/g' "$snippet" > "$use_snippet"
    fi

    if grep -q "Valolink security rules" "$dst"; then
      echo "      Security rules already present, skipping injection."
    else
      echo -e "  ${CYAN}→${NC} Injecting security rules into $dst"
      # Insert snippet before the first 'location /' block. Match leading
      # whitespace generically: Hestia's default.tpl is tab-indented, and an
      # earlier four-space pattern here never matched — cp still succeeded, so
      # every box got a wp-secure template containing no security rules at all,
      # and the file-exists status check called it installed. Hence the verify
      # step below: this must never fail quietly again.
      awk -v snippet="$use_snippet" '
        /^[[:space:]]*location \/[[:space:]]*\{/ && !done {
          while ((getline line < snippet) > 0) print line
          close(snippet)
          done=1
        }
        { print }
      ' "$dst" > "${dst}.tmp" && mv "${dst}.tmp" "$dst"
    fi

    [ "$use_snippet" != "$snippet" ] && rm -f "$use_snippet"

    if grep -q "Valolink security rules" "$dst"; then
      echo -e "  ${GREEN}✓ $dst${NC}"
    else
      echo -e "  ${RED}✗ $dst — injection produced no rules${NC}"
      echo "      No 'location / {' line matched in $_NGINX_TPL_DIR/default.${ext}."
      echo "      The template layout has changed; do not apply this template."
    fi
  done

  echo ""

  # --- wp-rocket: straight copy --------------------------------------------
  echo -e "  ${CYAN}→${NC} Copying wp-rocket.tpl"
  cp "$rocket_tpl" "$_NGINX_TPL_DIR/wp-rocket.tpl"
  echo -e "  ${CYAN}→${NC} Copying wp-rocket.stpl"
  cp "$rocket_stpl" "$_NGINX_TPL_DIR/wp-rocket.stpl"
  echo -e "  ${GREEN}✓ $_NGINX_TPL_DIR/wp-rocket.{tpl,stpl}${NC}"

  # --- apache2 wp-secure: inject into <Directory %docroot%> ------------------
  local ap_snippet="$SCRIPT_DIR/templates/apache2/wp-secure-snippet.conf"
  if [ -d "$_APACHE_TPL_DIR" ] && [ -f "$ap_snippet" ] && [ -f "$_APACHE_TPL_DIR/default.tpl" ]; then
    echo ""
    for ext in tpl stpl; do
      local asrc="$_APACHE_TPL_DIR/default.${ext}"
      local adst="$_APACHE_TPL_DIR/wp-secure.${ext}"
      [ -f "$asrc" ] || continue

      echo -e "  ${CYAN}→${NC} cp $asrc $adst"
      cp "$asrc" "$adst"

      if grep -q "Valolink security rules" "$adst"; then
        echo "      Security rules already present, skipping injection."
      else
        echo -e "  ${CYAN}→${NC} Injecting security rules into $adst"
        # After the <Directory %docroot%> line, not before: these are
        # per-directory rewrite rules and have to live inside that block.
        awk -v snippet="$ap_snippet" '
          { print }
          /^[[:space:]]*<Directory %docroot%>/ && !done {
            while ((getline line < snippet) > 0) print line
            close(snippet)
            done=1
          }
        ' "$adst" > "${adst}.tmp" && mv "${adst}.tmp" "$adst"
      fi

      if grep -q "Valolink security rules" "$adst"; then
        echo -e "  ${GREEN}✓ $adst${NC}"
      else
        echo -e "  ${RED}✗ $adst — injection produced no rules${NC}"
        echo "      No '<Directory %docroot%>' line matched; do not apply this template."
      fi
    done
  fi

  echo ""
  echo "  Apply in HestiaCP: Web → Edit domain → Advanced Options"
  echo "    Proxy Template (Nginx) → wp-secure   (security hardening)"
  echo "                           → wp-rocket   (sites using WP Rocket)"
  echo "    Web Template (Apache2) → wp-secure   (PHP-probe + secret rules)"
  echo ""
  echo -e "  ${YELLOW}Test on a staging domain first${NC} — v-wp-staging-create builds one."
  echo "  These templates change how missing files are answered; verify the site"
  echo "  renders and that fonts/images still load before rolling out to live."
  echo ""

  # A template file on disk changes nothing by itself — Hestia bakes it into
  # each domain's conf at rebuild time. Enumerate what is actually affected
  # rather than leaving the operator to guess.
  echo "  Domains already on these templates keep their OLD generated conf until"
  echo "  rebuilt. Affected here:"
  echo ""
  local found=0
  local uconf u
  for uconf in /usr/local/hestia/data/users/*/web.conf; do
    [ -f "$uconf" ] || continue
    u=$(basename "$(dirname "$uconf")")
    while IFS= read -r line; do
      found=1
      echo "    v-rebuild-web-domain $u $line"
    done < <(grep -oE "DOMAIN='[^']+'.*PROXY='wp-(rocket|secure)'" "$uconf" 2>/dev/null |
             sed -E "s/^DOMAIN='([^']+)'.*PROXY='(wp-[a-z]+)'.*/\1   # \2/")
  done
  [ "$found" -eq 0 ] && echo "    (none — no domain is assigned wp-rocket or wp-secure yet)"
  echo ""
  press_enter
}
