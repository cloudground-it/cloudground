#!/usr/bin/env bash
# Builds the benchmark site on a CloudGround server (bench/SPEC.md §1).
#
#   sudo PANEL_EMAIL=... PANEL_PW=... IP=<public-ip> bash bench/setup-workload.sh
#
# A WooCommerce site at bench.<ip>.sslip.io with 100 posts, 50 products, the
# default theme and a 1 KB file at /bench-static.txt. Safe to run again: an
# existing bench site is reused and only topped up. The last line is the
# target for bench/wrk-suite.sh.
set -euo pipefail

IP="${IP:?set IP=<public ip>}"
EMAIL="${PANEL_EMAIL:?set PANEL_EMAIL}"
PW="${PANEL_PW:?set PANEL_PW}"
SOCK="${API_SOCK:-/run/cloudground/api.sock}" # root and nginx can open it
BASE=http://localhost
DOMAIN="bench.${IP//./-}.sslip.io"
POSTS=100
PRODUCTS=50

# The newest PHP 8.x that is really installed: its FPM binary exists. A
# directory under /etc/php proves nothing, a failed provision leaves one.
if [[ -z "${PHPV:-}" ]]; then
  for bin in /usr/sbin/php-fpm8.*; do
    v="${bin#/usr/sbin/php-fpm}"
    [[ -x "$bin" && "$v" =~ ^8\.[0-9]+$ ]] && PHPV="$v"
  done
fi
[[ -n "${PHPV:-}" ]] || { echo "no PHP 8 FPM binary in /usr/sbin" >&2; exit 1; }

cookies=$(mktemp)
trap 'rm -f "$cookies"' EXIT
api() { curl -sk --unix-socket "$SOCK" -b "$cookies" -c "$cookies" "$@"; }
field() { python3 -c 'import json,sys; v=json.load(sys.stdin)
for k in sys.argv[1:]: v=v[k]
print(v)' "$@" 2>/dev/null; }

api -o /dev/null -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,sys; print(json.dumps({"email": sys.argv[1], "password": sys.argv[2]}))' "$EMAIL" "$PW")" \
  "$BASE/api/login"

site=$(api "$BASE/api/sites" | python3 -c 'import json,sys
d=json.load(sys.stdin)
for s in (d if isinstance(d, list) else d.get("sites", [])):
    if s.get("domain")==sys.argv[1]: print(s["id"])' "$DOMAIN" 2>/dev/null || true)

if [[ -z "$site" ]]; then
  body=$(python3 -c 'import json,sys,secrets
print(json.dumps({"domain": sys.argv[1], "type": "woocommerce", "profile": "commerce",
  "php_version": sys.argv[2], "admin_email": sys.argv[3], "admin_user": "admin",
  "admin_password": secrets.token_urlsafe(18), "object_cache": True}))' "$DOMAIN" "$PHPV" "$EMAIL")
  site=$(api -H 'Content-Type: application/json' -d "$body" "$BASE/api/sites" | field site id)
  [[ -n "$site" ]] || { echo "creating $DOMAIN failed" >&2; exit 1; }
  # Provisioning installs WordPress and WooCommerce: allow five minutes.
  for _ in $(seq 60); do
    status=$(api "$BASE/api/sites/$site" | field status)
    case "$status" in
      active) break ;;
      error) echo "provisioning $DOMAIN failed" >&2; exit 1 ;;
    esac
    sleep 5
  done
  [[ "$status" == active ]] || { echo "$DOMAIN still $status after five minutes" >&2; exit 1; }
fi

owner=$(api "$BASE/api/sites/$site" | field system_user)
root="/srv/sites/$DOMAIN/current"
# Everything in the site tree is done as the site's user, never as root: the
# tree belongs to the tenant (AGENTS.md, on uutils and chown).
as_site() { sudo -u "$owner" -- "$@"; }
wp() { as_site wp --path="$root" "$@"; }

have=$(wp post list --post_type=post --post_status=publish --format=count)
if (( have < POSTS )); then
  wp post generate --count=$((POSTS - have)) --post_status=publish >/dev/null
fi
have=$(wp post list --post_type=product --format=count)
for ((n = have + 1; n <= PRODUCTS; n++)); do
  wp wc product create --user=1 --name="Bench product $n" --type=simple \
    --regular_price="$((10 + n)).00" --description="Benchmark product $n." >/dev/null
done

# 1,024 bytes, of a size no WordPress release can change.
printf '%01024d' 0 | tr 0 x | as_site tee "$root/bench-static.txt" >/dev/null

# WooCommerce's first-run steps (they run on a theme switch or a first visit
# to the admin) put a new store in "coming soon" mode: /shop/ then answers a
# placeholder with no product, twice as fast as the real page, and the
# uncached scenario measures the wrong thing (bench/uncached-php.md). Keep
# the store open, then check that the page lists the products. The flush
# goes through the generation file, so it reaches PHP-FPM's APCu.
wp option update woocommerce_coming_soon no >/dev/null
wp cache flush --skip-plugins --skip-themes >/dev/null
listed=$(curl -sk --resolve "$DOMAIN:443:127.0.0.1" "https://$DOMAIN/shop/?setup=$RANDOM" | grep -o 'Bench product [0-9]*' | sort -u | wc -l)
(( listed > 0 )) || { echo "https://$DOMAIN/shop/ lists no product: not the page the suite measures" >&2; exit 1; }

echo "site id $site"
echo "posts $(wp post list --post_type=post --post_status=publish --format=count), products $(wp post list --post_type=product --format=count)"
echo "WordPress $(wp core version), WooCommerce $(wp plugin get woocommerce --field=version), PHP $PHPV"
echo "target https://$DOMAIN"
