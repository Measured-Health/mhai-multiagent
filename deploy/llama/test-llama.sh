#!/usr/bin/env bash
# The llama sidecar end to end, with Docker (own answer model plan, PR 3, Task 10).
#
# Builds deploy/llama (or uses IMAGE), serves a tiny public test GGUF (llama.cpp's own
# stories260K, pinned by commit and SHA-256: no clinical content), and checks that:
#   1. with a matching active.json the sidecar serves exactly the alias, and answers;
#   2. on its internal network it reaches nothing outside (no DNS, no 1.1.1.1);
#   3. after one flipped byte in the GGUF it refuses to start.
# Every container, network, volume and image it makes carries PREFIX (default
# llamatest-<random>) and is removed on exit. Run: bash deploy/llama/test-llama.sh
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
PREFIX=${PREFIX:-llamatest-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')}
case "$PREFIX" in *[!a-z0-9-]*|'') echo "PREFIX must be [a-z0-9-]" >&2; exit 2 ;; esac
PY_IMAGE=${PY_IMAGE:-python:3.12.15-slim}
GGUF_URL=https://huggingface.co/ggml-org/models/resolve/499bc8821c6b12b4e53c5bffcb21ec206f212d81/tinyllamas/stories260K.gguf
GGUF_SHA=270cba1bd5109f42d03350f60406024560464db173c0e387d91f0426d3bd256d
NAME=mhai-cca-synthetic
DIR=1.0.0-0123456789ab
ALIAS="$NAME@1.0.0#0123456789ab"

tmp=$(mktemp -d)
built=""
cleanup() {
  docker rm -f "$PREFIX-llama" "$PREFIX-llama2" >/dev/null 2>&1 || true
  docker network rm "$PREFIX-model" >/dev/null 2>&1 || true
  docker volume rm "$PREFIX-cache" >/dev/null 2>&1 || true
  if [ -n "$built" ]; then docker image rm "$built" >/dev/null 2>&1 || true; fi
  rm -rf "$tmp"
}
trap cleanup EXIT

if [ -z "${IMAGE:-}" ]; then
  IMAGE="$PREFIX:test"
  docker build -q -t "$IMAGE" "$here" >/dev/null
  built=$IMAGE
fi

curl -fsSL -o "$tmp/model.gguf" "$GGUF_URL"
[ "$(sha256sum "$tmp/model.gguf" | cut -d' ' -f1)" = "$GGUF_SHA" ] || { echo "test GGUF hash" >&2; exit 1; }
printf '{"gguf":"%s/%s/model-q4_k_m.gguf","sha256":"%s","version_string":"%s"}' \
  "$NAME" "$DIR" "$GGUF_SHA" "$ALIAS" >"$tmp/active.json"
chmod 0644 "$tmp/model.gguf" "$tmp/active.json"

# The cache as the worker leaves it: owned by 1000, files 0400, directories 0500.
docker volume create "$PREFIX-cache" >/dev/null
docker run --rm --network none --user 0 -v "$PREFIX-cache:/models" -v "$tmp:/in:ro" \
  --entrypoint sh "$IMAGE" -c "
    mkdir -p /models/$NAME/$DIR && cp /in/model.gguf /models/$NAME/$DIR/model-q4_k_m.gguf &&
    cp /in/active.json /models/active.json &&
    chown -R 1000:1000 /models && chmod 0700 /models && chmod 0400 /models/active.json \
      /models/$NAME/$DIR/model-q4_k_m.gguf && chmod 0500 /models/$NAME/$DIR /models/$NAME"
docker network create --internal "$PREFIX-model" >/dev/null

run_sidecar() { # name: the hardened sidecar, as deploy/compose.yml runs it
  docker run -d --name "$1" --network "$PREFIX-model" --network-alias llama \
    --read-only --cap-drop ALL --security-opt no-new-privileges:true --tmpfs /tmp \
    --user 1000:1000 --memory 1g -v "$PREFIX-cache:/models:ro" \
    -e LLAMA_THREADS=2 -e LLAMA_CTX=512 -e LLAMA_ACTIVE_TIMEOUT=30 "$IMAGE" >/dev/null
}

py() { # run python in a client container on the model network
  docker run --rm --network "$PREFIX-model" -e ALIAS="$ALIAS" "$PY_IMAGE" python -c "$1"
}

run_sidecar "$PREFIX-llama"
py '
import json, os, time, urllib.request
alias = os.environ["ALIAS"]
deadline = time.time() + 120
while True:
    try:
        data = json.load(urllib.request.urlopen("http://llama:8080/v1/models", timeout=5))["data"]
        break
    except Exception:
        if time.time() > deadline:
            raise SystemExit("the sidecar never served")
        time.sleep(1)
ids = [m["id"] for m in data]
assert ids == [alias], ids
body = json.dumps({"model": alias, "messages": [{"role": "user", "content": "Once upon a time"}],
                   "max_tokens": 16, "temperature": 0, "stream": False}).encode()
req = urllib.request.Request("http://llama:8080/v1/chat/completions", body,
                             {"Content-Type": "application/json"})
reply = json.load(urllib.request.urlopen(req, timeout=60))
assert isinstance(reply["choices"][0]["message"]["content"], str)
print("ok   the sidecar serves exactly the verified alias, and answers")
'

# Nothing outside the internal network, from inside the sidecar's own network namespace.
docker run --rm --network "container:$PREFIX-llama" "$PY_IMAGE" python -c '
import socket
for host, port in (("1.1.1.1", 443), ("8.8.8.8", 53)):
    s = socket.socket(); s.settimeout(3)
    try:
        s.connect((host, port))
    except OSError:
        pass
    else:
        raise SystemExit(f"reached {host}:{port}")
try:
    socket.getaddrinfo("huggingface.co", 443)
except OSError:
    pass
else:
    raise SystemExit("resolved a public name")
print("ok   the sidecar has no route and no DNS outside its network")
'
docker rm -f "$PREFIX-llama" >/dev/null

# One flipped byte in the GGUF: the sidecar refuses to start.
docker run --rm --network none --user 0 -v "$PREFIX-cache:/models" --entrypoint sh "$IMAGE" -c "
  f=/models/$NAME/$DIR/model-q4_k_m.gguf; chmod u+w /models/$NAME/$DIR \$f
  printf 'X' | dd of=\$f bs=1 seek=100 conv=notrunc status=none; chmod 0400 \$f"
run_sidecar "$PREFIX-llama2"
code=$(docker wait "$PREFIX-llama2")
logs=$(docker logs "$PREFIX-llama2" 2>&1)
if [ "$code" = 0 ] || ! grep -q "not the verified one" <<<"$logs"; then
  echo "FAIL the sidecar started on a changed GGUF (exit $code)" >&2
  exit 1
fi
echo "ok   a flipped byte stops the sidecar from starting"
