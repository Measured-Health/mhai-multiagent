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
# ADR 0012: adding a value to Problem.code is not breaking. oasdiff reports it
# as response-property-enum-value-added on the Problem schema's code property;
# drop exactly those findings (matched on the structured id and property path)
# and fail on anything else.
"$tmp/oasdiff" breaking "$base" "$head" --format json > "$tmp/findings.json"
python3 - "$tmp/findings.json" <<'PY'
import json, re, sys

ALLOWED_ID = "response-property-enum-value-added"
# text: added the new `X` enum value to the `oneOf[#/components/schemas/Problem]/code` response property ...
ALLOWED_TEXT = re.compile(
    r"^added the new `[^`]+` enum value to the `(?:oneOf\[)?#/components/schemas/Problem\]?/code` response property "
)
findings = json.load(open(sys.argv[1])) or []
kept, allowed = [], 0
for f in findings:
    if f.get("id") == ALLOWED_ID and ALLOWED_TEXT.match(f.get("text", "")):
        allowed += 1
    else:
        kept.append(f)
errors = [f for f in kept if f.get("level", 0) >= 3]
for f in kept:
    lvl = "error" if f.get("level", 0) >= 3 else "warning"
    print(f"{lvl}\t[{f.get('id')}] {f.get('operation', '')} {f.get('path', '')}: {f.get('text')}")
print(f"openapi-breaking: {allowed} new Problem.code value finding(s) allowed (ADR 0012)")
if errors:
    print(f"openapi-breaking: {len(errors)} breaking change(s)")
    sys.exit(1)
PY
echo "openapi-breaking: no breaking changes"
