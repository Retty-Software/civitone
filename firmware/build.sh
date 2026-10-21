#!/usr/bin/env bash
# Build Civitone node firmware.
#
#   ./build.sh [board]          board defaults to amped-esp32-plus
#
# Boards named *-source are input boards: they build the source/ app (no
# snapclient) with configs/sdkconfig.<board> as its sdkconfig.defaults.
#
# Copies the pinned upstream snapclient (submodule) into build/src, applies
# patches/*.patch in order, adds components/ (Civitone code), drops in
# configs/sdkconfig.<board>, and builds in
# Espressif's ESP-IDF container. Output: build/<board>/*.bin plus flash.sh.
set -euo pipefail

BOARD="${1:-amped-esp32-plus}"
IDF_IMAGE="espressif/idf:v5.5.1"   # same as Sonocotta's CI
HERE="$(cd "$(dirname "$0")" && pwd)"
SRC="$HERE/build/src"
OUT="$HERE/build/$BOARD"

[ -f "$HERE/configs/sdkconfig.$BOARD" ] || { echo "no configs/sdkconfig.$BOARD"; exit 1; }

if [[ "$BOARD" == *-source ]]; then
  echo "==> preparing $SRC (source app)"
  rm -rf "$SRC"
  mkdir -p "$SRC" "$OUT"
  rsync -a "$HERE/source/" "$SRC/"
  rsync -a "$HERE/components/" "$SRC/civitone_components/"
  sed -i.bak 's#EXTRA_COMPONENT_DIRS ../components#EXTRA_COMPONENT_DIRS civitone_components#' "$SRC/CMakeLists.txt"
  cp "$HERE/configs/sdkconfig.$BOARD" "$SRC/sdkconfig.defaults"
  echo "==> building in $IDF_IMAGE"
  docker run --rm -v "$SRC:/project" -w /project "$IDF_IMAGE" \
    bash -c '. $IDF_PATH/export.sh >/dev/null && idf.py build'
  cp "$SRC/build/civitone-source.bin" \
     "$SRC/build/bootloader/bootloader.bin" \
     "$SRC/build/partition_table/partition-table.bin" "$OUT/"
  cat > "$OUT/flash.sh" <<'EOF'
#!/usr/bin/env bash
# ./flash.sh /dev/cu.usbserial-XXXX   (keeps Wi-Fi and adoption in NVS)
set -euo pipefail
cd "$(dirname "$0")"
esptool --port "$1" --baud 115200 write-flash \
  0x1000 bootloader.bin 0x8000 partition-table.bin 0x10000 civitone-source.bin
EOF
  chmod +x "$OUT/flash.sh"
  echo "==> done: $OUT"
  ls -la "$OUT"
  exit 0
fi
[ -f "$HERE/snapclient/CMakeLists.txt" ] || { echo "run: git submodule update --init --recursive"; exit 1; }

echo "==> preparing $SRC"
rm -rf "$SRC"
mkdir -p "$SRC" "$OUT"
rsync -a --exclude .git "$HERE/snapclient/" "$SRC/"
for p in "$HERE"/patches/*.patch; do
  [ -e "$p" ] || continue
  echo "    applying $(basename "$p")"
  patch -s -p1 -d "$SRC" < "$p"
done
# Civitone's own components (control channel, ...) sit beside upstream's.
rsync -a "$HERE/components/" "$SRC/components/"
# The image's own version (esp_app_desc, shown when it's uploaded to the
# console) is Civitone's, not upstream's git describe.
sed -n 's/^#define CIVI_FW_VERSION "\(.*\)"/\1/p' "$HERE/components/civitone_ctrl/civitone_ctrl.c" > "$SRC/version.txt"
cp "$HERE/configs/sdkconfig.$BOARD" "$SRC/sdkconfig"

echo "==> building in $IDF_IMAGE"
docker run --rm -v "$SRC:/project" -w /project "$IDF_IMAGE" \
  bash -c '. $IDF_PATH/export.sh >/dev/null && idf.py build'

cp "$SRC/build/snapclient.bin" \
   "$SRC/build/ota_data_initial.bin" \
   "$SRC/build/bootloader/bootloader.bin" \
   "$SRC/build/partition_table/partition-table.bin" "$OUT/"

cat > "$OUT/flash.sh" <<'EOF'
#!/usr/bin/env bash
# ./flash.sh /dev/cu.usbserial-XXXX   (keeps WiFi and server settings in NVS)
set -euo pipefail
cd "$(dirname "$0")"
esptool --port "$1" --baud 115200 write-flash \
  0x1000 bootloader.bin 0x8000 partition-table.bin \
  0x1d000 ota_data_initial.bin 0x20000 snapclient.bin
EOF
chmod +x "$OUT/flash.sh"
echo "==> done: $OUT"
ls -la "$OUT"
