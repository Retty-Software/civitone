#!/usr/bin/env bash
# One-time setup of a Civitone cloud controller on Debian 13 (trixie) or 12
# (bookworm), the releases snapcast ships .debs for. Run as root:
#
#   bash cloud-setup.sh civitone.example.com you@example.com
#
# Expects civitone-linux and snapserver.conf next to this script (deploy.sh
# copies them). Safe to re-run: it updates in place and keeps secrets.
#
# What ends up listening:
#   443/80  nginx: console (basic auth), /node and /source (WebSocket)
#   1704    snapserver audio to the nodes
#   everything else (controller 8090, snapserver control 1705/1780) on
#   localhost only.
set -euo pipefail

HOST="${1:?usage: cloud-setup.sh HOSTNAME EMAIL}"
EMAIL="${2:?usage: cloud-setup.sh HOSTNAME EMAIL}"
HERE="$(cd "$(dirname "$0")" && pwd)"
SNAP_VERSION="0.35.0"

echo "==> packages"
apt-get update -q
apt-get install -yq nginx certbot python3-certbot-nginx ufw curl apache2-utils openssl

echo "==> snapserver $SNAP_VERSION"
if ! snapserver --version 2>/dev/null | grep -q "$SNAP_VERSION"; then
  . /etc/os-release
  case "$VERSION_CODENAME" in
    trixie|bookworm) ;;
    *) echo "snapcast $SNAP_VERSION has no .deb for '$VERSION_CODENAME'; use Debian 13"; exit 1 ;;
  esac
  curl -fsSL -o /tmp/snapserver.deb \
    "https://github.com/badaix/snapcast/releases/download/v$SNAP_VERSION/snapserver_${SNAP_VERSION}-1_amd64_${VERSION_CODENAME}.deb"
  apt-get install -yq /tmp/snapserver.deb
fi
# Cloud differences from the laptop config: the JSON-RPC/HTTP port stays on
# localhost, and a 2 s buffer absorbs internet jitter.
sed -e 's/^bind_to_address = 0\.0\.0\.0$/bind_to_address = 127.0.0.1/' \
    -e 's/^buffer = .*/buffer = 2000/' \
    "$HERE/snapserver.conf" > /etc/snapserver.conf
# The controller runs snapserver itself (so the console can change the
# speaker buffer, which needs a restart): the packaged unit stays off.
systemctl disable --now snapserver 2>/dev/null || true

echo "==> controller"
id civitone >/dev/null 2>&1 || useradd --system --home /var/lib/civitone --create-home --shell /usr/sbin/nologin civitone
install -d -o civitone -g civitone /var/lib/civitone
install -m 0755 "$HERE/civitone-linux" /usr/local/bin/civitone
if [ ! -f /etc/civitone.env ]; then
  echo "CIVITONE_SOURCE_TOKEN=$(openssl rand -hex 24)" > /etc/civitone.env
  chmod 0640 /etc/civitone.env
  chgrp civitone /etc/civitone.env
fi
cat > /etc/systemd/system/civitone.service <<'UNIT'
[Unit]
Description=Civitone controller
After=network-online.target
Wants=network-online.target

[Service]
User=civitone
EnvironmentFile=/etc/civitone.env
WorkingDirectory=/var/lib/civitone
ExecStart=/usr/local/bin/civitone --listen 127.0.0.1:8090 --db /var/lib/civitone/civitone.db --snapserver /usr/bin/snapserver --snapserver-config /etc/snapserver.conf --snapserver-runtime-config /var/lib/civitone/snapserver.run.conf --snap-buffer-ms 2000
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable civitone
systemctl restart civitone

echo "==> nginx"
# The console has no login of its own yet, so nginx asks for one. Nodes and
# sources don't send it: /source checks its token, /node gets device tokens
# with adoption, /firmware/ checks the node's device token.
if [ ! -f /etc/nginx/civitone.htpasswd ]; then
  CONSOLE_PW=$(openssl rand -base64 18)
  htpasswd -bc /etc/nginx/civitone.htpasswd operator "$CONSOLE_PW" 2>/dev/null
  echo "$CONSOLE_PW" > /root/civitone-console-password
  chmod 600 /root/civitone-console-password
fi
cat > /etc/nginx/sites-available/civitone <<NGINX
map \$http_upgrade \$connection_upgrade { default upgrade; '' close; }

server {
    listen 80;
    listen [::]:80;
    server_name $HOST;

    proxy_http_version 1.1;
    proxy_set_header Host \$host;
    proxy_set_header Upgrade \$http_upgrade;
    proxy_set_header Connection \$connection_upgrade;
    proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
    proxy_read_timeout 1h;
    proxy_send_timeout 1h;

    location = /node   { proxy_pass http://127.0.0.1:8090; }
    location = /source { proxy_pass http://127.0.0.1:8090; proxy_buffering off; }
    location ^~ /firmware/ { proxy_pass http://127.0.0.1:8090; }
    location / {
        client_max_body_size 4m;   # firmware uploads
        auth_basic "Civitone";
        auth_basic_user_file /etc/nginx/civitone.htpasswd;
        proxy_pass http://127.0.0.1:8090;
    }
}
NGINX
ln -sf /etc/nginx/sites-available/civitone /etc/nginx/sites-enabled/civitone
nginx -t
systemctl reload nginx

echo "==> firewall"
ufw allow OpenSSH >/dev/null
ufw allow 80/tcp >/dev/null
ufw allow 443/tcp >/dev/null
ufw allow 1704/tcp comment 'snapcast audio' >/dev/null
ufw --force enable >/dev/null
ufw status | sed 's/^/    /'

echo "==> TLS for $HOST"
certbot --nginx -d "$HOST" --non-interactive --agree-tos -m "$EMAIL" --redirect

cat <<DONE

Done.
  Console:       https://$HOST/   user: operator   password: in /root/civitone-console-password
  Nodes:         controller URL wss://$HOST/node, snapserver $HOST:1704
  Laptop source: civitone.exe source --url wss://$HOST/source --stream Music \\
                   --audio-device "CABLE Output" --token <CIVITONE_SOURCE_TOKEN from /etc/civitone.env>
DONE
