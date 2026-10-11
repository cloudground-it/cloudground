#!/usr/bin/env bash
# Runs inside the container (see run.sh).
set -euo pipefail
{ apt-get update -qq && apt-get install -y -qq libfcgi-bin >/dev/null; } 2>/dev/null
{ pecl install apcu >/dev/null && docker-php-ext-enable apcu >/dev/null; } 2>/dev/null
cat >/usr/local/etc/php/conf.d/zz-bench.ini <<'INI'
apc.enabled=1
apc.enable_cli=0
apc.shm_size=128M
opcache.enable=1
INI

# The site layout the agent builds: the drop-in in shared/, linked from the
# live release.
site=/srv/site
mkdir -p $site/releases/r1/wp-content $site/shared
cp /dropin/object-cache.php $site/shared/object-cache.php
ln -s ../../../shared/object-cache.php $site/releases/r1/wp-content/object-cache.php
ln -s releases/r1 $site/current
cp /bench/*.php $site/releases/r1/
chown -R www-data:www-data $site

printf '[www]\naccess.log = /dev/null\n' >/usr/local/etc/php-fpm.d/zz-bench.conf
php-fpm -D
sleep 1
fpm() { # <script> [query]
  SCRIPT_FILENAME=$site/current/$1 QUERY_STRING="${2:-}" REQUEST_METHOD=GET \
    cgi-fcgi -bind -connect 127.0.0.1:9000 | tr -d '\r' | sed '1,/^$/d'
}
cli() { su -s /bin/sh www-data -c "php $site/current/cli.php $*"; }

echo "== PHP $(php -r 'echo PHP_VERSION;'), APCu $(php --ri apcu | awk '/^Version/ {print $3}')"
echo
echo "== the PHP suites with a shared APCu segment, as PHP-FPM has it"
su -s /bin/sh www-data -c "php -d apc.enable_cli=1 /dropin/tests/object-cache.php"
su -s /bin/sh www-data -c "php -d apc.enable_cli=1 /dropin/tests/connector.php"
echo
echo "== correctness"
cli set v1 >/dev/null
echo "FPM reads (fills its APCu):                 $(fpm read.php)"
cli set v2 >/dev/null
echo "control, wp-cli wrote v2, no flush:         $(fpm read.php)   (stale is expected: separate segments)"
echo "A: wp-cli flush (new generation):           $(cli flush)"
echo "   FPM reads:                               $(fpm read.php)"
cli set v3 >/dev/null
echo "   control before B:                        $(fpm read.php)"
echo "B: flush inside an FPM worker:              $(fpm flush.php)"
echo "   FPM reads:                               $(fpm read.php)"
chmod 0555 $site/shared
echo "A with shared/ unwritable (must be false):  $(cli flush)"
chmod 0755 $site/shared

echo
echo "== cost"
fpm fill.php n=50000 >/dev/null
echo "APCu entries after filling:                 $(fpm info.php)"
t0=$(date +%s%N); for i in $(seq 20); do cli flush >/dev/null; done; t1=$(date +%s%N)
echo "A: wp-cli process (drop-in only) flushing:  $(( (t1 - t0) / 20000000 )) ms each over 20 runs; real wp-cli adds its WordPress boot"
fpm read.php >/dev/null; sleep 1
echo "   entries after the next FPM request:      $(fpm info.php)   (orphans reclaimed after the response)"
fpm fill.php n=50000 >/dev/null
t0=$(date +%s%N); fpm flush.php >/dev/null; t1=$(date +%s%N)
echo "B: FPM flush request, 50000 entries:        $(( (t1 - t0) / 1000000 )) ms; production adds TLS, nginx and the WordPress boot"
fpm fill.php n=50000 >/dev/null
cli flush >/dev/null
t0=$(date +%s%N); fpm read.php >/dev/null; t1=$(date +%s%N)
echo "   first FPM request after A:               $(( (t1 - t0) / 1000000 )) ms to the end of the request"
fpm reclaimcost.php
for i in 1 2 3; do fpm read.php >/dev/null; done
t0=$(date +%s%N); for i in $(seq 500); do fpm read.php >/dev/null; done; t1=$(date +%s%N)
echo "an FPM request through the drop-in:         $(( (t1 - t0) / 500000 )) us average over 500, cgi-fcgi process included"
fpm epochcost.php
