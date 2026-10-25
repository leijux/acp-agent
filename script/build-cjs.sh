#!/usr/bin/env bash
# 把 billion-context/kernel 的官方 ESM 产物重新打成 goja 可执行的 CJS(ES2017)
set -euo pipefail
KERNEL=${KERNEL:-/tmp/billion-context/kernel}
"$KERNEL/node_modules/.bin/esbuild" "$KERNEL/dist/index.js" \
  --bundle --format=cjs --platform=neutral --target=es2017 \
  --external:crypto --external:path --external:fs --external:module --external:node:* \
  --define:import.meta.url='"file:///kernel/index.js"' \
  --outfile=./internal/kernel/kernel.dist.cjs
