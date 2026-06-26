#!/bin/bash
# Nginx template management (wp-secure, wp-rocket)
#
# wp-rocket.tpl uses %proxy_port% + proxy_pass to %web_port% — these are
# HestiaCP proxy-mode placeholders. Proxy templates (nginx in front of Apache)
# live in nginx/, not nginx/php-fpm/. They show up in the UI as
# "Proxy Template (Nginx)". The php-fpm/ subdir is only relevant when the
# site has no Apache backend.
_NGINX_TPL_DIR="/usr/local/hestia/data/templates/web/nginx"

menu_nginx_templates() {
  while true; do
    clear
    echo ""
    echo -e "  ${BOLD}Nginx Templates${NC}"
    echo "$DIV"

    for name in "wp-secure" "wp-rocket"; do
      if [ -f "$_NGINX_TPL_DIR/${name}.tpl" ] && [ -f "$_NGINX_TPL_DIR/${name}.stpl" ]; then
        status_line "$name" OK "installed"
      else
        status_line "$name" ERR "not installed"
      fi
    done

    echo ""
    echo "  1) Install / update wp-secure + wp-rocket  (both proxy templates)"
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice

    case "$choice" in
      1) _nginx_install_profiles ;;
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
  echo "  This will install both proxy templates into $_NGINX_TPL_DIR:"
  echo ""
  echo "    wp-secure  — default.{tpl,stpl} + injected security rules"
  echo "                 (blocks hidden files, sensitive extensions, xmlrpc, bad bots)"
  echo "    wp-rocket  — WP Rocket cache-proxy template"
  echo "                 (the stpl serves cached HTML from /wp-content/cache/wp-rocket/"
  echo "                  directly, bypassing PHP on cache hits)"
  echo ""

  if ! confirm "Install both?"; then return; fi

  echo ""

  # --- wp-secure: copy default.{tpl,stpl}, inject snippet -------------------
  for ext in tpl stpl; do
    local src="$_NGINX_TPL_DIR/default.${ext}"
    local dst="$_NGINX_TPL_DIR/wp-secure.${ext}"

    echo -e "  ${CYAN}→${NC} cp $src $dst"
    cp "$src" "$dst"

    if grep -q "Valolink security rules" "$dst"; then
      echo "      Security rules already present, skipping injection."
    else
      echo -e "  ${CYAN}→${NC} Injecting security rules into $dst"
      # Insert snippet before the first 'location /' block
      awk -v snippet="$snippet" '
        /^    location \// && !done {
          while ((getline line < snippet) > 0) print line
          close(snippet)
          done=1
        }
        { print }
      ' "$dst" > "${dst}.tmp" && mv "${dst}.tmp" "$dst"
    fi
    echo -e "  ${GREEN}✓ $dst${NC}"
  done

  echo ""

  # --- wp-rocket: straight copy --------------------------------------------
  echo -e "  ${CYAN}→${NC} Copying wp-rocket.tpl"
  cp "$rocket_tpl" "$_NGINX_TPL_DIR/wp-rocket.tpl"
  echo -e "  ${CYAN}→${NC} Copying wp-rocket.stpl"
  cp "$rocket_stpl" "$_NGINX_TPL_DIR/wp-rocket.stpl"
  echo -e "  ${GREEN}✓ $_NGINX_TPL_DIR/wp-rocket.{tpl,stpl}${NC}"

  echo ""
  echo "  Apply in HestiaCP: Web → Edit domain → Advanced Options → Proxy Template"
  echo "    → wp-secure  (default security hardening)"
  echo "    → wp-rocket  (sites using WP Rocket)"
  press_enter
}
