#!/usr/bin/env bash
# Fail when the head OpenAPI file has a breaking change against the base file
# (ADR 0012). Usage: openapi-breaking.sh BASE HEAD
#   BASE  the spec on main (a file; if it does not exist the check passes)
#   HEAD  the spec in this change
# Installs a pinned oasdiff, verified by SHA-256, into a temp directory.
set -euo pipefail

OASDIFF_VERSION="1.34.1"
OASDIFF_SHA256="75775f65b0adebd26a8e013af51a799342dfdc6262d89efa9e3b7bdf89657d90" # oasdiff_1.34.1_linux_amd64.tar.gz

base=${1:?base spec}
head=${2:?head spec}

if [ ! -s "$base" ]; then
  echo "openapi-breaking: no spec on the base branch; nothing to compare"
  exit 0
fi
if cmp -s "$base" "$head"; then
  echo "openapi-breaking: spec unchanged"
  exit 0
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
tgz="oasdiff_${OASDIFF_VERSION}_linux_amd64.tar.gz"
curl -fsSL -o "$tmp/$tgz" "https://github.com/oasdiff/oasdiff/releases/download/v${OASDIFF_VERSION}/$tgz"
echo "${OASDIFF_SHA256}  $tmp/$tgz" | sha256sum -c -
tar -xzf "$tmp/$tgz" -C "$tmp" oasdiff

"$tmp/oasdiff" --version
# ERR level is a breaking change; warnings are listed but do not fail.
"$tmp/oasdiff" breaking "$base" "$head" --fail-on ERR
echo "openapi-breaking: no breaking changes"
