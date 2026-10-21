#!/usr/bin/env bash
# Build and push the cloud controller.
#
#   deploy/deploy.sh root@203.0.113.10 civitone.example.com you@example.com   # first time
#   deploy/deploy.sh root@203.0.113.10                                        # updates
set -euo pipefail

TARGET="${1:?usage: deploy.sh user@host [HOSTNAME EMAIL]}"
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$HERE/.."

make -C "$ROOT" linux
ssh "$TARGET" 'mkdir -p /root/civitone-deploy'
scp -q "$ROOT/dist/civitone-linux" "$ROOT/controller/snapserver.conf" "$HERE/cloud-setup.sh" "$TARGET:/root/civitone-deploy/"

if [ $# -ge 3 ]; then
  ssh "$TARGET" "bash /root/civitone-deploy/cloud-setup.sh '$2' '$3'"
else
  ssh "$TARGET" 'install -m 0755 /root/civitone-deploy/civitone-linux /usr/local/bin/civitone && systemctl restart civitone && sleep 2 && systemctl is-active civitone'
fi
