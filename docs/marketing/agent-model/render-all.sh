#!/usr/bin/env bash
# Render every page-N.html in this folder to page-N.png (2160x2160).
set -euo pipefail
cd "$(dirname "$0")"
CHROME="${CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
for html in page-*.html; do
  out="${html%.html}.png"
  "$CHROME" --headless=new --disable-gpu --hide-scrollbars \
    --force-device-scale-factor=2 --window-size=1080,1080 \
    --run-all-compositor-stages-before-draw --virtual-time-budget=4000 \
    --screenshot="$out" "$html" >/dev/null 2>&1
  echo "wrote $out"
done
