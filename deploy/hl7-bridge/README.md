# MHAI HL7 bridge: install guide for hospital IT

The bridge is a small program you run on your network, next to your interface engine. Your engine sends HL7 v2 messages to it over MLLP, as it would to any HL7 receiver. The bridge saves each message to disk, acknowledges it, and keeps the messages in files that MHAI collects over SFTP. MHAI never opens a connection into your interface engine, and your engine never sends anything to the internet.

```
interface engine ──MLLP (LAN)──▶ HL7 bridge ◀──SFTP (port 22)── MHAI
```

## What it keeps, and for how long

- Only lab results and vital signs (ORU^R01) and pharmacy orders (RDE^O11). Every other message (admissions, transfers and so on) is acknowledged and thrown away, never saved.
- Files MHAI has collected are deleted after 7 days (`HL7_BRIDGE_RETAIN`). Files MHAI refused stay in `rejected/` until you or MHAI look at them; nothing else is ever deleted.
- The saved files contain patient information. **The bridge host's disk must be encrypted** (for example LUKS on Linux, or an encrypted volume from your hypervisor). Please confirm this to MHAI in writing before go-live.

## Host requirements

- A Linux host (x86-64 or arm64) with Docker Engine 24 or later and Docker Compose v2.
- 1 CPU, 1 GB of memory, 10 GB of encrypted disk.
- The host clock synchronised (NTP).
- Network: reachable from the interface engine on the LAN; port 22 reachable from MHAI's address only (see below), or a VPN to MHAI.

## Ports

| Port | From | What |
|---|---|---|
| 2575 (TCP), on the LAN address | your interface engine only | MLLP |
| 22 (TCP) | MHAI's egress address only, or the VPN | SFTP (the container listens on 2222) |
| 8080, on 127.0.0.1 | this host | health check |

MHAI connects to port 22 of a DNS name (not an IP address). Give the bridge a DNS name that resolves to the address MHAI reaches, and allow only MHAI's address through your firewall.

## Install

1. Copy `compose.yml` and `bridge.env.example` to a folder on the host, and copy `bridge.env.example` to `bridge.env`.
2. In `bridge.env`, set:
   - `HL7_BRIDGE_MLLP_ALLOW`: the interface engine's address (or addresses);
   - `HL7_BRIDGE_SFTP_ALLOW`: MHAI's egress address (MHAI gives it to you), or the VPN range;
   - `HL7_BRIDGE_SFTP_USER`: the user name MHAI gives you.
3. Save the public key MHAI gives you as `authorized_key` in the same folder (one line starting `ssh-ed25519` or `ecdsa-sha2-…`).
4. Start it, with the version MHAI gives you and the host's addresses:
   ```bash
   BRIDGE_VERSION=<version> BRIDGE_LAN_IP=<LAN address> BRIDGE_SFTP_IP=<address MHAI reaches> docker compose up -d
   ```
5. Read the host key fingerprint and send it to MHAI by a different channel from the one you use for the address (for example, read it out on a call):
   ```bash
   BRIDGE_VERSION=<version> BRIDGE_LAN_IP=<LAN address> BRIDGE_SFTP_IP=<address MHAI reaches> docker compose run --rm hl7-bridge fingerprint
   ```
   It looks like `SHA256:` followed by 43 characters. MHAI types it into the connection; MHAI's side then refuses any other server.
6. Check it is healthy: `curl -s http://127.0.0.1:8080/healthz` shows `"status":"ok"`.

Send MHAI: the DNS name, the host key fingerprint, and how often your engine sends results at the quietest time of day (MHAI uses it to notice when messages stop).

## Configure your interface engine

- A new outbound channel: **MLLP (TCP) client** to `<bridge LAN address>:2575`, with the standard framing (start `0x0B`, end `0x1C 0x0D`).
- Messages: ORU^R01 and RDE^O11, HL7 v2.3 to v2.8, ER7 encoding. Production messages only (MSH-11 = `P`).
- Acknowledgements: original mode. The bridge answers `AA` once the message is saved, `AE` when the message is malformed, `AR` when it is refused (unsupported version or character set, or over 1 MB). Configure the channel to **resend when no ACK arrives**, and to stop and alert on `AE` or `AR` (resending won't help).
- Character set (MSH-18): empty or `ASCII`, `UNICODE UTF-8`, or `8859/1`.
- Times: with a time-zone offset if you can (`20261127101500+0100`). If not, tell MHAI your time zone.
- The patient's MRN in PID-3 with its identifier type (PID-3.5, for example `MR`). Tell MHAI the type you use.
- Optional: TLS on MLLP. Put a certificate and key on the host, mount them into the container and set `HL7_BRIDGE_MLLP_TLS_CERT` and `HL7_BRIDGE_MLLP_TLS_KEY`; set `HL7_BRIDGE_MLLP_TLS_CLIENT_CA` as well to require the engine's client certificate.

## Health

`GET http://127.0.0.1:8080/healthz` returns counts and times only, never patient data:

- `ok`;
- `degraded`: files are waiting and MHAI hasn't collected any for an hour, or there are files in `rejected/`;
- `failing` (HTTP 503): the bridge can't write to its disk. It then stops acknowledging, so your engine keeps the messages and resends; fix the disk and restart the bridge.

The logs (`docker compose logs`) hold event names, counts and the addresses of refused connections, never message contents.

## Upgrades

1. Pause the engine's outbound channel (it queues messages).
2. `BRIDGE_VERSION=<new version> ... docker compose up -d` (the saved files and the host key stay in their volumes, so the fingerprint doesn't change).
3. Check `/healthz`, then resume the channel.

## Removing the bridge

Tell MHAI first (it switches the connection off). Then `docker compose down -v` deletes the containers and the saved files and host key.
