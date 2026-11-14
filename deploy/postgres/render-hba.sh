#!/usr/bin/env bash
# Render pg_hba.split.conf for one intelligence host (docs/deploy/remote-intelligence.md):
#
#   bash deploy/postgres/render-hba.sh INTELLIGENCE_ADDR SUBNET [SUBNET...] > pg_hba.conf
#
# INTELLIGENCE_ADDR is one private IPv4 address; each SUBNET an IPv4 CIDR (the
# app host's Docker networks Postgres is on: backend and edge). Anything else
# is refused, so nothing but an address can reach the file.
set -euo pipefail
die() { printf 'render-hba: %s\n' "$*" >&2; exit 1; }
[ $# -ge 2 ] || { echo "usage: bash render-hba.sh INTELLIGENCE_ADDR SUBNET [SUBNET...]" >&2; exit 2; }
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
octets() { # octets ADDR: valid dotted IPv4
  [[ $1 =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$ ]] || return 1
  local o; for o in "${BASH_REMATCH[@]:1}"; do [ "$((10#$o))" -le 255 ] || return 1; done
}
addr=$1; shift
octets "$addr" || die "not an IPv4 address"
for s in "$@"; do
  if ! [[ $s =~ ^([0-9.]+)/([0-9]{1,2})$ ]] || ! octets "${BASH_REMATCH[1]}" || [ "${BASH_REMATCH[2]}" -gt 32 ]; then
    die "not an IPv4 CIDR"
  fi
done
while IFS= read -r line; do
  case "$line" in
    *@DOCKER_SUBNET@*) for s in "$@"; do printf '%s\n' "${line//@DOCKER_SUBNET@/$s}"; done ;;
    *) printf '%s\n' "${line//@INTELLIGENCE_ADDR@/$addr}" ;;
  esac
done < "$here/pg_hba.split.conf"
