#!/usr/bin/env bash
# Switch on the operations baseline on the demo/pilot host (deploy/README.md,
# "Operations"): the nightly encrypted backup and the five-minute health
# check, as systemd timers. Opt-in: the deploy workflow never runs this, and
# nothing here changes the compose project. Run by the operator, on the
# host, from a copy of this repo's deploy/ folder, after putting the age
# PUBLIC key in /etc/mhai-ops/backup-recipients.txt:
#
#   sudo bash deploy/ops/install-ops.sh
#   sudo bash deploy/ops/install-ops.sh --uninstall   # timers off; backups and config kept
#
# Safe to run again: it replaces the scripts and units, keeps ops.env and
# every key, URL and backup. It never generates, reads out or prints a key.
set -euo pipefail
umask 022

LIB=/usr/local/lib/mhai-ops
ETC=/etc/mhai-ops
UNITS="mhai-backup.service mhai-backup.timer mhai-healthcheck.service mhai-healthcheck.timer"
SRC=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

say()  { printf '\n==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "run with sudo (the timers, /etc/mhai-ops and the backups are root's)"

if [ "${1:-}" = --uninstall ]; then
  systemctl disable --now mhai-backup.timer mhai-healthcheck.timer 2>/dev/null || true
  for u in $UNITS; do rm -f "/etc/systemd/system/$u"; done
  systemctl daemon-reload
  info "timers off and units removed; $ETC, the backups and $LIB are kept"
  exit 0
fi
[ $# -eq 0 ] || die "usage: install-ops.sh [--uninstall]"

say "Prerequisites"
for bin in docker age curl flock sha256sum systemctl; do
  command -v "$bin" >/dev/null || die "$bin is not installed (age: apt-get install age)"
done
docker compose version >/dev/null 2>&1 || die "docker compose (v2) is not available"
info "docker, age, curl present"

say "Settings in $ETC (root only)"
install -d -m 700 "$ETC"
if [ -f "$ETC/ops.env" ]; then
  info "ops.env exists, kept"
else
  install -m 600 "$SRC/ops.env.example" "$ETC/ops.env"
  info "ops.env written from ops.env.example (edit HOST_LABEL)"
fi
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$SRC/lib.sh"
export OPS_CONFIG="$ETC/ops.env"
ops_load_config

R=$BACKUP_RECIPIENTS_FILE
[ -s "$R" ] || die "put the age PUBLIC key (one line, age1...) in $R first; keep the private key off this host (deploy/README.md, 'Operations')"
if grep -qi 'AGE-SECRET-KEY' "$R"; then
  die "$R holds a PRIVATE key: delete it (shred -u), put only the public key there, and treat that key as exposed"
fi
grep -Eq '^(age1[0-9a-z]+|ssh-(ed25519|rsa) )' "$R" || die "$R has no age public key (age1...)"
chmod 644 "$R"
info "recipients: $(grep -cE '^(age1|ssh-)' "$R") public key(s)"
for f in "$ALERT_WEBHOOK_FILE" "$OFFSITE_ENV_FILE"; do
  if [ -f "$f" ]; then chown root:root "$f"; chmod 600 "$f"; info "$(basename "$f"): present, mode 600"; fi
done
[ -s "$ALERT_WEBHOOK_FILE" ] || info "no $ALERT_WEBHOOK_FILE: alerts are off until it exists"
[ -f "$OFFSITE_ENV_FILE" ] || info "no $OFFSITE_ENV_FILE: no offsite copy"
[ -f "$MHAI_BACKEND_DIR/compose.yml" ] || die "$MHAI_BACKEND_DIR is not set up (deploy/setup-host.sh)"

say "Scripts in $LIB"
install -d -m 755 "$LIB"
install -m 644 "$SRC/lib.sh" "$LIB/lib.sh"
for s in backup.sh restore-test.sh healthcheck.sh; do install -m 755 "$SRC/$s" "$LIB/$s"; done
install -d -m 700 "$BACKUP_DIR" "$STATE_DIR"
info "backup.sh, restore-test.sh, healthcheck.sh; backups in $BACKUP_DIR, status in $STATE_DIR"

say "Timers"
for u in $UNITS; do install -m 644 "$SRC/systemd/$u" "/etc/systemd/system/$u"; done
systemctl daemon-reload
systemctl enable --now mhai-backup.timer mhai-healthcheck.timer >/dev/null
systemctl list-timers --no-pager 'mhai-*' | sed 's/^/    /'

say "Done"
info "Now: sudo systemctl start mhai-backup.service   (the first backup; see journalctl -u mhai-backup)"
info "Then: the first restore test (deploy/README.md, 'Operations'), and weekly after that."
