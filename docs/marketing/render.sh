#!/usr/bin/env bash
# Render a marketing card HTML to a 2160x2160 PNG (1080 logical @2x) via headless Chrome.
# Usage: ./render.sh agent-model-card.html [agent-model-card.png]
set -euo pipefail
cd "$(dirname "$0")"
HTML="${1:?usage: render.sh <file.html> [out.png]}"
OUT="${2:-${HTML%.html}.png}"
CHROME="${CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
"$CHROME" --headless=new --disable-gpu --hide-scrollbars \
  --force-device-scale-factor=2 --window-size=1080,1080 \
  --run-all-compositor-stages-before-draw --virtual-time-budget=4000 \
  --screenshot="$OUT" "$HTML" >/dev/null 2>&1
echo "wrote $OUT ($(file -b "$OUT"))"
