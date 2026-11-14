# shellcheck shell=bash
# Shared by enable-split.sh (the app host) and enable-intelligence-host.sh
# (docs/deploy/remote-intelligence.md). Sourced, never run. Nothing here reads
# or prints a secret; test-split-lib.sh tests the checks without Docker.

say()  { printf '\n==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '    warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

# is_ipv4 ADDR: a dotted IPv4 address, each octet 0-255, no leading zeros.
is_ipv4() {
  [[ $1 =~ ^(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})$ ]] || return 1
  local o
  for o in "${BASH_REMATCH[@]:1}"; do [ "$o" -le 255 ] || return 1; done
}

# is_private_ipv4 ADDR: in 10/8, 172.16/12, 192.168/16 or 100.64/10 (decision
# D6: NATS and Postgres are never published on a public address), and not a
# network or broadcast-looking .0 / .255 host.
is_private_ipv4() {
  is_ipv4 "$1" || return 1
  local a b d
  IFS=. read -r a b _ d <<<"$1"
  [ "$d" -ne 0 ] && [ "$d" -ne 255 ] || return 1
  [ "$a" -eq 10 ] && return 0
  [ "$a" -eq 172 ] && [ "$b" -ge 16 ] && [ "$b" -le 31 ] && return 0
  [ "$a" -eq 192 ] && [ "$b" -eq 168 ] && return 0
  [ "$a" -eq 100 ] && [ "$b" -ge 64 ] && [ "$b" -le 127 ] && return 0
  return 1
}

# is_nkey_public KEY: a NATS user public key (U + 55 base32 characters).
is_nkey_public() { [[ $1 =~ ^U[A-Z2-7]{55}$ ]]; }

# cert_days_left CERT: whole days until the certificate expires (negative once
# it has).
cert_days_left() {
  local end
  end=$(openssl x509 -in "$1" -noout -enddate | sed 's/^notAfter=//')
  echo $(( ($(date -d "$end" +%s) - $(date +%s)) / 86400 ))
}

# report_expiry CERT LABEL: prints the expiry; warns under 30 days.
report_expiry() {
  local left
  left=$(cert_days_left "$1")
  info "$2: expires in $left days"
  [ "$left" -ge 30 ] || warn "$2 expires in $left days: renew it (docs/deploy/remote-intelligence.md, 'Renewal')"
}

# cert_has_ip CERT ADDR: the certificate names ADDR as an IP SAN.
cert_has_ip() {
  openssl x509 -in "$1" -noout -ext subjectAltName 2>/dev/null | tr -d ' ' | tr ',' '\n' \
    | grep -qx "IPAddress:$2"
}

# place_files SRC_DIR DEST_DIR UID KEYMODE [keepdir]: copy every file of SRC_DIR
# into DEST_DIR inside a throwaway container with no network, owned by UID, keys
# (*-key.pem, *.nk) at KEYMODE and the rest 0444; DEST_DIR itself becomes 0700
# owned by UID, unless keepdir (then it stays as it is). The
# files go in as a tar stream (written to a temp name, then renamed), so a key
# never reaches an argument or the output. Needs $HELPER_IMAGE.
place_files() {
  mkdir -p "$2"
  tar -C "$1" -cf - . | docker run --rm -i --network none --user 0 --entrypoint sh \
    -e U="$3" -e M="$4" -e K="${5:-}" -v "$2:/d" "$HELPER_IMAGE" -c '
      set -eu; umask 077
      t=$(mktemp -d /d/.new.XXXXXX); trap "rm -rf $t" EXIT
      tar -xf - -C "$t"
      for f in "$t"/*; do
        [ -f "$f" ] || continue
        n=$(basename "$f")
        case "$n" in *-key.pem|*.nk) chmod "$M" "$f" ;; *) chmod 444 "$f" ;; esac
        chown "$U:$U" "$f"; mv -f "$f" "/d/$n"
      done
      [ "$K" = keepdir ] || { chown "$U:$U" /d; chmod 700 /d; }'
}
