#!/usr/bin/env bash
# Set up a fresh Ubuntu server to run the whole MHAI stack (this repo's
# backend and mhai-web's clinical app) on one domain, as on the demo host.
# How-to: deploy/NEW-SERVER.md.
#
# Run as the deploy user (ubuntu, with sudo), from a copy of this repo's
# deploy/ folder and of mhai-web's deploy/ folder:
#   bash deploy/new-server.sh --domain ai.example.org --email ops@example.org \
#     --web-repo-dir PATH [--demo] [--platform-domain platforms.example.org]
#
#   --domain           the site's name; its DNS A record must point here before
#                      the certificate step (the script stops there until it does)
#   --email            for Let's Encrypt (expiry notices)
#   --web-repo-dir     a checkout (or copy) of mhai-web: its deploy/ folder is used
#   --demo             this server is a public demo: copy the live-demo override
#                      and list the demo's settings at the end
#   --platform-domain  also set up mhai-web's platform admin app on this name:
#                      /srv/mhai-web-platform, its session secret, the nginx
#                      site with its IP allow list, and its certificate
#                      (webroot, as mhai-web's deploy/README.md)
#
# Every step says what it does. It is safe to run again: it installs only what
# is missing, keeps every file, secret, key and volume that exists (the
# backend's setup-host.sh does the same), and stops at the first error. It
# never prints a secret, never prunes images and never touches a container,
# or an nginx site, that it did not make. It starts no app: the two Deploy
# workflows do that once the GitHub secrets point here.

set -euo pipefail

DOMAIN=
EMAIL=
WEB_REPO_DIR=
DEMO=0
PLATFORM_DOMAIN=
WEB_DEST=${MHAI_WEB_DIR:-/srv/mhai-web}
BACKEND_DEST=${MHAI_BACKEND_DIR:-/srv/mhai-backend}
SITE_NAME=mhai-demo                 # as on the demo host
SITE=/etc/nginx/sites-available/$SITE_NAME
WEB_PORT=8090                       # mhai-web's compose.yml: 127.0.0.1:8090
WEB_UID=1001                        # mhai-web's container user (nextjs)
PLATFORM_DEST=${MHAI_WEB_PLATFORM_DIR:-/srv/mhai-web-platform}
PLATFORM_SITE_NAME=mhai-platform    # as on the demo host
PLATFORM_SITE=/etc/nginx/sites-available/$PLATFORM_SITE_NAME
PLATFORM_ALLOW=/etc/nginx/mhai-platform-allow.conf
PLATFORM_PORT=8091                  # mhai-web's compose.platform.yml: 127.0.0.1:8091

usage() {
  echo "usage: bash deploy/new-server.sh --domain DOMAIN --email EMAIL --web-repo-dir PATH [--demo] [--platform-domain DOMAIN]" >&2
  exit 2
}
while [ $# -gt 0 ]; do
  case "$1" in
    --domain) [ $# -ge 2 ] || usage; DOMAIN=$2; shift 2 ;;
    --email) [ $# -ge 2 ] || usage; EMAIL=$2; shift 2 ;;
    --web-repo-dir) [ $# -ge 2 ] || usage; WEB_REPO_DIR=$2; shift 2 ;;
    --demo) DEMO=1; shift ;;
    --platform-domain) [ $# -ge 2 ] || usage; PLATFORM_DOMAIN=$2; shift 2 ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
done

SRC=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
OWNER=$(id -un)

say()  { printf '\n==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

# A lower-case host name of at least two labels: it goes into the nginx site
# and certbot's command line, so nothing else is let through.
is_host() { printf '%s' "$1" | grep -qE '^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$'; }
[ -n "$DOMAIN" ] || usage
DOMAIN=$(printf '%s' "$DOMAIN" | tr '[:upper:]' '[:lower:]')
is_host "$DOMAIN" || die "--domain must be a host name such as ai.example.org"
if [ -n "$PLATFORM_DOMAIN" ]; then
  PLATFORM_DOMAIN=$(printf '%s' "$PLATFORM_DOMAIN" | tr '[:upper:]' '[:lower:]')
  is_host "$PLATFORM_DOMAIN" || die "--platform-domain must be a host name such as platforms.example.org"
  [ "$PLATFORM_DOMAIN" != "$DOMAIN" ] || die "--platform-domain must differ from --domain"
fi
[ -n "$EMAIL" ] || usage
printf '%s' "$EMAIL" | grep -qE '^[A-Za-z0-9._%+-]+@([A-Za-z0-9-]+\.)+[A-Za-z]{2,63}$' \
  || die "--email must be a plain address such as ops@example.org"
[ -n "$WEB_REPO_DIR" ] || die "--web-repo-dir is required: a checkout of mhai-web (git clone https://github.com/Measured-Health/mhai-web), or a copy of its deploy/ folder's parent"
WEB_SRC=$(cd "$WEB_REPO_DIR" 2>/dev/null && pwd)/deploy || die "--web-repo-dir $WEB_REPO_DIR does not exist"

# ---------------------------------------------------------------------------
say "1/9 Checking this server"
[ "$(id -u)" -ne 0 ] || die "run as the deploy user (ubuntu), not root; it uses sudo where it needs to"
[ -r /etc/os-release ] || die "no /etc/os-release: this script is for Ubuntu"
# shellcheck disable=SC1091
. /etc/os-release
[ "${ID:-}" = ubuntu ] || die "this script is for Ubuntu (found ${ID:-unknown})"
info "Ubuntu ${VERSION_ID:-?} (${VERSION_CODENAME:-?})"
# The Deploy workflows build every image for linux/amd64 and linux/arm64.
case "$(uname -m)" in
  aarch64|arm64) info "arm64 (the images are multi-arch)" ;;
  x86_64|amd64) info "x86_64 (the images are multi-arch)" ;;
  *) die "this server is $(uname -m); the Deploy workflows build amd64 and arm64 images only" ;;
esac
# A GPU is only noted: the stack runs on CPU today (the intelligence images
# use CPU PyTorch) and nothing here configures the GPU.
if command -v nvidia-smi >/dev/null 2>&1 && gpu=$(nvidia-smi --query-gpu=name --format=csv,noheader 2>/dev/null | head -n1) && [ -n "$gpu" ]; then
  info "NVIDIA GPU found ($gpu); not used by the stack yet (CPU images), nothing configured for it"
elif [ -d /proc/driver/nvidia ] || { command -v lspci >/dev/null 2>&1 && lspci 2>/dev/null | grep -qi nvidia; }; then
  info "NVIDIA GPU found; not used by the stack yet (CPU images), nothing configured for it"
fi
command -v sudo >/dev/null || die "sudo is not installed"
# -n: no prompt (sudo -v asks for a terminal on Ubuntu 26.04 even with NOPASSWD).
sudo -n true 2>/dev/null || die "$OWNER cannot use sudo without a password (passwordless sudo is needed, as for ubuntu on cloud images)"
info "sudo works for $OWNER"
mem_mb=$(awk '/^MemTotal:/ {print int($2/1024)}' /proc/meminfo)
if [ "$mem_mb" -lt 3500 ]; then
  echo "    warning: ${mem_mb} MiB of memory; the stack is run on 4 GiB on the demo host" >&2
else
  info "${mem_mb} MiB of memory"
fi
for f in setup-host.sh compose.yml nats/nats.conf; do
  [ -f "$SRC/$f" ] || die "$SRC/$f is missing: run this from a copy of mhai-multiagent's deploy/ folder"
done
for f in compose.yml nginx/live-proxy.conf.template; do
  [ -f "$WEB_SRC/$f" ] || die "$WEB_SRC/$f is missing: --web-repo-dir must be a checkout of mhai-web"
done
if [ "$DEMO" = 1 ]; then
  [ -f "$WEB_SRC/compose.demo-live.yml" ] || die "$WEB_SRC/compose.demo-live.yml is missing (needed for --demo)"
fi
if [ -n "$PLATFORM_DOMAIN" ]; then
  for f in compose.platform.yml nginx/mhai-platform.conf; do
    [ -f "$WEB_SRC/$f" ] || die "$WEB_SRC/$f is missing (needed for --platform-domain)"
  done
fi
info "deploy files found in $SRC and $WEB_SRC"

# ---------------------------------------------------------------------------
say "2/9 Packages (installed only if missing)"
APT_UPDATED=0
apt_install() {
  if [ "$APT_UPDATED" = 0 ]; then
    sudo apt-get update -qq
    APT_UPDATED=1
  fi
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$@" >/dev/null
}
need_pkgs=()
command -v curl >/dev/null || need_pkgs+=(curl)
command -v openssl >/dev/null || need_pkgs+=(openssl)
command -v ss >/dev/null || need_pkgs+=(iproute2)
command -v iconv >/dev/null || need_pkgs+=(libc-bin)
dpkg -s ca-certificates >/dev/null 2>&1 || need_pkgs+=(ca-certificates)
if [ ${#need_pkgs[@]} -gt 0 ]; then
  apt_install "${need_pkgs[@]}"
  info "installed ${need_pkgs[*]}"
else
  info "curl, openssl, iproute2, ca-certificates present"
fi

codename=${UBUNTU_CODENAME:-$VERSION_CODENAME}
if command -v docker >/dev/null; then
  # Kept as it is (e.g. preinstalled on AWS Deep Learning AMIs).
  info "Docker present ($(docker --version 2>/dev/null | head -n1)), not reinstalled"
elif ! curl -fsSI --max-time 10 "https://download.docker.com/linux/ubuntu/dists/$codename/Release" >/dev/null 2>&1; then
  # Docker's repository has no release for this Ubuntu yet (a new LTS such
  # as 26.04 at first): Ubuntu's own packages instead.
  info "Docker's apt repository has no '$codename' release yet: using Ubuntu's docker.io and docker-compose-v2"
  apt_install docker.io docker-compose-v2
  info "Docker Engine (docker.io) and docker-compose-v2 installed"
else
  # Docker's own apt repository (docs.docker.com/engine/install/ubuntu).
  sudo install -m 0755 -d /etc/apt/keyrings
  if [ ! -s /etc/apt/keyrings/docker.asc ]; then
    sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
    sudo chmod a+r /etc/apt/keyrings/docker.asc
  fi
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $codename stable" \
    | sudo tee /etc/apt/sources.list.d/docker.list >/dev/null
  APT_UPDATED=0
  apt_install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
  info "Docker Engine and the compose plugin installed"
fi
if sudo docker compose version >/dev/null 2>&1; then
  info "docker compose $(sudo docker compose version --short)"
else
  # Docker from Ubuntu's own packages: its compose v2 package; Docker's
  # repository: the plugin.
  apt_install docker-compose-v2 2>/dev/null || apt_install docker-compose-plugin
  sudo docker compose version >/dev/null 2>&1 || die "docker compose (v2) could not be installed"
  info "docker compose installed"
fi
sudo systemctl enable --now docker >/dev/null 2>&1 || true

if command -v nginx >/dev/null; then
  info "nginx present"
else
  apt_install nginx
  info "nginx installed"
fi
sudo systemctl enable --now nginx >/dev/null 2>&1 || true

if command -v certbot >/dev/null && dpkg -s python3-certbot-nginx >/dev/null 2>&1; then
  info "certbot with the nginx plugin present"
else
  apt_install certbot python3-certbot-nginx
  info "certbot and python3-certbot-nginx installed"
fi

# ---------------------------------------------------------------------------
say "3/9 The docker group"
if id -nG "$OWNER" | tr ' ' '\n' | grep -qx docker; then
  info "$OWNER is in the docker group"
else
  sudo usermod -aG docker "$OWNER"
  info "$OWNER added to the docker group (takes effect at the next login; this run uses sg)"
fi
# This shell may predate the group change: run docker commands through sg
# until the next login.
if docker info >/dev/null 2>&1; then
  as_docker() { bash -c "$1"; }
else
  as_docker() { sg docker -c "$1"; }
  as_docker "docker info >/dev/null" || die "docker is not usable by $OWNER even through the docker group"
fi

# ---------------------------------------------------------------------------
say "4/9 The backend: setup-host.sh (secrets, NKeys, volumes, network mhai-bff)"
# setup-host.sh announces its own steps, keeps whatever exists and starts
# nothing. SECURITY_CONTACT_NAME and _EMAIL pass through from the environment
# (it asks for them at a terminal otherwise).
as_docker "MHAI_BACKEND_DIR=$(printf '%q' "$BACKEND_DEST") bash $(printf '%q' "$SRC/setup-host.sh")"

# ---------------------------------------------------------------------------
say "5/9 The clinical app's folder $WEB_DEST${PLATFORM_DOMAIN:+ and the platform app folder $PLATFORM_DEST}"
if [ -d "$WEB_DEST" ]; then
  info "exists, kept"
else
  sudo mkdir -p "$WEB_DEST"
  sudo chown "$OWNER:$OWNER" "$WEB_DEST"
  info "created, owner $OWNER"
fi
[ -w "$WEB_DEST" ] || die "$WEB_DEST is not writable by $OWNER"
copy() {
  local from=$1 to=$2
  if [ -f "$to" ] && cmp -s "$from" "$to"; then
    info "$to unchanged"
  else
    install -m 644 "$from" "$to"
    info "$to written"
  fi
}
copy "$WEB_SRC/compose.yml"                     "$WEB_DEST/compose.yml"
copy "$WEB_SRC/nginx/live-proxy.conf.template"  "$WEB_DEST/live-proxy.conf.template"
if [ "$DEMO" = 1 ]; then
  copy "$WEB_SRC/compose.demo-live.yml"         "$WEB_DEST/compose.demo-live.yml"
fi
# Demo feedback: writable by the container's user only.
if [ -d "$WEB_DEST/feedback" ]; then
  info "$WEB_DEST/feedback exists, kept"
else
  sudo mkdir -p "$WEB_DEST/feedback"
  sudo chown "$WEB_UID:$WEB_UID" "$WEB_DEST/feedback"
  sudo chmod 750 "$WEB_DEST/feedback"
  info "$WEB_DEST/feedback created (uid $WEB_UID, mode 750)"
fi
as_docker "cd $(printf '%q' "$WEB_DEST") && docker compose config --quiet"
info "compose.yml is valid"
if ss -Hltn "sport = :$WEB_PORT" | grep -q . \
  && ! as_docker "docker ps --filter name='^mhai-web-web' --format '{{.Ports}}'" | grep -q "127.0.0.1:$WEB_PORT->"; then
  die "port $WEB_PORT is in use by something other than the clinical app; free it first"
fi
info "127.0.0.1:$WEB_PORT is free for the clinical app (or already its own)"

if [ -n "$PLATFORM_DOMAIN" ]; then
  # The platform admin app (mhai-web deploy/README.md, "Platform admin app").
  if [ -d "$PLATFORM_DEST" ]; then
    info "$PLATFORM_DEST exists, kept"
  else
    sudo mkdir -p "$PLATFORM_DEST"
    sudo chown "$OWNER:$OWNER" "$PLATFORM_DEST"
    info "$PLATFORM_DEST created, owner $OWNER"
  fi
  [ -w "$PLATFORM_DEST" ] || die "$PLATFORM_DEST is not writable by $OWNER"
  copy "$WEB_SRC/compose.platform.yml"            "$PLATFORM_DEST/compose.platform.yml"
  copy "$WEB_SRC/nginx/live-proxy.conf.template"  "$PLATFORM_DEST/live-proxy.conf.template"
  # Its session secret, made once and kept (a new one only signs everyone
  # out). Never printed.
  if [ -f "$PLATFORM_DEST/.env" ] && grep -q '^PLATFORM_SESSION_SECRET=.' "$PLATFORM_DEST/.env"; then
    info "$PLATFORM_DEST/.env has PLATFORM_SESSION_SECRET, kept"
  else
    ( umask 077; touch "$PLATFORM_DEST/.env"
      sed -i '/^PLATFORM_SESSION_SECRET=/d' "$PLATFORM_DEST/.env"
      printf 'PLATFORM_SESSION_SECRET=%s\n' "$(openssl rand -base64 48 | tr -d '\n')" >> "$PLATFORM_DEST/.env" )
    info "PLATFORM_SESSION_SECRET generated in $PLATFORM_DEST/.env"
  fi
  chmod 600 "$PLATFORM_DEST/.env"
  if ss -Hltn "sport = :$PLATFORM_PORT" | grep -q . \
    && ! as_docker "docker ps --filter name='^mhai-web-platform-platform' --format '{{.Ports}}'" | grep -q "127.0.0.1:$PLATFORM_PORT->"; then
    die "port $PLATFORM_PORT is in use by something other than the platform app; free it first"
  fi
  info "127.0.0.1:$PLATFORM_PORT is free for the platform app (or already its own)"
fi

# ---------------------------------------------------------------------------
say "6/9 nginx site $SITE for $DOMAIN"
# Another site already serving this name would make certbot edit the wrong file.
other=$(sudo grep -rlE "server_name[^;]*[[:space:]]$(printf '%s' "$DOMAIN" | sed 's/\./\\./g')([[:space:];]|$)" /etc/nginx/sites-enabled/ /etc/nginx/conf.d/ 2>/dev/null \
  | while read -r f; do [ "$(readlink -f "$f")" = "$SITE" ] || echo "$f"; done || true)
[ -z "$other" ] || die "$DOMAIN is already served by $other; remove that site or use another domain"
if sudo test -f "$SITE"; then
  # Kept as it is: certbot added the :443 block to it on an earlier run.
  sudo grep -qE "server_name[[:space:]]+$(printf '%s' "$DOMAIN" | sed 's/\./\\./g');" "$SITE" \
    || die "$SITE exists for another domain; this script never overwrites a site (move it away to start over)"
  info "exists for $DOMAIN, kept"
else
  # The same proxy settings as mhai-web's deploy/nginx/mhai-demo.conf. The
  # app reads the client IP from X-Real-IP, so it is always overwritten here.
  sudo tee "$SITE" >/dev/null <<EOF
# Host nginx site for the MHAI clinical app, written by mhai-multiagent's
# deploy/new-server.sh (as mhai-web's deploy/nginx/mhai-demo.conf).
# certbot adds the :443 block.

server {
    listen 80;
    server_name $DOMAIN;
    server_tokens off;

    access_log /var/log/nginx/$SITE_NAME.access.log;
    error_log  /var/log/nginx/$SITE_NAME.error.log;

    location / {
        proxy_pass http://127.0.0.1:$WEB_PORT;
        proxy_http_version 1.1;
        proxy_set_header Host              \$host;
        proxy_set_header X-Real-IP         \$remote_addr;
        proxy_set_header X-Forwarded-For   \$remote_addr;
        proxy_set_header X-Forwarded-Proto \$scheme;
        proxy_set_header X-Forwarded-Host  \$host;
        proxy_read_timeout 60s;
    }

    add_header X-Robots-Tag "noindex, nofollow" always;
    add_header X-Content-Type-Options "nosniff" always;
}
EOF
  info "written"
fi
if [ -L "/etc/nginx/sites-enabled/$SITE_NAME" ]; then
  info "enabled"
else
  sudo ln -s "$SITE" "/etc/nginx/sites-enabled/$SITE_NAME"
  info "enabled (linked into sites-enabled)"
fi
# Test before reloading: a bad config must never take another site down.
sudo nginx -t 2>&1 | sed 's/^/    /' || die "nginx -t failed (above); nothing was reloaded"
sudo systemctl reload nginx
info "nginx reloaded"

if [ -n "$PLATFORM_DOMAIN" ]; then
  say "6/9 nginx site $PLATFORM_SITE for $PLATFORM_DOMAIN (IP allow list)"
  other=$(sudo grep -rlE "server_name[^;]*[[:space:]]$(printf '%s' "$PLATFORM_DOMAIN" | sed 's/\./\\./g')([[:space:];]|$)" /etc/nginx/sites-enabled/ /etc/nginx/conf.d/ 2>/dev/null \
    | while read -r f; do [ "$(readlink -f "$f")" = "$PLATFORM_SITE" ] || echo "$f"; done || true)
  [ -z "$other" ] || die "$PLATFORM_DOMAIN is already served by $other; remove that site or use another domain"
  # The allow list: kept by the product owner, never overwritten. Empty
  # means everyone gets 403 until addresses are added.
  if sudo test -f "$PLATFORM_ALLOW"; then
    info "$PLATFORM_ALLOW exists, kept ($(sudo grep -c '^[[:space:]]*allow ' "$PLATFORM_ALLOW" || true) allow lines)"
  else
    sudo install -m 644 /dev/null "$PLATFORM_ALLOW"
    info "$PLATFORM_ALLOW created EMPTY: everyone gets 403 until you add 'allow <ip>;' lines (copy the old server's file when moving)"
  fi
  if sudo test -f "$PLATFORM_SITE"; then
    sudo grep -qE "server_name[[:space:]]+$(printf '%s' "$PLATFORM_DOMAIN" | sed 's/\./\\./g');" "$PLATFORM_SITE" \
      || die "$PLATFORM_SITE exists for another domain; this script never overwrites a site (move it away to start over)"
    info "exists for $PLATFORM_DOMAIN, kept"
  else
    # mhai-web's :80 block (the ACME challenge path, everything else to
    # https) with this server's name. The :443 block comes after certbot.
    sed "s/server_name platforms\.measured-health\.org;/server_name $PLATFORM_DOMAIN;/" \
      "$WEB_SRC/nginx/mhai-platform.conf" | sudo tee "$PLATFORM_SITE" >/dev/null
    sudo grep -qE "server_name[[:space:]]+$(printf '%s' "$PLATFORM_DOMAIN" | sed 's/\./\\./g');" "$PLATFORM_SITE" \
      || die "could not set server_name in $PLATFORM_SITE (mhai-web's nginx/mhai-platform.conf changed?)"
    info "written (from mhai-web's deploy/nginx/mhai-platform.conf)"
  fi
  if [ -L "/etc/nginx/sites-enabled/$PLATFORM_SITE_NAME" ]; then
    info "enabled"
  else
    sudo ln -s "$PLATFORM_SITE" "/etc/nginx/sites-enabled/$PLATFORM_SITE_NAME"
    info "enabled (linked into sites-enabled)"
  fi
  sudo mkdir -p /var/www/html
  sudo nginx -t 2>&1 | sed 's/^/    /' || die "nginx -t failed (above); nothing was reloaded"
  sudo systemctl reload nginx
  info "nginx reloaded"
fi
if command -v ufw >/dev/null && sudo ufw status 2>/dev/null | grep -q '^Status: active'; then
  sudo ufw allow 'Nginx Full' >/dev/null
  info "ufw: ports 80 and 443 allowed"
fi

# ---------------------------------------------------------------------------
say "7/9 DNS: $DOMAIN${PLATFORM_DOMAIN:+ and $PLATFORM_DOMAIN} must point at this server"
public_ip=$(curl -fsS --max-time 5 https://checkip.amazonaws.com 2>/dev/null | tr -d '[:space:]' || true)
local_ips=$(hostname -I 2>/dev/null || true)
info "this server: public ${public_ip:-unknown}, local ${local_ips:-unknown}"
# Everything above is in place; when moving a live site, this is where the
# script stops until the A records are switched (NEW-SERVER.md).
points_here() {
  local name=$1 resolved match=0 ip
  resolved=$(getent ahostsv4 "$name" 2>/dev/null | awk '{print $1}' | sort -u | tr '\n' ' ' || true)
  info "$name resolves to: ${resolved:-nothing}"
  [ -n "$resolved" ] || die "$name does not resolve yet: add a DNS A record for it pointing at ${public_ip:-the public IP of this server}, wait for it to show, then run this again"
  for ip in $resolved; do
    if [ "$ip" = "$public_ip" ] || printf ' %s ' "$local_ips" | grep -q " $ip "; then match=1; fi
  done
  [ "$match" = 1 ] || die "$name points at $resolved, not at this server (${public_ip:-public IP unknown}); switch the A record, then run this again (steps 1-6 are kept)"
  info "$name points here"
}
points_here "$DOMAIN"
[ -z "$PLATFORM_DOMAIN" ] || points_here "$PLATFORM_DOMAIN"

# ---------------------------------------------------------------------------
say "8/9 TLS certificates (Let's Encrypt) for $DOMAIN${PLATFORM_DOMAIN:+ and $PLATFORM_DOMAIN}"
if sudo test -f "/etc/letsencrypt/live/$DOMAIN/fullchain.pem" && sudo grep -q 'listen 443' "$SITE"; then
  info "certificate and https site present, kept (certbot renews it on its own timer)"
else
  info "ports 80 and 443 must be open to the internet (security group / firewall)"
  sudo certbot --nginx -d "$DOMAIN" -m "$EMAIL" --agree-tos --redirect \
    --non-interactive --keep-until-expiring
  sudo nginx -t >/dev/null 2>&1 || die "nginx -t failed after certbot"
  sudo systemctl reload nginx
  info "certificate installed, http redirects to https"
fi

if [ -n "$PLATFORM_DOMAIN" ]; then
  # Webroot: Let's Encrypt's check comes from addresses not on the allow
  # list, and the :80 block serves only the challenge path.
  if sudo test -f "/etc/letsencrypt/live/$PLATFORM_DOMAIN/fullchain.pem"; then
    info "certificate for $PLATFORM_DOMAIN present, kept (certbot renews it on its own timer)"
  else
    sudo certbot certonly --webroot -w /var/www/html -d "$PLATFORM_DOMAIN" -m "$EMAIL" --agree-tos \
      --non-interactive --keep-until-expiring
    info "certificate for $PLATFORM_DOMAIN installed"
  fi
  if sudo grep -q 'listen 443' "$PLATFORM_SITE"; then
    info "$PLATFORM_SITE has its https block, kept"
  else
    # mhai-web deploy/README.md, "Platform admin app", step 5: the allow
    # list and deny all apply to everything; the app sees $host.
    sudo tee -a "$PLATFORM_SITE" >/dev/null <<EOF

# The app, over https only, behind the allow list ($PLATFORM_ALLOW, then
# deny all). Added by mhai-multiagent's deploy/new-server.sh.
server {
    listen 443 ssl;
    server_name $PLATFORM_DOMAIN;
    server_tokens off;

    ssl_certificate     /etc/letsencrypt/live/$PLATFORM_DOMAIN/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/$PLATFORM_DOMAIN/privkey.pem;

    access_log /var/log/nginx/$PLATFORM_SITE_NAME.access.log;
    error_log  /var/log/nginx/$PLATFORM_SITE_NAME.error.log;

    include $PLATFORM_ALLOW;
    deny all;

    location / {
        proxy_pass http://127.0.0.1:$PLATFORM_PORT;
        proxy_http_version 1.1;
        # The app compares a write request's Origin with Host: keep Host.
        proxy_set_header Host              \$host;
        proxy_set_header X-Real-IP         \$remote_addr;
        proxy_set_header X-Forwarded-For   \$remote_addr;
        proxy_set_header X-Forwarded-Proto \$scheme;
        proxy_set_header X-Forwarded-Host  \$host;
        proxy_read_timeout 60s;
    }

    add_header Strict-Transport-Security "max-age=31536000" always;
    add_header X-Robots-Tag "noindex, nofollow" always;
    add_header X-Content-Type-Options "nosniff" always;
}
EOF
    info "https block added to $PLATFORM_SITE"
  fi
  sudo nginx -t 2>&1 | sed 's/^/    /' || die "nginx -t failed (above) for $PLATFORM_SITE; nothing was reloaded"
  sudo systemctl reload nginx
  info "nginx reloaded"
fi

# ---------------------------------------------------------------------------
say "9/9 Done. What to set in GitHub"
fp=
for t in ecdsa rsa; do
  if [ -r "/etc/ssh/ssh_host_${t}_key.pub" ]; then
    fp=$(ssh-keygen -lf "/etc/ssh/ssh_host_${t}_key.pub" | awk '{print $2}')
    info "SSH host key fingerprint ($t): $fp"
    break
  fi
done
[ -n "$fp" ] || info "no ECDSA or RSA host key found in /etc/ssh: DEPLOY_HOST_FINGERPRINT needs one"
cat <<EOF

    In BOTH repositories, Measured-Health/mhai-multiagent and
    Measured-Health/mhai-web (Settings, Environments, preview):
      secret   DEPLOY_HOST              this server's public address (${public_ip:-see your cloud console})
      secret   DEPLOY_USER              $OWNER
      secret   DEPLOY_SSH_KEY           the private key you SSH in with
      secret   DEPLOY_HOST_FINGERPRINT  the fingerprint above (check it from your
                                        machine: ssh-keyscan -t ecdsa <address> | ssh-keygen -lf -)
    and the repository variable:
      variable DEPLOY_ENABLED           true

    The preview environment names ONE server: changing these secrets moves the
    deploys from the old server to this one (NEW-SERVER.md, "Moving the demo
    to a new server").

    Next: run "Deploy backend demo" in mhai-multiagent first (it seeds the
    demo hospital), then "Deploy demo" in mhai-web (Actions, Run workflow,
    branch main). Then open https://$DOMAIN.
EOF
if [ -n "$PLATFORM_DOMAIN" ]; then
  cat <<EOF

    The platform admin app: "Deploy demo" also starts it ($PLATFORM_DEST has
    its session secret). Only the addresses in $PLATFORM_ALLOW reach
    https://$PLATFORM_DOMAIN (everyone else gets 403); edit that file, then
    sudo nginx -t && sudo systemctl reload nginx.
EOF
fi
if [ "$DEMO" = 1 ]; then
  cat <<'EOF'

    Demo settings (optional repository variables; the defaults are the demo):
      mhai-multiagent  DEMO_SAMPLE_AI    on unless "off" (the sample AI answers)
      mhai-multiagent  DEMO_RESET        scheduled demo reset, on unless "off"
      mhai-multiagent  DEMO_RESET_HOURS  every 6 hours unless set (1-24)
      mhai-web         DEMO_MODE         live unless "mock" (sample data only)
EOF
else
  cat <<'EOF'

    Note: the Deploy workflows set up the public demo today (a synthetic demo
    hospital with published demo passwords). Not for real patient data.
EOF
fi
cat <<'EOF'

    Email (optional): see deploy/README.md, "Turn on email".
EOF
