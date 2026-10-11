#!/usr/bin/env bash
# Runs inside ubuntu:24.04 / ubuntu:26.04 (make docker-install-test): cgctl
# install --plan, then cgctl install, with systemd not PID 1 and the apt
# steps skipped (internal/install/SPEC.md §8). Validates what a container
# can: the rendered files, users, directories, nginx -t, sshd -t, nginx
# answering on 80/443/8443, and a second plan and doctor finding nothing.
#
#   install-container-check.sh <dir with cgctl, panel-api, panel-agent>
set -euo pipefail
build=${1:?build dir}
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null
apt-get install -y -qq nginx openssh-server logrotate curl >/dev/null 2>&1
. /etc/os-release
say() { printf '== %s: %s\n' "$PRETTY_NAME" "$*"; }
die() { printf 'FAIL (%s): %s\n' "$PRETTY_NAME" "$*"; exit 1; }
cg="$build/cgctl"
# What a booted server has and a container lacks: sshd's runtime directory
# and host keys.
mkdir -p /run/sshd && ssh-keygen -A >/dev/null

# The bootstrap (installer/install.sh, SPEC §6), with curl and uname stood in
# for (no network, and this container may not be amd64): a fake release whose
# cgctl records how it was called.
if [[ -f /i/install.sh ]]; then
  say "bootstrap"
  fake=$(mktemp -d); rel=$fake/rel; mkdir -p "$rel" "$fake/bin"
  printf '#!/bin/sh\necho x86_64\n' >"$fake/bin/uname"
  cat >"$fake/bin/curl" <<'CURL'
#!/bin/sh
out=""; url=""
while [ $# -gt 0 ]; do case $1 in -o) out=$2; shift 2 ;; -*) [ "$1" = --proto ] || [ "$1" = --proto-redir ] && shift; shift ;; *) url=$1; shift ;; esac; done
echo "$url" >>"$FAKE/urls"
cp "$FAKE/rel/$(basename "$url")" "$out"
CURL
  printf '#!/bin/sh\necho "args=$* version=$CLOUDGROUND_VERSION" >"$FAKE/called"\n' >"$rel/cgctl"
  chmod +x "$fake/bin/"* "$rel/cgctl"
  for f in SHA256SUMS SHA256SUMS.sig panel-api panel-agent cg-site-shell; do echo "$f" >"$rel/$f"; done
  sum=$(sha256sum "$rel/cgctl" | cut -d' ' -f1)
  sed "s/@CGCTL_VERSION@/v9.8.7/; s/@CGCTL_SHA256@/$sum/" /i/install.sh >"$rel/install.sh"
  run() { env FAKE="$fake" PATH="$fake/bin:$PATH" "$@"; }
  out=$(run bash /i/install.sh 2>&1) && die "an unstamped bootstrap ran: $out"
  grep -q "names no release" <<<"$out" || die "unstamped bootstrap: $out"
  run bash "$rel/install.sh" || die "stamped bootstrap failed"
  grep -q "args=install --release /tmp/.* --remove-release-dir version=v9.8.7" "$fake/called" || die "stamped: $(cat "$fake/called")"
  grep -q "/releases/download/v9.8.7/cgctl" "$fake/urls" || die "stamped bootstrap fetched $(cat "$fake/urls")"
  rm -f "$fake/called" "$fake/urls"
  run env CLOUDGROUND_VERSION=latest bash /i/install.sh || die "latest via an unstamped copy failed"
  grep -q "/releases/latest/download/install.sh" "$fake/urls" || die "latest: $(cat "$fake/urls")"
  grep -q "version=v9.8.7" "$fake/called" || die "latest did not run the latest release's own copy"
  out=$(run env CLOUDGROUND_VERSION=latest CLOUDGROUND_INSTALL_SHA256=0000 bash /i/install.sh 2>&1) && die "a wrong CLOUDGROUND_INSTALL_SHA256 ran"
  grep -q "does not match CLOUDGROUND_INSTALL_SHA256" <<<"$out" || die "install sha: $out"
  echo tampered >>"$rel/cgctl"; rm -f "$fake/called"
  before=$(ls -d /tmp/tmp.* 2>/dev/null | wc -l)
  out=$(run bash "$rel/install.sh" 2>&1) && die "a tampered cgctl ran"
  [[ ! -e $fake/called ]] || die "the tampered cgctl was run"
  [[ $(ls -d /tmp/tmp.* 2>/dev/null | wc -l) == "$before" ]] || die "the refused download was left in /tmp"
  rm -rf "$fake"
fi

# What the machine looks like before CloudGround, for the uninstall below
# (SPEC §9.5): every path outside the pseudo filesystems, and nginx.conf.
snapshot() {
  find / -xdev \( -path /proc -o -path /sys -o -path /dev -o -path /tmp -o -path /var/tmp -o -path /root/cloudground-uninstall \
    -o -path /var/log -o -path /var/cache/apt -o -path /var/lib/apt -o -path /var/lib/dpkg -o -path /var/cache/debconf -o -path /run/sshd \
    -o -path /run/nginx.pid -o -path /var/lib/nginx \) -prune -o -print | sort
}
snapshot >/tmp/before.paths
cp /etc/nginx/nginx.conf /tmp/nginx.conf.before
default_site=$(readlink /etc/nginx/sites-enabled/default || true)

say "cgctl install --plan"
plan=$("$cg" install --plan --container --skip-packages --from "$build") || die "plan: $plan"
echo "$plan"
grep -q '^+ nginx.vhost' <<<"$plan" || die "the plan of a fresh container does not write the vhost"
[[ ! -e /etc/nginx/sites-enabled/cloudground-panel-bootstrap.conf ]] || die "--plan wrote a file"

say "cgctl install"
"$cg" install --container --skip-packages --from "$build" || { tail -n 30 /var/log/cloudground-install.log; die "install"; }

say "second plan"
again=$("$cg" install --plan --container --skip-packages --from "$build")
if grep -q '^+' <<<"$again"; then echo "$again"; die "a second plan still has work"; fi

say "doctor"
"$cg" doctor --container --skip-packages || die "doctor found drift right after the install"

id cloudground >/dev/null || die "no user cloudground"
getent group cg-sftp-off >/dev/null || die "no group cg-sftp-off"
[[ $(stat -c '%a %U:%G' /var/lib/cloudground) == "750 cloudground:cloudground" ]] || die "/var/lib/cloudground: $(stat -c '%a %U:%G' /var/lib/cloudground)"
for d in /srv/sites /var/log/cloudground /var/cache/cloudground /var/www/cloudground-acme /etc/cloudground-php; do
	[[ $(stat -c '%a %U:%G' "$d") == "755 root:root" ]] || die "$d: $(stat -c '%a %U:%G' "$d"), want 755 root:root"
done
[[ $(stat -c '%a %U:%G' /etc/cloudground/agent.token) == "640 root:cloudground" ]] || die "agent.token: $(stat -c '%a %U:%G' /etc/cloudground/agent.token)"
[[ $(stat -c '%a %U:%G' /etc/cloudground/certs/fallback.key) == "640 root:www-data" ]] || die "fallback.key: $(stat -c '%a %U:%G' /etc/cloudground/certs/fallback.key)"
for b in cgctl panel-api panel-agent cg-site-shell; do cmp -s "$build/$b" "/usr/local/bin/$b" || die "$b not installed"; done
grep -q '^worker_connections\|worker_connections 4096;' /etc/nginx/nginx.conf || die "nginx.conf not tuned"
grep -q 'cloudground-tuned' /etc/nginx/nginx.conf || die "nginx.conf has no marker"
[[ -f /etc/systemd/system/cloudground-api.socket ]] || die "units not written"

# systemd is not PID 1 here, so the units never start; what can be checked
# is what made the agent fail at NAMESPACE on a fresh 26.04: every path the
# units' sandbox names without "-" exists after the install (this image has
# neither certbot nor cron, like a fresh server before their first run).
say "unit paths"
n=0
for u in /etc/systemd/system/cloudground-*.service; do
  while IFS= read -r p; do
    n=$((n + 1))
    [[ -e $p ]] || die "$(basename "$u") names $p, which does not exist: systemd would fail the unit at NAMESPACE"
  done < <(sed -nE 's/^(ReadWritePaths|ReadOnlyPaths|BindPaths|BindReadOnlyPaths|InaccessiblePaths|ExecPaths|NoExecPaths|TemporaryFileSystem)=//p' "$u" |
    tr ' ' '\n' | grep -v '^-' | sed -E 's/^\+//; s/:.*//' | grep . || true)
done
(( n >= 10 )) || die "found only $n unit paths: the parser missed them"
echo "$n paths, all present"

say "nginx -t"
nginx -t
say "sshd -t"
sshd -t

say "nginx answers"
nginx
sleep 1
code=0; curl -s -o /dev/null http://127.0.0.1/ || code=$?
[[ $code == 52 ]] || die "port 80 for an unknown name: curl exit $code, want 52 (444: no reply)"
code=0; curl -sk -o /dev/null https://127.0.0.1/ || code=$?
# 52 (empty reply) or 56 (connection closed in TLS, newer curl): no response.
[[ $code == 52 || $code == 56 ]] || die "port 443 for an unknown name: curl exit $code, want 52 or 56"
status=$(curl -sk -o /dev/null -w '%{http_code}' https://127.0.0.1:8443/)
[[ $status == 502 ]] || die "panel port 8443: HTTP $status, want 502 (no API in a container)"
status=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8443/)
[[ $status == 301 ]] || die "plain HTTP to 8443: HTTP $status, want 301 to https"
nginx -s stop

# Uninstall (SPEC §9.5): --plan changes nothing; the default tier leaves no
# file of CloudGround's and puts nginx.conf and the default site back; a
# second run has nothing to do; then the install works again.
say "cgctl uninstall --plan"
"$cg" uninstall --plan || die "uninstall --plan"
[[ -e /usr/local/bin/panel-agent && -e /etc/cloudground/agent.token ]] || die "uninstall --plan removed something"
mkdir -p /srv/sites/kept.test/current && echo kept >/srv/sites/kept.test/current/index.html
say "cgctl uninstall --yes"
"$cg" uninstall --yes || die "uninstall"
[[ $(cat /srv/sites/kept.test/current/index.html) == kept ]] || die "the default tier removed a site's files"
rm -rf /srv/sites/kept.test
left=$(snapshot | comm -13 /tmp/before.paths - | grep -v '^/srv/sites$' || true)
[[ -z $left ]] || die "the uninstall left these behind: $left"
gone=$(snapshot | comm -23 /tmp/before.paths - | grep -v '^/etc/nginx/sites-enabled/default$' || true)
[[ -z $gone ]] || die "the uninstall removed what was there before: $gone"
cmp -s /etc/nginx/nginx.conf /tmp/nginx.conf.before || die "nginx.conf is not the distribution's again: $(diff /tmp/nginx.conf.before /etc/nginx/nginx.conf)"
if [[ -n $default_site ]]; then
  [[ $(readlink /etc/nginx/sites-enabled/default) == "$default_site" ]] || die "the default site was not put back"
fi
for name in cloudground cg-sftp-off cg-sftp-keys cg-shell; do
  ! getent group "$name" >/dev/null || die "group $name is still there"
done
! id cloudground >/dev/null 2>&1 || die "user cloudground is still there"
for f in $(find / -xdev \( -path /proc -o -path /sys -o -path /tmp -o -path /root/cloudground-uninstall \) -prune -o \( -name '*cloudground*' -o -name 'cg-site*' \) -print); do
  die "a file named after CloudGround is left: $f"
done
nginx -t || die "nginx -t after the uninstall"
sshd -t || die "sshd -t after the uninstall"
again=$("/usr/bin/env" PATH="$build:$PATH" cgctl uninstall --yes) || die "a second uninstall failed: $again"
grep -q "nothing to remove" <<<"$again" || die "a second uninstall still had work: $again"

say "install again"
"$cg" install --container --skip-packages --from "$build" || { tail -n 30 /var/log/cloudground-install.log; die "reinstall"; }
"$cg" doctor --container --skip-packages || die "doctor found drift after the reinstall"
nginx -t
say "OK"
