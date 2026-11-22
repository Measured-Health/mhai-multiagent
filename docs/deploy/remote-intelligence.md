# Running intelligence on its own server

- **Status:** Accepted (decisions delegated by the product owner, 14 Nov 2026)
- **Scope:** the pipeline worker, the llama sidecar, `model-fetch` and the egress
  proxy on an **intelligence host**; core, Postgres, NATS and the web apps stay on
  the **app host**. Both are in one private network (a VPC or a LAN).
- **Closes:** TODO.md "NATS TLS for production" and "Postgres TLS" for the
  worker's links, and tracker B-67 ("Internal TLS for NATS and Postgres").
- **Runbook:** [Operator steps](#operator-steps) below. Files:
  `deploy/tls/`, `deploy/compose.split.yml`, `deploy/nats/tls.conf`,
  `deploy/postgres/pg_hba.split.conf`, `deploy/enable-split.sh`,
  `deploy/intelligence-host/`.

## Why it works

ADR 0010 made NATS the only link between core and the Python side. The worker
holds no session, no user and no permission rule. It needs only three things,
and none of them depends on being on core's machine:

1. **NATS.** It takes requests and jobs on its own subjects as its own NKey
   user (`pipeline`), and asks core for grants over NATS (ADR 0011).
2. **Request tokens.** It verifies every request with core's public
   request-token keys (`request-keys.json`): signature, audience, expiry,
   replay and hospital. This needs a file with public keys only, not a call to
   core.
3. **Postgres.** It uses its own login, `intelligence_app`, a member of
   `mhai_worker_app` only. Every query runs under row-level security for the
   hospital in the verified token (`with_hospital`).

So moving the worker to another machine changes only the transport. What it
can do and what it can see stay the same. The transport must then be
encrypted and authenticated in both directions. Until now it was not, because
everything stayed on one host's Docker networks.

## What single-host allows and refuses

`INTELLIGENCE_SINGLE_HOST=on` is the pilot's exception on the demo host
(`deploy/pilot/README.md`). This design does not change it.

| | Allowed with `INTELLIGENCE_SINGLE_HOST=on` | Still refused |
|---|---|---|
| NATS | `nats://<service>` without TLS, where the service name is a single DNS label (`nats`) | a dot, an IP address, `localhost`, `tls://`, `ws://`, a password in the URL, no NKey |
| Postgres | `sslmode=disable`, or `verify-full`, to a single-label service name (`postgres`) | `require`, `verify-ca`, `prefer`, an empty mode, a dot or an address, a password in the URL |
| Everything else | unchanged | a value other than exactly `on`; the setting with `APP_ENV=development` (development has its own rules) |

The worker logs a warning at every start with the switch on. The switch can
never point at another machine: a remote host has a dot or is an address,
and both are refused. **On the intelligence host the switch is off.** The
worker then needs NATS TLS with a client certificate, an NKey, and Postgres
`sslmode=verify-full` with `sslrootcert` (decision D12). Without them it
refuses to start.

## Target layout

```
                 private network (VPC / LAN): only these two hosts
  ┌────────────── app host (small) ───────────────┐      ┌──────── intelligence host (GPU optional) ────────┐
  │                                                │      │                                                    │
  │ web (BFFs) ─mhai-bff─┐                         │      │  pipeline ──backend (internal)──┬─ app-link ────────┼──┐
  │                      ├─> nats :4222 (mTLS) <───┼──────┼──────────── TLS, end to end ────┘   (TCP relay,    │  │
  │ core ───backend──────┤                         │      │                                      uplink only)  │  │
  │        (internal)    └─> postgres :5432 <──────┼──────┼──── TLS verify-full, intelligence_app only ─────────┼──┘
  │                          (hostssl from the     │      │  pipeline ──model (internal)── llama (GPU optional)│
  │                           intelligence host)   │      │  pipeline ──backend── egress-proxy ──> one API host │
  │ published on the PRIVATE address only:         │      │  model-fetch ──model-store──> model store (HTTPS)  │
  │   <app-addr>:4222, <app-addr>:5432             │      │  no published port                                 │
  └────────────────────────────────────────────────┘      └────────────────────────────────────────────────────┘
```

- **App host**: core, Postgres, NATS and the web apps, as `deploy/compose.yml`
  runs them today, plus the override `deploy/compose.split.yml`. NATS and
  Postgres are published on the host's **private** address (and NATS on
  127.0.0.1 as before). Nothing is published on a public address.
  `enable-split.sh` refuses an address outside 10/8, 172.16/12, 192.168/16
  or 100.64/10.
- **Intelligence host** (`deploy/intelligence-host/compose.yml`, project
  `mhai-intelligence`): `pipeline`, `app-link`, and optionally `egress-proxy`
  (profile `cloud`), `llama` and `model-fetch` (profile `ownmodel`). The
  worker still has **no route out**. It is on internal networks only:
  `backend` (with `app-link` and `egress-proxy`) and `model` (with `llama`).
  `app-link` is a TCP relay (HAProxy). It is the only container on the
  `uplink` network, and it forwards exactly two ports to exactly one address:
  `<app-addr>:4222` and `<app-addr>:5432`. TLS runs from the worker to NATS
  and to Postgres *through* the relay, so the relay sees only ciphertext.
  The worker reaches the app host by the name `mhai-app` (a network alias of
  the relay), and the app host's certificates carry that name.
- **GPU** (optional): `deploy/intelligence-host/compose.gpu.yml` adds an
  NVIDIA device to `llama` and offloads its layers (`LLAMA_GPU_LAYERS`). The
  image is the same pinned llama.cpp, built with CUDA
  (`deploy/llama/Dockerfile`, build args). The host needs the NVIDIA driver and
  the NVIDIA Container Toolkit.

## TLS between hosts

One private CA signs every certificate (`deploy/tls/make-ca.sh`,
`deploy/tls/issue.sh`). It uses ECDSA P-256 and TLS 1.2 or later.

| Link | Server certificate | Client side | Authentication |
|---|---|---|---|
| worker → NATS | `nats` (SANs `nats`, `mhai-nats`, `mhai-app`, the app address) | CA plus the `pipeline` client certificate; **TLS first** (handshake before any NATS byte, on every reconnect) | mutual TLS (`verify: true`), then the pipeline NKey, as today |
| core → NATS (same host) | the same | `NATS_TLS_CA_PATH`, `_CERT_PATH`, `_KEY_PATH`; INFO-first, upgraded through `handshake_first: 300ms` (the fallback is still TLS, never plaintext) | mutual TLS, then the gateway NKey |
| BFFs → NATS (same host) | the same | mhai-web's `NATS_TLS_CA_FILE`, `_CERT_FILE`, `_KEY_FILE` and a `tls://` URL; INFO-first | mutual TLS, then each BFF's NKey |
| worker → Postgres | `postgres` (SANs `postgres`, `mhai-app`, the app address) | `sslmode=verify-full&sslrootcert=<CA>` | TLS with the name checked, then SCRAM-SHA-256 for `intelligence_app` |
| core → Postgres (same host, internal network) | unchanged | unchanged (`sslmode=disable`) | SCRAM-SHA-256 |

With `verify: true`, every NATS client needs a client certificate from the CA.
That includes core and both BFFs, so `enable-split.sh` issues certificates for
them too. A NATS client without one cannot connect even if it holds an NKey.
Every client still needs its NKey even if it holds a certificate. The
certificate proves the client is ours. The NKey says which user it is and
what it may do.

Postgres decides who may connect in `pg_hba.conf`. `deploy/postgres/render-hba.sh`
renders it from `deploy/postgres/pg_hba.split.conf`, filling in the
intelligence host's address and the app host's Docker subnets. Its lines, in
order:

```
local   all   all                                trust          # inside the container (health check, ops scripts)
hostssl mhai  intelligence_app  <intel-addr>/32  scram-sha-256  # the worker: TLS, its database, its address only
host    all   all               <intel-addr>/32  reject         # nothing else from the intelligence host
host    all   intelligence_app  all              reject         # the worker's login from nowhere else
host    all   all               <backend subnet> scram-sha-256  # core, core-migrate, core-cli on this host's
host    all   all               <edge subnet>    scram-sha-256  #   Docker networks (named, not samenet)
```

Any other address on the private network matches no line and is refused.

## Firewall

Three layers, so one mistake doesn't open the database:

1. **The cloud firewall** (security group, VPC firewall, or the LAN's).
   - App host inbound: TCP 4222 and 5432 **from the intelligence host's
     private address only**, plus SSH from your admin address and 443 from
     users.
   - Intelligence host inbound: SSH from your admin address only. Nothing
     else.
   - Intelligence host outbound: the app host's 4222 and 5432, the model
     provider's API host on 443 (cloud route), the model store on 443
     (`model-fetch`), and the registry and OS mirrors for updates.
2. **Docker's own chain on the app host.** Docker writes its iptables rules
   ahead of ufw, so ufw rules don't protect published ports. `enable-split.sh`
   prints the `DOCKER-USER` rules (below) for the operator to apply and
   persist. It doesn't apply them itself, so it can't lock you out of SSH.

   ```bash
   # DOCKER-USER sees packets after DNAT, so match the original destination.
   for p in 4222 5432; do
     sudo iptables -I DOCKER-USER -p tcp ! -s <intel-addr> \
       -m conntrack --ctorigdst <app-addr> --ctorigdstport "$p" -j DROP
   done
   sudo netfilter-persistent save   # or your distribution's equivalent
   ```
3. **The services themselves.** NATS refuses any client without a
   certificate from the CA. Postgres refuses every login from the intelligence
   host except `intelligence_app` over TLS, and refuses that login from
   anywhere else.

## Sizing

The pipeline itself (masking, retrieval, the NLI check, the cloud client)
stays at about 1 GiB (cap 1600 MiB) and 1.5 CPUs, as on the pilot.

**The own answer model, Gemma 4 E4B** (`gemma4-e4b-cca-v1`, about 8B raw
parameters, 4B "effective"). These are estimates for Q4_K_M with
`ctx 4096` and one question at a time. Measure on the real host with
`eval-local.sh` before you move a hospital.

| | CPU only | GPU |
|---|---|---|
| GGUF on disk | about 5 GB | about 5 GB |
| `llama` memory cap (`PILOT_LOCAL_MODEL_MEM`) | 8g (RAM) | 4g RAM, plus about 6 GB of VRAM with every layer offloaded |
| Threads / CPUs | 8 (`--threads 8`) | 4 |
| Speed | about 5-10 tokens/s on 8 modern x86 or Graviton3 cores: 30-90 s per answer | 40-100 tokens/s on a T4, L4 or A10G: under 10 s per answer |
| Host, in all | 8 vCPU and 16 GiB (e.g. c7g.2xlarge or c7i.2xlarge) | 4 vCPU, 16 GiB and one 16-24 GB GPU (e.g. g4dn.xlarge, g6.xlarge or g5g.xlarge) |
| Disk | 30 GB (images, mirror and cache: the GGUF is held twice) | 40 GB (the CUDA image is larger) |

The pipeline image must match the host's architecture. The "Pilot pipeline
image" workflow builds amd64 and arm64 under one multi-arch tag by default
(`latest`, each host pulls its own), arm64 alone with `arch: arm64`, and
amd64 alone with `arch: amd64` (tag `latest-amd64`). Most GPU instances are
amd64. The image is CPU PyTorch either way. The app host stays small
(the demo's t4g.medium shape, without the pipeline).

## Security and threat model

### 1. Assets

- **Patient data in flight:** questions, retrieved notes, answers and job
  payloads on NATS, and rows on the Postgres link.
- **Credentials:**
  - the pipeline NKey seed;
  - the `intelligence_app` password;
  - the TLS private keys (the CA key above all);
  - the cloud API key and the model store reader's keys.
- **Request tokens:** they carry hospital and user claims, and they are on
  NATS.
- **Hospital separation:** row-level security under `intelligence_app`, and
  hospitals in the verified token only.
- **Availability** of answers, and **clinical safety**: an unverified answer
  is never final, and a failed link escalates.

### 2. Actors and trust boundaries

Actors:
- core, the BFFs and the worker;
- the operator;
- other machines on the private network;
- anyone on the internet;
- the cloud provider and the model store.

Crossings, and what is checked at each one:

| # | Crossing | Checked |
|---|---|---|
| B1 | intelligence host → app host, NATS 4222 | cloud firewall and DOCKER-USER (source address); TLS with the server name checked against the CA; the server checks the client certificate (`verify: true`); the NKey and its subject permissions |
| B2 | intelligence host → app host, Postgres 5432 | firewall; `pg_hba` (`hostssl`, the source /32, the user, the database); TLS `verify-full` against the CA; SCRAM; RLS |
| B3 | worker → `app-link` (relay) | internal network; the relay forwards to one fixed address and two ports, and TLS runs through it, so it never sees plaintext |
| B4 | worker → egress proxy → provider | unchanged: one API host on 443 (`allowed-hosts`), PII masking and the egress gate before anything leaves |
| B5 | `model-fetch` → model store | unchanged: HTTPS, reader credentials, and the worker verifies everything offline |
| B6 | operator → both hosts | SSH; the scripts take secrets on stdin only, never print them, and write them as 0400 or 0600 files |

### 3. Threats (STRIDE) and mitigations

| | Threat | Mitigation |
|---|---|---|
| **S** | A machine on the private network pretends to be NATS or Postgres (DNS or ARP spoofing, a stolen IP). | The worker checks the server certificate against our CA only (not the system roots) and checks the name (`mhai-app`). It does the TLS handshake first, so a fake plaintext server gets no CONNECT and no NKey signature. Postgres runs `verify-full` with `sslrootcert`. |
| **S** | Someone connects to NATS as the pipeline. | They need a client certificate from our CA *and* the NKey seed. Both stay on the intelligence host (D4). The firewall admits the intelligence host only. |
| **S** | Someone logs in to Postgres from another host, or as another login (`core_app`, `pgadmin`) from the intelligence host. | `pg_hba`: only `intelligence_app`, only over `hostssl`, only from `<intel-addr>/32`. Every other login from that address is rejected. That login from anywhere else is rejected. |
| **T** | Someone changes a request or job in flight. | TLS on both links. Tokens are EdDSA-signed by core and verified by the worker, as before. |
| **T** | A compromised relay changes bytes. | TLS is end to end through it, so any change breaks the TLS session. |
| **R** | Who did what. | Unchanged: core audits every action. The worker's job outcomes go to Postgres under its own login. `pg_hba` and NATS log refused connections on the app host. |
| **I** | Someone sniffs the private network. | Every byte between the hosts is TLS. The demo's plaintext links stay inside one host's Docker networks. |
| **I** | Secrets leak through scripts, logs or the repository. | The scripts read secrets from stdin and write them into a container with no network (0400, the service's uid). They print public keys, paths and fingerprints only. `issue.sh` never prints a key. No certificate or key is committed: the tests make their own at test time. |
| **I** | The worker sends data to the internet. | It is on internal networks only. Its only ways out are the relay (two ports, one fixed address: the app host, which already holds the data) and the egress proxy (one provider host, after masking). |
| **D** | Someone floods 4222 or 5432 from the network. | The firewall admits one source. NATS refuses a client without a certificate after the handshake. Postgres `max_connections=40`. The worker's pool is capped at 4. |
| **D** | The link between the hosts goes down. | Questions fail at their deadline and escalate. JetStream keeps jobs for redelivery. The worker reconnects TLS-first. Nothing is answered unchecked. |
| **D** | A certificate expires. | Leaf certificates last 365 days and the CA 10 years. `enable-split.sh` and `enable-intelligence-host.sh` print the expiry dates and warn under 30 days. Renewal is `--renew` (runbook). |
| **E** | The worker's login gets more than its grants. | Unchanged: a member of `mhai_worker_app` only, RLS, and `check_login` refuses role or setting drift at start. The `pg_hba` lines add no privilege. |
| **E** | The CA key is stolen. | Then the attacker can mint client and server certificates, but still needs an NKey seed or a database password to log in. Keep the CA directory off both hosts after issuing (D3). To recover, make a new CA and re-issue everything. |
| **E** | The single-host exception is used from the intelligence host. | It is refused: remote names have a dot or are addresses. The intelligence host's `.env` never sets the switch, and `enable-intelligence-host.sh` refuses to run when it is set. |

### 4. Controls (decided)

- **Authentication:** mutual TLS on NATS plus NKeys. TLS with `verify-full`
  on Postgres, plus SCRAM. Request tokens are unchanged.
- **Authorization:** NATS subject permissions are unchanged (a test compares
  them). `pg_hba` restricts by address, login, database and TLS. RLS and
  the `mhai_worker_app` grants are unchanged.
- **Data handling:** patient data is encrypted on every link between hosts.
  The intelligence host stores none, apart from model weights and the
  verified cache. Its logs keep the existing rules (no PII, fixed codes).
- **Concurrency:** unchanged (one question at a time, job slots).
- **Secrets:**
  - The CA key is 0600 in a 0700 directory, and offline after issuing.
  - Leaf keys are 0400, owned by their service's uid.
  - The pipeline NKey seed and its TLS key are generated **on the
    intelligence host** and never leave it. Only the CSR and the public
    key cross.
  - The database password crosses once, through an SSH pipe, never on
    screen.
  - Placeholders are refused at start (as before).
- **Third parties:** unchanged (the cloud provider under its agreement, and
  the model store).

### 5. Abuse-case tests

| Behaviour | Test |
|---|---|
| The scripts refuse a public or repeated address, a malformed NKey, an unknown provider or a plain-http store before they touch anything | `deploy/tls/test-split-lib.sh` |
| The llama entrypoint offloads `LLAMA_GPU_LAYERS` with `-ngl` and refuses a malformed value | `deploy/llama/test-entrypoint.sh` |
| A production worker refuses a plaintext NATS server (no CONNECT is sent) | `tests/boundary/test_worker.py::test_production_worker_refuses_a_server_without_tls` (existing) |
| The worker connects to the TLS NATS (`deploy/nats/tls.conf`) with the CA and its client certificate, and the connection is TLS | `tests/boundary/test_worker_tls.py` |
| … refuses a server signed by another CA | `tests/boundary/test_worker_tls.py` |
| … refuses a certificate for another name | `tests/boundary/test_worker_tls.py` |
| The server refuses a client with no certificate (`verify: true`) | `tests/boundary/test_worker_tls.py` |
| An INFO-first client (core, the BFFs) still connects through `handshake_first` | `tests/boundary/test_worker_tls.py` |
| Production settings refuse a remote host without TLS, `sslmode` other than `verify-full`, or `verify-full` without `sslrootcert` | `tests/security/test_remote_transport.py` |
| Two networks standing in for two hosts: the real worker code connects through the relay to NATS (TLS-first, NKey) and Postgres (`verify-full`); a wrong CA is refused; Postgres refuses plaintext, refuses `core_app` from the intelligence address and refuses `intelligence_app` from another address; NATS refuses a client without a certificate | `deploy/intelligence-host/test-split.sh` (CI, boundary-smoke job) |
| The CA scripts write keys 0600 and print no key; certificates carry the right SANs and EKUs; a CSR with the wrong name is refused | `deploy/tls/test-tls.sh` |
| `nats-server -t` accepts `deploy/nats/tls.conf` with test certificates | `deploy/nats/check-configs.sh` |
| Every compose file renders (app-host override, intelligence host, GPU) | CI "Compose files are valid" |
| The single-host path still works | `boundary-smoke` (unchanged) |

### 6. Residual risk and sign-off

- **Postgres joins `edge` in split mode.** Docker publishes a port only
  from a network that isn't internal, so the Postgres container gets a route
  out. Nothing in Postgres starts an outbound connection. The firewall's
  outbound rules on the app host can close this.
- **NATS has one account.** Splitting accounts (TODO, "Split NATS accounts")
  is still open. TLS doesn't change that residual.
- **`request-keys.json` is copied by hand.** After a request-key rotation,
  copy it to the intelligence host again. Until then the worker refuses
  tokens signed by the new key (fail closed, an outage, not a leak).
- **Core's own Postgres link stays plaintext** on the app host's internal
  Docker network (TODO "Postgres TLS" keeps that item for core).
- **IPv4 only** in the scripts.
- **`pg_hba.conf` names the app host's Docker subnets.** If the `backend` or
  `edge` network is ever recreated with another subnet, core can't log in
  (it fails closed). Run `enable-split.sh` again to fix it.
- **The enable scripts were tested in parts, not end to end on two real
  hosts.** Their checks are tested (`test-split-lib.sh`), and the files they
  write are tested in `test-split.sh`. The first real run is the pilot's
  rehearsal.
- **Sign-off:** none clinical. Privacy: patient data now crosses a network
  link, encrypted and between our two hosts. Record it with the pilot's
  deferred sign-offs (PR #134).

## Decisions (delegated by the product owner, 14 Nov)

| # | Decision | Why |
|---|---|---|
| D1 | NATS mutual TLS (`verify: true`) for every client, with `handshake_first: 300ms`; NKeys unchanged | The production block `nats.conf` already described; core and mhai-web already support client certificates. |
| D2 | Postgres TLS with a server certificate; `intelligence_app` only over `hostssl` from the intelligence host's /32; core's local login unchanged | Smallest change that closes the remote link; core's link never leaves the host. |
| D3 | One private CA (ECDSA P-256), made by `make-ca.sh`; leaves for 365 days, the CA for 10 years; keep the CA directory off both hosts after issuing | No public CA can name private addresses; short leaves bound a leak. |
| D4 | The intelligence host makes its own TLS key and NKey seed; only the CSR and the public key cross to the app host. The database password is the one secret that crosses (stdin, SSH pipe) | Keys never travel. |
| D5 | The worker stays on internal networks; a TCP relay (`app-link`, HAProxy) is the only container with a route to the app host | Keeps the existing "no route out" property without depending on a host firewall. |
| D6 | NATS and Postgres are published on the app host's private address only (RFC 1918 or 100.64/10, checked); firewall in the cloud and in `DOCKER-USER` | Docker bypasses ufw. |
| D7 | The deploy switch is `COMPOSE_FILE=compose.yml:compose.split.yml` in the app host's `.env`; the demo never sets it, so TLS stays off there | One line, survives the deploy workflow (`docker compose` reads `.env`). |
| D8 | `request-keys.json` (public) is copied to the intelligence host; re-copied after a rotation | No new service; fails closed. |
| D9 | GPU through a CUDA build of the same pinned llama.cpp (build args) and `LLAMA_GPU_LAYERS`; CPU stays the default | Same commit as the answer gate; optional. |
| D10 | `CORE_ENV` stays as the app host's compose file sets it; the BFFs get client certificates from `enable-split.sh` | The split is about transport; production mode for core is its own change. |
| D11 | IPv4 only for now | Every target VPC hands out IPv4. |
| D12 | Outside development, `sslmode=verify-full` needs `sslrootcert` (an absolute path) | Otherwise libpq would look for `~/.postgresql/root.crt` and fail at connect time, not at start. |

## Operator steps

Prerequisites:
- two hosts in one private network, and their private IPv4 addresses
  (`APP_ADDR`, `INTEL_ADDR`);
- Docker and Compose v2 on both;
- for a GPU host, the NVIDIA driver and the NVIDIA Container Toolkit;
- a checkout of this repository at the deployed release on both hosts and on
  your machine.

Never run these on the demo host.

1. **App host: the backend as usual.** Run `bash deploy/setup-host.sh` and do
   the first deploy (deploy/README.md). Don't enable the pilot here.
2. **Your machine: the CA.** Run
   `bash deploy/tls/make-ca.sh ~/mhai-ca`. This writes `ca.pem` and
   `ca-key.pem` (0600) in a 0700 directory. Keep the directory on an
   encrypted disk.
3. **Intelligence host: keys and a CSR.**

   ```bash
   bash deploy/intelligence-host/enable-intelligence-host.sh init --app-addr "$APP_ADDR"
   ```

   This makes the pipeline's NKey seed and its TLS key, and prints the NKey's
   public key and the CSR's path (`/srv/mhai-intelligence/pipeline.csr`).
   Copy the CSR (public) to your machine.
4. **App host: TLS on.** Copy the CA directory there for this step only, then
   remove it:

   ```bash
   bash deploy/enable-split.sh --app-addr "$APP_ADDR" --intelligence-addr "$INTEL_ADDR" \
     --ca-dir ~/mhai-ca --pipeline-nkey <public key from step 3> --pipeline-csr pipeline.csr
   ```

   It issues the NATS and Postgres server certificates and the core and BFF
   client certificates (0400, each owned by its service's uid), signs the
   CSR, writes `pg_hba.conf`, sets `COMPOSE_FILE` and the pipeline's NKey,
   and makes `INTELLIGENCE_DB_PASSWORD` (core-migrate then creates the
   login). It recreates `postgres`, `nats` and `core`, waits for `readyz`,
   and prints the firewall rules. Everything to copy to the intelligence
   host is in `/srv/mhai-backend/split-out/`: `ca.pem`, `pipeline.pem` and
   `request-keys.json`, all public.
5. **The frontend.** Set mhai-web's live services to `NATS_URL=tls://mhai-nats:4222`
   plus `NATS_TLS_CA_FILE`, `NATS_TLS_CERT_FILE` and `NATS_TLS_KEY_FILE`.
   The files are in `/srv/mhai-backend/secrets/bff/tls/`, owned by uid 1001.
   Until then the BFFs can't reach NATS (`verify: true`).
6. **Firewall.** Apply the rules from the "Firewall" section above (cloud
   rules, plus the `DOCKER-USER` lines the script printed).
7. **Intelligence host: start.** Copy `split-out/*` there, then pipe the
   database password from the app host without showing it:

   ```bash
   ssh app-host "sed -n 's/^INTELLIGENCE_DB_PASSWORD=//p' /srv/mhai-backend/.env" \
     | ssh intel-host "bash mhai/deploy/intelligence-host/enable-intelligence-host.sh up \
         --app-addr $APP_ADDR --ca split-out/ca.pem --client-cert split-out/pipeline.pem \
         --trusted-keys split-out/request-keys.json [--gpu]"
   ```

   It checks the certificate against the CA, its key and its use, installs
   it, writes the password file (0400), starts `app-link` and `pipeline`,
   and waits for `worker started service=pipeline`.
8. **Cloud route** (optional): first put the key in place:

   ```bash
   bash deploy/intelligence-host/put-secret.sh pilot-cloud-api-key < key
   ```

   Then run `up` again with `--provider anthropic|gemini --zdr-ref <ref>
   [--model <id>] [--gemini-paid-tier verified]`. On the app host, set the
   hospital's model policy with `core-cli` (pilot README, steps 7 and 8;
   `CORE_CLOUD_PROVIDERS` via `enable-split.sh --cloud-provider`).
9. **Own model** (optional): first put the model store reader's two keys in
   place:

   ```bash
   put-secret.sh model-store-reader-access-key
   put-secret.sh model-store-reader-secret-key
   ```

   Then run `up` again with `--ownmodel --store-endpoint https://… --store-bucket …
   [--mem 8g --threads 8]` (and `--gpu` on a GPU host). It copies
   `models.lock`, builds `llama` (CUDA with `--gpu`), runs `model-fetch`,
   and waits for `model.served role=cca`.

**Renewal (yearly):** run `enable-split.sh … --renew`, then `init --renew` and
`up` on the intelligence host. **Rotating the request-token key:** after
`core keys` rotates it, copy `split-out/request-keys.json` again (re-run
`enable-split.sh`), then run `up`. **Switching off:** on the intelligence
host, `docker compose down`. On the app host, remove the `COMPOSE_FILE` line
from `.env` and run `docker compose up -d --force-recreate postgres nats core`.
The firewall rules can stay.
