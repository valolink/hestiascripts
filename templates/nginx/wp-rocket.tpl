server {
    listen      %ip%:%proxy_port%;
    server_name %domain_idn% %alias_idn%;
    error_log   /var/log/%web_system%/domains/%domain%.error.log error;

    set $rocket_file "/wp-content/cache/wp-rocket/$host${uri}index.html";

    if ($request_method !~ ^(GET|HEAD)$)                                               { set $rocket_file "/rocket-no-cache"; }
    if ($query_string != "")                                                            { set $rocket_file "/rocket-no-cache"; }
    if ($request_uri ~* "/(wp-admin/|wp-login\.php|wp-cron\.php|xmlrpc\.php|wp-json/|index\.php|feed/|sitemap.*\.xml|.*\.php)") { set $rocket_file "/rocket-no-cache"; }
    if ($http_cookie ~* "(wordpress_logged_in_|wp-postpass_|comment_author_)")         { set $rocket_file "/rocket-no-cache"; }

    location ~ /\.(?!well-known\/|file) {
        deny all;
        return 404;
    }

    location / {
        root %docroot%;

        location ~* ^.+\.(%proxy_extensions%)$ {
            try_files  $uri @fallback;
            root       %docroot%;
            access_log /var/log/%web_system%/domains/%domain%.log combined;
            access_log /var/log/%web_system%/domains/%domain%.bytes bytes;
            expires    max;
        }

        try_files  $rocket_file @backend;
        access_log /var/log/%web_system%/domains/%domain%.log combined;
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
