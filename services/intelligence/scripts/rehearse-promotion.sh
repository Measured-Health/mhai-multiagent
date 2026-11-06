#!/usr/bin/env bash
# The model promotion flow on a rehearsal store (plan Task 24), with development keys and
# synthetic data only. Three commands:
#
#   rehearse-promotion.sh [run]
#       The whole flow, CI's model-promotion-e2e (and tests/modelstore/test_rehearsal.py):
#       test split -> fixture run -> dry run -> promote 0.1.0 -> pin -> lock-check ->
#       sign-off -> load in the pipeline's loader (model_versions) -> tamper a cached byte
#       -> promote, pin, lock-check and sign off 0.2.0 -> rollback to 0.1.0 -> lock-check
#       -> loads 0.1.0. Output: one line per stage, `<stage> <the command's JSON
#       summary>`; the store test asserts each one.
#   rehearse-promotion.sh store-up ENV_FILE ROLE...
#       Start docker-compose.models.yml's MinIO on loopback with a random root password,
#       provision it (its one-shot minio-init: deploy/minio/init-model-store.sh, the script
#       `mhai-models store-init` runs, in the pinned client image, so the runner needs no
#       mc), and append the store settings and the credentials of the ROLEs named
#       (promoter, signoff, reader, eval-reader, eval-writer) to ENV_FILE (GitHub's
#       KEY=VALUE env file): a job gets only the roles it uses. In GitHub Actions each
#       secret is masked first.
#   rehearse-promotion.sh keys DIR ENV_FILE
#       Development keys and a trust policy in DIR; MHAI_MODELS_SIGNER and
#       MHAI_MODELS_TRUST_POLICY appended to ENV_FILE (model-testset.yml's rehearsal).
#   rehearse-promotion.sh prepare DIR NAME RUN ENV_FILE
#       For model-promote.yml in rehearsal mode, after store-up: the keys as above, a
#       small locked test split and its lock file, and the fixture run NAME/RUN, with
#       their settings appended to ENV_FILE.
#
# `run` and `prepare` need a provisioned rehearsal store and mhai-models on PATH (run
# them with `uv run scripts/rehearse-promotion.sh ...` in services/intelligence):
#
#   MODEL_STORE_ENDPOINT          a loopback http(s) URL (the CLI refuses plain HTTP
#                                 anywhere else)
#   MODEL_STORE_BUCKET
#   MODEL_STORE_<ROLE>_ACCESS_KEY, MODEL_STORE_<ROLE>_SECRET_KEY  for PROMOTER, SIGNOFF,
#                                 READER, EVAL_READER and EVAL_WRITER (`prepare`: EVAL_WRITER)
#   REHEARSAL_NAME                `run`: the model name (default fixture-tiny)
#   REHEARSAL_TESTSET_VERSION     the test split's version (default 1.0.0)
#
# `run` makes everything else in a private temporary folder removed on exit: the
# development keys and trust policy, a throwaway git repository standing in for main and
# the PRs (so lock-check and the sign-off diff real commits, D35), and GitHub's answers in
# a file (MHAI_MODELS_GITHUB_FAKE, which the CLI refuses outside development). No real
# GitHub API, no real key, no outside credential. Every command is development only (it
# sets MHAI_MODELS_ENV and APP_ENV to development itself), and any failure stops it with
# a non-zero exit.
set -euo pipefail
umask 077

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
SERVICE="$(dirname "$HERE")"
REPO_ROOT="$(dirname "$(dirname "$SERVICE")")"
THRESHOLDS_SRC="$SERVICE/intelligence/doceval/thresholds"
THRESHOLDS_REL="services/intelligence/intelligence/doceval/thresholds"
WARMUP_PAGE="$REPO_ROOT/testdata/models/warmup/page.png"
ROLE="extract"
TESTSET_VERSION="${REHEARSAL_TESTSET_VERSION:-1.0.0}"
GATE="rehearsal"
SUITE="clinical_document"
CLINICAL="syn-clinical"
PLATFORM="syn-platform"
AUTHOR="syn-author"
REPOSITORY="syn-org/syn-rehearsal"

say() { printf '%s %s\n' "$1" "$2"; }

fail() {
  printf 'rehearse-promotion: %s\n' "$1" >&2
  exit 1
}

# One field of a JSON object on stdin (python from the same environment as mhai-models).
field() { python3 -I -c 'import json,sys; v=json.load(sys.stdin)[sys.argv[1]]; print(v if isinstance(v,str) else json.dumps(v))' "$1"; }

# mhai-models as one store role: only that role's credentials reach the command.
as_role() {
  local role="$1"; shift
  local a="MODEL_STORE_${role}_ACCESS_KEY" s="MODEL_STORE_${role}_SECRET_KEY"
  env -u MODEL_STORE_PROMOTER_ACCESS_KEY -u MODEL_STORE_PROMOTER_SECRET_KEY \
    -u MODEL_STORE_SIGNOFF_ACCESS_KEY -u MODEL_STORE_SIGNOFF_SECRET_KEY \
    -u MODEL_STORE_READER_ACCESS_KEY -u MODEL_STORE_READER_SECRET_KEY \
    -u MODEL_STORE_EVAL_READER_ACCESS_KEY -u MODEL_STORE_EVAL_READER_SECRET_KEY \
    -u MODEL_STORE_EVAL_WRITER_ACCESS_KEY -u MODEL_STORE_EVAL_WRITER_SECRET_KEY \
    "$a=${!a}" "$s=${!s}" "$@"
}

# make_keys DIR: development manifest and sign-off keys in DIR/manifest and DIR/signoff,
# and the trust policy that trusts them (the manifest key also signs test splits) written
# to DIR/trust-policy.json. Prints the two key ids as JSON.
make_keys() {
  local dir="$1" m s
  m="$(mhai-models dev-keygen "$dir/manifest")"
  s="$(mhai-models dev-keygen "$dir/signoff")"
  python3 -I - "$m" "$s" "$dir/trust-policy.json" <<'PY'
import json, sys
def entry(raw):
    k = json.loads(raw)
    return {"kind": "public_key", "alg": "ed25519", "key_id": k["key_id"],
            "public_key_pem": k["public_key_pem"], "development": True}
m, s = entry(sys.argv[1]), entry(sys.argv[2])
policy = {"schema": "mhai.models.trust/1",
          "signers": {"manifest": [m], "signoff": [s], "testset": [m]}}
with open(sys.argv[3], "w", encoding="ascii") as f:
    json.dump(policy, f, sort_keys=True, separators=(",", ":"))
print(json.dumps({"manifest": m["key_id"], "signoff": s["key_id"]}))
PY
}

# build_testset SIGNER OUT: build and lock a small test split as the eval writer and write
# its lock line to OUT. Exit 4 (locked, scratch copy left behind) is a warning only; its
# code is the last stderr line (Task 17 N1).
build_testset() {
  local signer="$1" out="$2" line code=0 err
  err="$(mktemp)"
  line="$(as_role EVAL_WRITER env MHAI_MODELS_SIGNER="$signer" \
    mhai-models testset-build --suite "$SUITE" --version "$TESTSET_VERSION" --scale 0.02 \
    2>"$err")" || code=$?
  if [ "$code" -eq 4 ] && tail -n 1 "$err" | grep -q '"scratch_not_removed"'; then
    printf 'rehearse-promotion: warning: scratch_not_removed\n' >&2
    code=0
  fi
  rm -f "$err"
  if [ "$code" -ne 0 ]; then
    printf '%s\n' "$line" >&2
    return "$code"
  fi
  printf '%s' "$line" | grep -Eqx "$SUITE@[0-9.]+#[0-9a-f]{64}" || return 1
  printf '%s\n' "$line" >"$out"
}

need_store() {
  : "${MODEL_STORE_ENDPOINT:?set MODEL_STORE_ENDPOINT}"
  : "${MODEL_STORE_BUCKET:?set MODEL_STORE_BUCKET}"
  local role part var
  for role in "$@"; do
    for part in ACCESS SECRET; do
      var="MODEL_STORE_${role}_${part}_KEY"
      [ -n "${!var:-}" ] || fail "set $var"
    done
  done
  command -v mhai-models >/dev/null || fail "mhai-models not on PATH"
}

# env_line FILE KEY VALUE: append KEY=VALUE (one line; no newline in the value).
env_line() {
  case "$3" in *$'\n'*) fail "newline in $2" ;; esac
  printf '%s=%s\n' "$2" "$3" >>"$1"
}

# ---------------------------------------------------------------- store-up

store_up() {
  [ "$#" -gt 0 ] || fail "store-up needs ENV_FILE and the roles"
  local envfile="$1" secrets root user role var
  shift
  [ -n "$envfile" ] && [ "$#" -gt 0 ] || fail "store-up needs ENV_FILE and the roles"
  for role in "$@"; do
    case "$role" in
      promoter | signoff | reader | eval-reader | eval-writer) ;;
      *) fail "unknown store role" ;;
    esac
  done
  command -v docker >/dev/null || fail "docker not found"
  secrets="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/model-store-secrets.XXXXXXXX")"
  chmod 700 "$secrets"
  user="rehearsal-root"
  root="$(python3 -I -c 'import secrets; print(secrets.token_hex(24))')"
  [ "${GITHUB_ACTIONS:-}" != true ] || printf '::add-mask::%s\n' "$root"
  (
    cd "$REPO_ROOT"
    export MINIO_ROOT_USER="$user" MINIO_ROOT_PASSWORD="$root"
    export MODEL_STORE_SECRETS_DIR="$secrets" MODEL_STORE_BUCKET=mhai-models
    export MODEL_STORE_SECRETS_UID="$(id -u)" MODEL_STORE_SECRETS_GID="$(id -g)"
    docker compose -f docker-compose.models.yml up -d --wait minio >&2
    docker compose -f docker-compose.models.yml run --rm minio-init >&2
  )
  env_line "$envfile" MODEL_STORE_ENDPOINT "http://127.0.0.1:9000"
  env_line "$envfile" MODEL_STORE_BUCKET mhai-models
  for role in "$@"; do
    var="$(printf '%s' "$role" | tr 'a-z-' 'A-Z_')"
    [ -s "$secrets/$role" ] || fail "no password for $role"
    [ "${GITHUB_ACTIONS:-}" != true ] || printf '::add-mask::%s\n' "$(cat "$secrets/$role")"
    env_line "$envfile" "MODEL_STORE_${var}_ACCESS_KEY" "$role"
    env_line "$envfile" "MODEL_STORE_${var}_SECRET_KEY" "$(cat "$secrets/$role")"
  done
  rm -rf "$secrets"
}

# ---------------------------------------------------------------- keys and prepare

keys() {
  local dir="$1" envfile="$2"
  [ -n "$dir" ] && [ -n "$envfile" ] || fail "keys needs DIR ENV_FILE"
  command -v mhai-models >/dev/null || fail "mhai-models not on PATH"
  mkdir -p -m 700 "$dir"
  dir="$(cd "$dir" && pwd -P)"
  make_keys "$dir" >/dev/null
  env_line "$envfile" MHAI_MODELS_SIGNER "$dir/manifest/dev-signing-key.pem"
  env_line "$envfile" MHAI_MODELS_TRUST_POLICY "$dir/trust-policy.json"
}

prepare() {
  local dir="$1" name="$2" run="$3" envfile="$4"
  [ -n "$dir" ] && [ -n "$name" ] && [ -n "$run" ] && [ -n "$envfile" ] ||
    fail "prepare needs DIR NAME RUN ENV_FILE"
  need_store EVAL_WRITER
  keys "$dir" "$envfile"
  dir="$(cd "$dir" && pwd -P)"
  export MHAI_MODELS_TRUST_POLICY="$dir/trust-policy.json"
  build_testset "$dir/manifest/dev-signing-key.pem" "$dir/testsets.lock"
  # The names are the workflow's inputs: make-fixture checks them before writing anything.
  mhai-models make-fixture "$dir/source/models/$name/$run" --variant good --seed 1 \
    --name "$name" >/dev/null
  env_line "$envfile" MHAI_MODELS_TESTSETS_LOCK "$dir/testsets.lock"
  env_line "$envfile" MHAI_MODELS_LOCAL_ROOT "$dir/source"
  env_line "$envfile" MHAI_EVAL_ROLE locked
}

# A rehearsal is development, both switches (the CLI's own rule), and never anything else.
export MHAI_MODELS_ENV=development APP_ENV=development
export MODEL_STORE_TESTSET_RETENTION_DAYS=1

case "${1:-run}" in
  store-up)
    shift
    store_up "$@"
    exit 0
    ;;
  keys)
    keys "${2:-}" "${3:-}"
    exit 0
    ;;
  prepare)
    prepare "${2:-}" "${3:-}" "${4:-}" "${5:-}"
    exit 0
    ;;
  run) ;;
  *) fail "usage: rehearse-promotion.sh [run | store-up ENV_FILE ROLE... | keys DIR ENV_FILE | prepare DIR NAME RUN ENV_FILE]" ;;
esac

# ---------------------------------------------------------------- run

need_store PROMOTER SIGNOFF READER EVAL_READER EVAL_WRITER
NAME="${REHEARSAL_NAME:-fixture-tiny}"
case "$NAME" in
  fixture-*) ;;
  *) fail "REHEARSAL_NAME must start with fixture-" ;;
esac
printf '%s' "$TESTSET_VERSION" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+' ||
  fail "bad REHEARSAL_TESTSET_VERSION"

# The store's retention for the rehearsal's objects: one day (two for sign-offs, so the
# extension is longer than the candidate lock).
export MODEL_STORE_CANDIDATE_RETENTION_DAYS=1 MODEL_STORE_APPROVED_RETENTION_DAYS=2
# A synthetic workflow run for the manifest (never the CI run's real actor or ref).
export GITHUB_RUN_ID=1 GITHUB_SHA=abababababababababababababababababababab GITHUB_ACTOR=syn-promoter
export GITHUB_WORKFLOW_REF="$REPOSITORY/.github/workflows/rehearsal.yml@refs/heads/main"
unset GITHUB_TOKEN GITHUB_REPOSITORY GITHUB_API_URL MHAI_MODELS_CACHE

T="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/mhai-rehearsal.XXXXXXXX")"
T="$(cd "$T" && pwd -P)"
cleanup() {
  # Verified copies are read-only: give write back before removing.
  chmod -R u+w "$T" 2>/dev/null || true
  rm -rf "$T"
}
trap cleanup EXIT
export MHAI_MODELS_SCRATCH="$T/scratch"
mkdir -m 700 "$MHAI_MODELS_SCRATCH" "$T/keys" "$T/cache" "$T/creds"
REPO="$T/repo"
POLICY="$REPO/models/trust-policy.json"
TESTSETS="$REPO/models/testsets.lock"
export MHAI_MODELS_TRUST_POLICY="$POLICY" MHAI_MODELS_TESTSETS_LOCK="$TESTSETS"
export MHAI_MODELS_GITHUB_FAKE="$T/github.json"

# stage NAME COMMAND...: run COMMAND, print `NAME <its output>`; a failure prints the same
# line (a JSON refusal) and stops the script with the command's exit code. (A plain
# `say x "$(cmd)"` would hide the failure.)
OUT=""
stage() {
  local name="$1" code=0
  shift
  OUT="$("$@")" || code=$?
  say "$name" "$OUT"
  [ "$code" -eq 0 ] || exit "$code"
}

# Run git with no user or system configuration.
git_() {
  env GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null HOME="$T" \
    GIT_AUTHOR_NAME=syn GIT_AUTHOR_EMAIL=syn@example.invalid \
    GIT_COMMITTER_NAME=syn GIT_COMMITTER_EMAIL=syn@example.invalid \
    git -C "$REPO" "$@"
}

# ---------------------------------------------------------------- keys and the test split

mkdir -p "$REPO/models" "$REPO/$THRESHOLDS_REL"
stage keys make_keys "$T/keys"
mv "$T/keys/trust-policy.json" "$POLICY"
build_testset "$T/keys/manifest/dev-signing-key.pem" "$TESTSETS"
say testset "$(python3 -I -c 'import json,sys; print(json.dumps({"lock_line": sys.argv[1].strip()}))' "$(cat "$TESTSETS")")"

# ---------------------------------------------------------------- main, as a git repository

printf '{"models":{},"schema":"mhai.models.lock/1"}\n' >"$REPO/models.lock"
printf '{"clinical":["%s"],"platform":["%s"]}\n' "$CLINICAL" "$PLATFORM" >"$REPO/models/reviewers.json"
cp "$THRESHOLDS_SRC"/*.json "$REPO/$THRESHOLDS_REL/"
git_ init -q -b main
git_ add -A
git_ commit -qm "main"

# ---------------------------------------------------------------- GitHub's answers

# write_github PR HEAD_SHA BODY [MERGE BASE]: the PR (open, or merged as MERGE onto BASE),
# its label, its two approvals on HEAD_SHA, and for a merge the commit's PR and parents.
write_github() {
  python3 -I - "$MHAI_MODELS_GITHUB_FAKE" "$REPOSITORY" "$AUTHOR" "$CLINICAL" "$PLATFORM" "$@" <<'PY'
import json, sys
out, repo, author, clinical, platform, pr, head, body = sys.argv[1:9]
merge, base = (sys.argv[9], sys.argv[10]) if len(sys.argv) > 10 else (None, None)
n = int(pr)
pull = {
    "number": n, "state": "closed" if merge else "open", "merged": bool(merge),
    "merge_commit_sha": merge, "merged_at": "2026-11-06T00:00:00Z" if merge else None,
    "user": {"login": author, "type": "User"}, "head": {"sha": head, "ref": "rehearsal"},
    "base": {"ref": "main", "sha": "0" * 40, "repo": {"id": 1}},
    "labels": [{"name": "clinical-review"}], "body": body,
}
reviews = [
    {"id": 1, "user": {"login": clinical, "type": "User"}, "state": "APPROVED", "commit_id": head},
    {"id": 2, "user": {"login": platform, "type": "User"}, "state": "APPROVED", "commit_id": head},
]
responses = {f"/pulls/{n}": pull, f"/pulls/{n}/reviews": reviews}
if merge:
    responses[f"/commits/{merge}/pulls"] = [{"number": n}]
    responses[f"/git/commits/{merge}"] = {"sha": merge, "parents": [{"sha": base}, {"sha": head}]}
with open(out, "w", encoding="utf-8") as f:
    json.dump({"repository": repo, "responses": responses}, f)
PY
}

# ---------------------------------------------------------------- one model version

LOCK_CHECK_ARGS=(--base-dir "$REPO" --testsets "$TESTSETS" --thresholds-dir "$REPO/$THRESHOLDS_REL" --rehearsal)

# lock_check PR HEAD_SHA STAGE: lock-check from main's checkout, the head lock from git.
lock_check() {
  local pr="$1" head="$2" stage="$3"
  git_ show "$head:models.lock" >"$T/head.lock"
  stage "$stage" as_role READER mhai-models lock-check "${LOCK_CHECK_ARGS[@]}" \
    --head "$T/head.lock" --head-sha "$head" --pr "$pr"
}

# release VERSION RUN VARIANT PR: fixture -> dry run -> promote -> pin -> lock-check ->
# merge -> sign-off, as the stages <stage>-<version>.
release() {
  local version="$1" run="$2" variant="$3" pr="$4"
  mhai-models make-fixture "$T/source/models/$NAME/$run" --variant "$variant" --seed 1 \
    --name "$NAME" >/dev/null
  local promote=(mhai-models promote "$NAME" "$version" --source local --run "$run" --gate "$GATE")
  local split=(env MHAI_MODELS_LOCAL_ROOT="$T/source" MHAI_EVAL_ROLE=locked)
  stage "dry-run-$version" as_role EVAL_READER "${split[@]}" "${promote[@]}" --dry-run
  local files
  files="$(printf '%s' "$OUT" | field files_sha256)"
  printf '%s' "$files" | grep -Eqx '[0-9a-f]{64}' || exit 1
  # The promotion as the promoter, with the eval reader for the locked split.
  stage "promote-$version" as_role PROMOTER env \
    MODEL_STORE_EVAL_READER_ACCESS_KEY="$MODEL_STORE_EVAL_READER_ACCESS_KEY" \
    MODEL_STORE_EVAL_READER_SECRET_KEY="$MODEL_STORE_EVAL_READER_SECRET_KEY" \
    MHAI_MODELS_SIGNER="$T/keys/manifest/dev-signing-key.pem" \
    "${split[@]}" "${promote[@]}" --expect-files "$files"

  git_ checkout -q -b "pr-$pr" main
  stage "pin-$version" as_role READER mhai-models pin "$ROLE" "$NAME" "$version" \
    --gate "$GATE" --lock "$REPO/models.lock" --rehearsal
  git_ commit -qam "pin $NAME $version"
  local head base merge
  head="$(git_ rev-parse HEAD)"
  git_ checkout -q main
  write_github "$pr" "$head" "Pins $NAME $version (rehearsal)."
  lock_check "$pr" "$head" "lock-check-$version"

  base="$(git_ rev-parse HEAD)"
  git_ merge -q --no-ff -m "merge pr-$pr" "pr-$pr"
  merge="$(git_ rev-parse HEAD)"
  write_github "$pr" "$head" "Pins $NAME $version (rehearsal)." "$merge" "$base"
  git_ show "$base:models.lock" >"$T/before.lock"
  git_ show "$merge:models.lock" >"$T/after.lock"
  git_ show "$head:models.lock" >"$T/head.lock"
  git_ checkout -q --detach "$base"  # --base-dir is a checkout of the base sha
  stage "signoff-$version" as_role SIGNOFF env \
    MHAI_MODELS_SIGNER="$T/keys/signoff/dev-signing-key.pem" \
    mhai-models signoff --base-dir "$REPO" --base-sha "$base" --before "$T/before.lock" \
    --after "$T/after.lock" --commit "$merge" --head-sha "$head" --head-lock "$T/head.lock" \
    --testsets "$TESTSETS" --thresholds-dir "$REPO/$THRESHOLDS_REL" --rehearsal
  git_ checkout -q main
}

# ---------------------------------------------------------------- the pipeline's loader

printf '%s' "$MODEL_STORE_READER_ACCESS_KEY" >"$T/creds/reader-access"
printf '%s' "$MODEL_STORE_READER_SECRET_KEY" >"$T/creds/reader-secret"
chmod 600 "$T/creds/reader-access" "$T/creds/reader-secret"

# load STAGE LOCK [ENDPOINT]: one start of the pipeline's model gate (the worker's own
# settings and build_model_gate), as a fresh process: ready or not, and model_versions.
load() {
  stage "$1" load_once "$2" "${3:-$MODEL_STORE_ENDPOINT}"
}

load_once() {
  local lockfile="$1" endpoint="$2"
  env -u MODEL_STORE_READER_ACCESS_KEY -u MODEL_STORE_READER_SECRET_KEY \
    INTELLIGENCE_SERVICE=pipeline NATS_URL=nats://127.0.0.1:4222 \
    INTELLIGENCE_TRUSTED_KEYS_PATH="$T/no-keys" INTELLIGENCE_MODEL_CLIENT=stub \
    INTELLIGENCE_MODEL_ROLES="$ROLE" INTELLIGENCE_MODELS_LOCK="$lockfile" \
    INTELLIGENCE_MODEL_CACHE="$T/cache" INTELLIGENCE_WARMUP_PAGE="$WARMUP_PAGE" \
    MODEL_STORE_ENDPOINT="$endpoint" \
    MODEL_STORE_READER_ACCESS_KEY_FILE="$T/creds/reader-access" \
    MODEL_STORE_READER_SECRET_KEY_FILE="$T/creds/reader-secret" \
    python3 -I - <<'PY'
import json, logging
from intelligence.boundary.config import WorkerSettings
from intelligence.boundary.worker import build_model_gate

logging.disable(logging.CRITICAL)
gate = build_model_gate(WorkerSettings.from_env())
assert gate is not None
ready = gate.attempt()
out = {
    "ready": ready,
    "model_versions": gate.models.versions() if gate.models is not None else {},
    "refused": sorted(f"{role}:{reason}" for role, reason in gate.failures),
}
print(json.dumps(out, sort_keys=True, separators=(",", ":")))
PY
}

# tamper: flip one byte of a cached weights file (its directory is read-only: give write
# back for the change, then take it away again). Prints the file's path.
tamper() {
  local f
  f="$(find "$T/cache" -type f -name '*.safetensors' | sort | head -n 1)"
  [ -n "$f" ] || exit 1
  chmod u+w "$(dirname "$f")" "$f"
  python3 -I - "$f" <<'PY'
import sys
with open(sys.argv[1], "r+b") as fh:
    fh.seek(-1, 2)
    last = fh.read(1)
    fh.seek(-1, 2)
    fh.write(bytes([last[0] ^ 0x01]))
PY
  chmod 0400 "$f"
  chmod 0500 "$(dirname "$f")"
}

# ---------------------------------------------------------------- the flow

release 0.1.0 run-1 good 1
load load-0.1.0 "$REPO/models.lock"

# A tampered cached byte: the next start re-checks the copy, refuses it and fetches the
# version again by its recorded ids (P11); the cache alone never makes the pipeline
# ready, so with the store out of reach a tampered copy leaves it not ready.
tamper
load tamper-store-down "$REPO/models.lock" "http://127.0.0.1:9"
load tamper-refetched "$REPO/models.lock"

release 0.2.0 run-2 good_v2 2
load load-0.2.0 "$REPO/models.lock"

# Roll back to the signed-off 0.1.0 by PR: rollback writes the lock, lock-check takes it
# as a rollback (one platform approval and a clinical note), and the pipeline loads 0.1.0.
git_ checkout -q -b pr-3 main
stage rollback as_role READER mhai-models rollback "$ROLE" --lock "$REPO/models.lock" --rehearsal
git_ commit -qam "roll back $NAME"
HEAD3="$(git_ rev-parse HEAD)"
git_ checkout -q main
write_github 3 "$HEAD3" "Clinical note: restoring the signed off synthetic version after a rehearsal."
lock_check 3 "$HEAD3" lock-check-rollback
git_ show "$HEAD3:models.lock" >"$T/rollback.lock"
load load-rollback "$T/rollback.lock"
