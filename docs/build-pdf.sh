#!/bin/sh
set -eu

doc_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
pandoc "$doc_dir/overview.cs.md" \
  --resource-path="$doc_dir" \
  --lua-filter="$doc_dir/pdf-pagebreak.lua" \
  --pdf-engine=typst \
  --metadata=lang:cs \
  --variable=papersize:a4 \
  --variable=fontsize:9pt \
  --variable=page-numbering:1 \
  --output="$doc_dir/overview.cs.pdf"
