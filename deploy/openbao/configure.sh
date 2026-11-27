#!/bin/sh
# Runs INSIDE the openbao container (busybox sh), on the operator's
# loopback listener, from deploy/openbao/enable-openbao.sh (and from the
# integration test of the emr service's OpenBao backend). Never on the host.
#
#   configure.sh setup                 the KV v2 mount mhai/, the policies
#                                      emr-<env> and backup, the AppRoles
#                                      emr-<env> and backup. Idempotent.
#   configure.sh emr-credentials       a new secret_id for emr-<env>: prints
#                                      one JSON line {role_id, secret_id,
#                                      secret_id_accessor} for the caller
#                                      to write straight to a 0400 file.
#   configure.sh backup-credentials    the same for the backup role.
#   configure.sh prune ROLE ACCESSOR   destroys every secret_id of ROLE but
#                                      ACCESSOR (after a rotation).
#   configure.sh check                 the mount, policies and roles exist.
#
# The token: BAO_TOKEN, or else the first line of stdin (the caller pipes
# it; it never reaches an argument). MHAI_ENV: development, staging or
# production. Prints no secret except in the *-credentials output, which
# the caller never shows.
set -eu
umask 077
export BAO_ADDR=http://127.0.0.1:8201
case "${MHAI_ENV:-}" in development|staging|production) ;; *) echo "configure: MHAI_ENV must be development, staging or production" >&2; exit 2 ;; esac
if [ -z "${BAO_TOKEN:-}" ]; then
  IFS= read -r BAO_TOKEN || true
  [ -n "$BAO_TOKEN" ] || { echo "configure: no token on stdin" >&2; exit 2; }
fi
export BAO_TOKEN
ROLE="emr-$MHAI_ENV"
CONF=${OPENBAO_CONF_DIR:-/openbao/config}

has_line() { grep -x -- "$1" >/dev/null; }

case "${1:-}" in
  setup)
    if bao secrets list | awk '{print $1}' | has_line "mhai/"; then echo "configure: mhai/ (KV v2) present" >&2
    else bao secrets enable -path=mhai -version=2 -description="MHAI EMR connection secrets" kv >/dev/null; echo "configure: mhai/ (KV v2) enabled" >&2; fi
    # Up to 10 versions per field; the service itself drops replaced ones
    # after 24 h (secrets.Keep) and a rolled-back put at once.
    bao write mhai/config max_versions=10 cas_required=false >/dev/null
    sed "s/@ENV@/$MHAI_ENV/g" "$CONF/emr-policy.hcl" | bao policy write "$ROLE" - >/dev/null
    bao policy write backup "$CONF/backup-policy.hcl" >/dev/null
    echo "configure: policies $ROLE and backup written" >&2
    if bao auth list | awk '{print $1}' | has_line "approle/"; then :; else bao auth enable approle >/dev/null; fi
    # Short tokens, the service logs in again as they near their end; no
    # default policy; a secret_id with no TTL and no use limit (rotated by
    # enable-openbao.sh --rotate-credentials).
    bao write "auth/approle/role/$ROLE" token_policies="$ROLE" token_no_default_policy=true \
      token_ttl=15m token_max_ttl=30m token_type=service secret_id_ttl=0 secret_id_num_uses=0 bind_secret_id=true >/dev/null
    bao write auth/approle/role/backup token_policies=backup token_no_default_policy=true \
      token_ttl=5m token_max_ttl=10m token_type=service secret_id_ttl=0 secret_id_num_uses=0 bind_secret_id=true >/dev/null
    echo "configure: AppRoles $ROLE and backup written" >&2
    ;;
  emr-credentials|backup-credentials)
    r=$ROLE; [ "$1" = emr-credentials ] || r=backup
    role_id=$(bao read -field=role_id "auth/approle/role/$r/role-id")
    out=$(bao write -format=json -f "auth/approle/role/$r/secret-id")
    sid=$(printf '%s' "$out" | sed -n 's/^ *"secret_id": *"\([^"]*\)".*/\1/p')
    acc=$(printf '%s' "$out" | sed -n 's/^ *"secret_id_accessor": *"\([^"]*\)".*/\1/p')
    unset out
    [ -n "$role_id" ] && [ -n "$sid" ] && [ -n "$acc" ] || { echo "configure: no secret_id made" >&2; exit 1; }
    printf '{"role_id":"%s","secret_id":"%s","secret_id_accessor":"%s"}\n' "$role_id" "$sid" "$acc"
    ;;
  prune)
    r=${2:?role}; keep=${3:?accessor}
    case "$r" in "$ROLE"|backup) ;; *) echo "configure: unknown role" >&2; exit 2 ;; esac
    n=0
    for a in $(bao list -format=json "auth/approle/role/$r/secret-id" 2>/dev/null | sed -n 's/^ *"\([0-9a-f-]\{36\}\)",\{0,1\}$/\1/p'); do
      [ "$a" = "$keep" ] && continue
      bao write "auth/approle/role/$r/secret-id-accessor/destroy" secret_id_accessor="$a" >/dev/null
      n=$((n + 1))
    done
    echo "configure: $n older secret_id(s) of $r destroyed" >&2
    ;;
  check)
    bao secrets list | awk '{print $1}' | has_line "mhai/" || { echo "configure: no mhai/ mount" >&2; exit 1; }
    bao policy read "$ROLE" >/dev/null && bao policy read backup >/dev/null || { echo "configure: a policy is missing" >&2; exit 1; }
    bao read "auth/approle/role/$ROLE" >/dev/null || { echo "configure: no $ROLE role" >&2; exit 1; }
    echo "configure: mount, policies and roles present" >&2
    ;;
  *) echo "usage: configure.sh setup | emr-credentials | backup-credentials | prune ROLE ACCESSOR | check" >&2; exit 2 ;;
esac
