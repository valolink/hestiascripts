#!/bin/bash
# Nginx template management (wp-secure, wp-rocket)

_NGINX_TPL_DIR="/usr/local/hestia/data/templates/web/nginx/php-fpm"

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
    echo "  1) Install / update wp-secure  (security hardening template)"
    echo "  2) Install / update wp-rocket  (WP Rocket cache proxy template)"
    echo "  0) Back"
    echo ""
    read -r -p "  Select: " choice

    case "$choice" in
      1) _nginx_install_wpsecure ;;
      2) _nginx_install_wprocket ;;
      0) return ;;
    esac
  done
}

_nginx_install_wpsecure() {
  local snippet="$SCRIPT_DIR/templates/nginx/wp-secure-snippet.conf"

  if [ ! -f "$snippet" ]; then
    echo "  Snippet not found: $snippet"; press_enter; return
  fi
  if [ ! -f "$_NGINX_TPL_DIR/default.tpl" ] || [ ! -f "$_NGINX_TPL_DIR/default.stpl" ]; then
    echo "  HestiaCP default templates not found in $_NGINX_TPL_DIR"
    press_enter; return
  fi

  echo ""
  echo "  This will:"
  echo "    1. Copy default.tpl  → wp-secure.tpl"
  echo "    2. Copy default.stpl → wp-secure.stpl"
  echo "    3. Inject security rules from templates/nginx/wp-secure-snippet.conf"
  echo "       (blocks hidden files, sensitive extensions, xmlrpc, bad bots)"
  echo ""

  if ! confirm "Install wp-secure templates?"; then return; fi

  echo ""

  for ext in tpl stpl; do
    local src="$_NGINX_TPL_DIR/default.${ext}"
    local dst="$_NGINX_TPL_DIR/wp-secure.${ext}"

    echo -e "  ${CYAN}→${NC} cp $src $dst"
    cp "$src" "$dst"

    if grep -q "Valolink security rules" "$dst"; then
      echo "  Security rules already present in $dst, skipping injection."
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
  echo "  Apply in HestiaCP: Web → Edit domain → Advanced Options → Web Template (Nginx) → wp-secure"
  press_enter
}

_nginx_install_wprocket() {
  local src_tpl="$SCRIPT_DIR/templates/nginx/wp-rocket.tpl"
  local src_stpl="$SCRIPT_DIR/templates/nginx/wp-rocket.stpl"

  for f in "$src_tpl" "$src_stpl"; do
    if [ ! -f "$f" ]; then
      echo "  Template not found: $f"; press_enter; return
    fi
  done

  echo ""
  echo "  This will copy wp-rocket.tpl and wp-rocket.stpl from the repo"
  echo "  into $_NGINX_TPL_DIR"
  echo ""
  echo "  The HTTPS template (stpl) serves WP Rocket cached HTML directly from"
  echo "  /wp-content/cache/wp-rocket/ — bypassing PHP entirely on cache hits."
  echo ""

  if ! confirm "Install wp-rocket templates?"; then return; fi

  echo ""
  echo -e "  ${CYAN}→${NC} Copying wp-rocket.tpl"
  cp "$src_tpl" "$_NGINX_TPL_DIR/wp-rocket.tpl"
  echo -e "  ${CYAN}→${NC} Copying wp-rocket.stpl"
  cp "$src_stpl" "$_NGINX_TPL_DIR/wp-rocket.stpl"
  echo ""
  echo -e "  ${GREEN}✓ Done${NC}"
  echo ""
  echo "  Apply in HestiaCP: Web → Edit domain → Advanced Options → Web Template (Nginx) → wp-rocket"
  press_enter
}
