#!/usr/bin/env bash
# Runs inside a distro container (make docker-nginx-test): installs the
# distro's nginx, puts the render matrix dumped by TestRenderMatrix
# (internal/engine/nginx, CG_NGINX_DUMP) in place, and runs `nginx -t` on all
# of it at once. The variant tested is the one this nginx build gets from
# the agent: "http2 on;" from 1.25.1, QUIC when the v3 module is there.
#
#   nginx-matrix-check.sh <matrix dir>
set -euo pipefail
matrix=${1:?matrix dir}
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null
apt-get install -y -qq nginx-core openssl >/dev/null 2>&1 || apt-get install -y -qq nginx openssl >/dev/null

info=$(nginx -V 2>&1)
ver=$(sed -n 's/.*nginx\/\([0-9.]*\).*/\1/p' <<<"$info" | head -1)
IFS=. read -r ma mi pa <<<"$ver"
build=nginx124
if ((ma > 1 || (ma == 1 && (mi > 25 || (mi == 25 && pa >= 1))))); then
  build=nginx126
  if grep -q http_v3_module <<<"$info"; then build=nginx128; fi
fi
. /etc/os-release
echo "== $PRETTY_NAME, nginx $ver: testing the $build renders"

# The installer's default servers (internal/install/templates/panel-bootstrap.conf, "Configuring
# nginx"): the one "reuseport" a QUIC address may carry is theirs.
rm -f /etc/nginx/sites-enabled/default
quic=""
if [ $build = nginx128 ]; then
  quic="listen 443 quic reuseport default_server; listen [::]:443 quic reuseport default_server;"
fi
cat >/etc/nginx/sites-enabled/cloudground-panel-bootstrap.conf <<NGINX
server { listen 80 default_server; listen [::]:80 default_server; server_name _; return 444; }
server {
    listen 443 ssl default_server; listen [::]:443 ssl default_server; $quic
    server_name _;
    ssl_certificate /srv/cgt/fallback/cert.pem; ssl_certificate_key /srv/cgt/fallback/key.pem;
    return 444;
}
NGINX

src=$matrix/$build
root=/srv/cgt
mkdir -p $root/acme $root/fallback $root/cache $root/php $root/auth
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=cgt -keyout $root/fallback/key.pem -out $root/fallback/cert.pem 2>/dev/null
n=0
for f in "$src"/sites-enabled/*.conf; do
  d=$(basename "$f" .conf)
  mkdir -p "$root/log/$d" "$root/certs/$d" "$root/sites/$d/current"
  cp $root/fallback/cert.pem "$root/certs/$d/fullchain.pem"
  cp $root/fallback/key.pem "$root/certs/$d/privkey.pem"
  cp "$f" /etc/nginx/sites-enabled/
  n=$((n + 1))
done
cp "$src"/conf.d/*.conf /etc/nginx/conf.d/
echo "== $n vhosts, $(ls "$src"/conf.d | wc -l) conf.d files"
out=$(nginx -t 2>&1) || { echo "$out"; echo "FAIL: nginx -t on $PRETTY_NAME"; exit 1; }
echo "$out" | grep -v -e 'syntax is ok' -e 'test is successful' || true
echo "OK: nginx -t on $PRETTY_NAME ($build)"
