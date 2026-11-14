#!/usr/bin/env bash
# Checks the emr image's file modes and owners (the image has no shell, so
# this reads its file list): /emr-secrets is a 0700 folder of the nonroot
# user (65532), as the file secret store requires; the other state folders
# belong to 65532 too; and nothing else at / changed owner: every other
# top-level entry is root's. Run on a Docker host (the build server):
#
#   bash services/emr/scripts/image-check.sh [image]
#
# Without an image it builds one under a throwaway tag and removes it.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
svc=$(cd "$here/../.." && pwd)
image=${1:-}
built=""
if [ -z "$image" ]; then
  image="emr-image-check:$RANDOM$RANDOM"
  docker build -q -t "$image" -f "$svc/emr/Dockerfile" "$svc" >/dev/null
  built=$image
fi
cid=$(docker create "$image")
cleanup() {
  docker rm -f "$cid" >/dev/null 2>&1 || true
  if [ -n "$built" ]; then docker rmi -f "$built" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT

# "<mode> <uid> <name>" for every top-level entry of the image's files.
list=$(docker export "$cid" | tar -tv --numeric-owner 2>/dev/null |
  awk '{split($2, o, "/"); n = $NF; sub(/\/$/, "", n); if (n != "" && n !~ /\//) print $1, o[1], n}')

fail=0
mode=$(awk '$3 == "emr-secrets" {print $1, $2}' <<<"$list")
if [ "$mode" != "drwx------ 65532" ]; then
  echo "FAIL /emr-secrets: want drwx------ 65532, got ${mode:-missing}"
  fail=1
fi
for d in emr-keys emr-proxy-key emr-egress; do
  owner=$(awk -v n="$d" '$3 == n {print $2}' <<<"$list")
  if [ "$owner" != 65532 ]; then
    echo "FAIL /$d: want owner 65532, got ${owner:-missing}"
    fail=1
  fi
done
# Everything else at the top is root's, except distroless's own home.
while read -r _ uid name; do
  case "$name" in emr-secrets | emr-keys | emr-proxy-key | emr-egress | home) continue ;; esac
  if [ "$uid" != 0 ]; then
    echo "FAIL /$name: owner $uid, want 0"
    fail=1
  fi
done <<<"$list"
if [ "$fail" = 0 ]; then
  echo "PASS emr image: /emr-secrets 0700 owned by 65532, the state folders 65532's, the rest root's"
fi
exit "$fail"
