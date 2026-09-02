server {
    listen      %ip%:%proxy_port%;
    server_name %domain_idn% %alias_idn%;
    error_log   /var/log/%web_system%/domains/%domain%.error.log error;

    # Hestia's "Force HTTPS" toggle (v-add-web-domain-ssl-force) writes this
    # file. Stock default.tpl includes it and this template did not, so the
    # toggle was a no-op at the nginx layer and plain-HTTP requests fell through
    # to the backend for a redirect WordPress or .htaccess happened to provide —
    # a full backend round-trip per request, and nothing at all if a non-https
    # rocket cache file ever existed. Note the name does not match the
    # `nginx.conf_*` glob at the bottom of this file; it needs its own include.
    include %home%/%user%/conf/web/%domain%/nginx.forcessl.conf*;

    # --- Valolink security rules: bad-bot UA block (from wp-secure-snippet) ---
    # Short-circuits before any cache logic so abusive crawlers cost ~nothing.
    if ($http_user_agent ~* (SemrushBot|AhrefsBot|MJ12bot|DotBot|PetalBot|MegaIndex|HTTrack|masscan)) {
        return 403;
    }

    # A cache hit here bypasses PHP entirely, so WP Rocket's own bypass rules
    # never get a chance to run — every exclusion has to be restated below.
    #
    # NOT SUPPORTED: WP Rocket's "Separate cache files for mobile devices". That
    # writes index-mobile.html, which this template never looks for, so enabling
    # it silently serves the desktop page to phones. Leave it off on any domain
    # using this template.
    set $rocket_file "/wp-content/cache/wp-rocket/$host${uri}index.html";

    if ($request_method !~ ^(GET|HEAD)$)                                               { set $rocket_file "/rocket-no-cache"; }
    if ($query_string != "")                                                            { set $rocket_file "/rocket-no-cache"; }
    if ($request_uri ~* "/(wp-admin/|wp-login\.php|wp-cron\.php|xmlrpc\.php|wp-json/|index\.php|feed/|sitemap.*\.xml|.*\.php)") { set $rocket_file "/rocket-no-cache"; }

    # Cart cookies are deliberately NOT listed here: a shopper holding a cart is
    # served the same cached page as an anonymous visitor, and the cart UI is
    # expected to repaint client-side (Store API / cart fragments).
    #
    # woocommerce_items_in_cart and woocommerce_cart_hash were added on
    # 2026-09-02 and removed the same day. The symptom was an empty mini-cart on
    # energiatuote.fi, but the cause was not the page cache — it was the asset
    # expiry rule below. `expires max` on a URL with no ?ver= froze the
    # mini-cart JS in returning browsers, so the Store API repaint never ran and
    # the cached page's empty cart stayed on screen. $vl_asset_expires fixes
    # that at source, so excluding cart holders from the cache is not needed and
    # costs a full PHP render to every shopper who has added anything.
    #
    # A site served by this template must therefore not render cart-dependent
    # markup server-side on cacheable pages. kuumalahde.fi's
    # custom_add_to_cart_notice() (valolink-functions) is one such case: the
    # nightly 00:02 preload bakes its empty-cart variant into every product and
    # category page, so shoppers see the generic free-shipping line rather than
    # a personalised remaining-amount.
    if ($http_cookie ~* "(wordpress_logged_in_|wp-postpass_|comment_author_)") { set $rocket_file "/rocket-no-cache"; }

    location ~ /\.(?!well-known\/|file) {
        deny all;
        return 404;
    }

    # --- Valolink security rules: sensitive file extensions (from wp-secure-snippet) ---
    location ~* (?:\.(?:bak|conf|dist|fla|in[ci]|log|psd|sh|sql|sw[op])|~)$ {
        deny all;
        access_log off;
        log_not_found off;
    }

    # --- Valolink security rules: xmlrpc deny (from wp-secure-snippet) ---
    # Blocks brute-force / pingback abuse before it reaches PHP.
    # If a site genuinely needs xmlrpc, switch its proxy template off wp-rocket.
    location = /xmlrpc.php {
        deny all;
        access_log off;
        log_not_found off;
    }

    location / {
        root %docroot%;

        # Code assets are split out from the block below so their expiry can
        # depend on whether the URL is bustable at all. This must come FIRST —
        # nested regex locations are tried in order and %proxy_extensions% also
        # covers css/js.
        #
        # `expires max` on a URL with no ?ver= freezes it in the browser for a
        # decade at an address that never changes: a plugin update swaps the
        # bytes on disk and returning visitors keep running the old file, since
        # a normal reload does not refetch subresources whose max-age still
        # holds. Only a hard reload does, which is the "broken until I
        # hard-refresh" report. $vl_asset_expires is `max` when the URL carries
        # ?ver= and 1h when it does not.
        location ~* ^.+\.(css|js|mjs)$ {
            try_files  $uri @fallback;
            root       %docroot%;
            access_log /var/log/%web_system%/domains/%domain%.log combined;
            access_log /var/log/%web_system%/domains/%domain%.bytes bytes;
            expires    $vl_asset_expires;
        }

        # Images, fonts and media keep the long expiry: stale media does not
        # break a site, and /wp-content/uploads carries no ?ver=, so including
        # it above would revalidate every image for no correctness gain.
        location ~* ^.+\.(%proxy_extensions%)$ {
            try_files  $uri @fallback;
            root       %docroot%;
            access_log /var/log/%web_system%/domains/%domain%.log combined;
            access_log /var/log/%web_system%/domains/%domain%.bytes bytes;
            expires    max;
        }

        # A cache hit is served from here as a plain static file, so nginx sends
        # ETag + Last-Modified and no Cache-Control — which browsers turn into a
        # heuristic freshness lifetime (~10% of the page's age) and reuse without
        # revalidating. The default that prevents that, and $vl_asset_expires
        # above, are both declared at http level in
        # /etc/nginx/conf.d/valolink-cache-headers.conf. That file MUST be
        # installed before this template is rebuilt into a domain conf, or nginx
        # will not start: setup/install-cache-headers.sh runs first for exactly
        # this reason.
        try_files  $rocket_file @backend;
        access_log /var/log/%web_system%/domains/%domain%.log combined;
        access_log /var/log/%web_system%/domains/%domain%.bytes bytes;
    }

    location @backend {
        proxy_pass http://%ip%:%web_port%;
    }

    location @fallback {
        proxy_pass http://%ip%:%web_port%;
    }

    location /error/ {
        alias %home%/%user%/web/%domain%/document_errors/;
    }

    proxy_hide_header Upgrade;

    include %home%/%user%/conf/web/%domain%/nginx.conf_*;
}
