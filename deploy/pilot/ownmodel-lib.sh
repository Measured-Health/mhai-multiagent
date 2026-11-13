# shellcheck shell=bash
# The own answer model's pre-flight checks, shared by enable-ownmodel.sh and
# eval-local.sh (deploy/pilot/README.md, "Switch a hospital to your own model").
# Sourced, never run. Each check prints what it saw and dies with the fix; none
# reads or prints a secret. test-preflight.sh tests them without Docker.
#
# Settings (for the tests; the defaults are the host's):
#   MEMINFO                   /proc/meminfo
#   PILOT_PIPELINE_MIB        1600  the pipeline's cap (compose.yml), and the
#                                   eval container's (a run of the same service)
#   PILOT_HOST_RESERVE_MIB    2048  everything else on the host: postgres, core,
#                                   nats, the EMR, MongoDB and the web demo

say()  { printf '\n==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

PIPELINE_MIB=${PILOT_PIPELINE_MIB:-1600}
RESERVE_MIB=${PILOT_HOST_RESERVE_MIB:-2048}

# mem_mib VALUE: a compose memory size (4g, 3500m) in MiB, or nothing (status 1).
mem_mib() {
  [[ $1 =~ ^([1-9][0-9]{0,5})([mMgG])$ ]] || return 1
  case "${BASH_REMATCH[2]}" in
    m|M) printf '%s\n' "${BASH_REMATCH[1]}" ;;
    *) printf '%s\n' "$(( BASH_REMATCH[1] * 1024 ))" ;;
  esac
}

# meminfo_mib FIELD: MemTotal or MemAvailable, in MiB.
meminfo_mib() {
  local v
  v=$(awk -v f="$1:" '$1 == f {print int($2/1024)}' "${MEMINFO:-/proc/meminfo}")
  [[ $v =~ ^[0-9]+$ ]] || die "could not read $1 from ${MEMINFO:-/proc/meminfo}"
  printf '%s\n' "$v"
}

# check_memory MODEL_MEM PIPELINE_RUNNING LLAMA_RUNNING EVAL_MIB
#   The host can hold the model next to the pilot at all (MemTotal), and has the
#   memory free now for what is not running yet (MemAvailable). EVAL_MIB is the
#   eval container's (0 for enable-ownmodel.sh).
check_memory() {
  local model running_p=$2 running_l=$3 eval_mib=$4 total avail need_total need_now
  model=$(mem_mib "$1") || die "PILOT_LOCAL_MODEL_MEM must be a size such as 3g or 3500m (got a malformed value)"
  total=$(meminfo_mib MemTotal)
  avail=$(meminfo_mib MemAvailable)
  need_total=$(( model + PIPELINE_MIB + RESERVE_MIB ))
  info "host memory: ${total} MiB in all, ${avail} MiB available"
  info "the model's cap ${model} MiB + the pipeline ${PIPELINE_MIB} MiB + the rest of the host ${RESERVE_MIB} MiB = ${need_total} MiB"
  if [ "$total" -lt "$need_total" ]; then
    die "this host has ${total} MiB in all: a ${model} MiB model can't fit next to the pilot (${need_total} MiB needed; deploy/README.md, 'Sizing'). Resize the instance first (t4g.large, 8 GiB, fits a 3B model; README step 1). Nothing was changed."
  fi
  need_now=$eval_mib
  [ "$running_l" = 1 ] || need_now=$(( need_now + model ))
  [ "$running_p" = 1 ] || need_now=$(( need_now + PIPELINE_MIB ))
  if [ "$avail" -lt "$need_now" ]; then
    die "not enough free memory now (${avail} < ${need_now} MiB for what is not running yet): stop what else is running or resize. Nothing was changed."
  fi
  info "memory ok (${need_now} MiB needed now)"
}

# check_lock DIR: DIR/models.lock pins cca, and DIR/trust-policy.json and
# DIR/testsets.lock are there (regular files, never links). The pipeline still
# verifies everything it loads; this only stops a run that can't work.
check_lock() {
  local f
  for f in models.lock trust-policy.json testsets.lock; do
    if [ -L "$1/$f" ] || [ ! -f "$1/$f" ]; then
      die "$1/$f is missing (or a link): copy it from the repository at the deployed commit (enable-ownmodel.sh does)"
    fi
  done
  grep -q '"cca":{' "$1/models.lock" \
    || die "$1/models.lock pins no cca: pin a passing cca first (README, 'pin'), merge it, and copy the lock (enable-ownmodel.sh)"
  info "models.lock pins cca; trust-policy.json and testsets.lock present"
}

# has_locked_split DIR: testsets.lock names a locked clinical_answer split.
has_locked_split() { grep -Eq '^clinical_answer@' "$1/testsets.lock"; }

# llama_commit_ok VERSION_OUTPUT GATE_COMMIT: llama-server --version names the
# commit the answer gate pinned (the first 7 or more hex of it).
llama_commit_ok() {
  local c
  c=$(printf '%s\n' "$1" | sed -n 's/.*version: [0-9][0-9a-z.-]* (build [0-9]*, commit \([0-9a-f]\{7,40\}\)).*/\1/p' | head -n1)
  [ -n "$c" ] && [[ $2 == "$c"* ]]
}

# wait_cca_served: until the pipeline logs model.served role=cca (it verified
# the pin offline, wrote active.json, and the sidecar serves that alias). A
# sidecar still serving an older pin (after a re-pin or a rollback it keeps the
# active.json it started with) is restarted once, so it reads the new one; the
# pipeline retries on its own. SINCE (optional): only log lines from then on
# (after a restart, whose container keeps its earlier lines). Uses the caller's
# compose function.
wait_cca_served() {
  local restarted=0 logs since=()
  [ -z "${1:-}" ] || since=(--since "$1")
  for _ in $(seq 1 180); do
    logs=$(compose logs --no-log-prefix "${since[@]}" pipeline 2>/dev/null || true)
    if grep -q 'model.served role=cca' <<<"$logs"; then
      info "the pipeline verified the cca pin and the sidecar serves it"
      return 0
    fi
    if [ "$restarted" = 0 ] && grep -Eq 'model.verify_failed role=cca reason=no_runtime detail=(alias_mismatch|start_timeout)' <<<"$logs"; then
      info "the sidecar serves another alias (an older pin): restarting llama once"
      compose restart llama >/dev/null
      restarted=1
    fi
    sleep 5
  done
  compose logs --tail=60 pipeline llama
  die "the pipeline has not served the cca pin after 15 minutes (its log names fixed codes only)"
}
