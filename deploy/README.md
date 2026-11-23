# Backend demo deployment

> **22 Nov 2026: the public demo is dropped.** The deployment runs as the
> live site with the repository variable `SITE_MODE=live` ("Live site",
> below). The demo sections stay for anyone who runs a demo (`SITE_MODE`
> unset or `demo`, the default).

The backend (the core service, Postgres and NATS) runs on the shared preview
instance, next to the EMR stack and the mhai-web demo. Nothing is open to
the internet:

```
host 127.0.0.1:8092 ──> core (HTTP API, probes)
host 127.0.0.1:4222 ──> NATS (client port; NKey users only)
frontend containers ──mhai-bff network──> mhai-nats:4222   (Plan 3)

network backend (internal, no route out): core, postgres, nats
```

Every merge to `main` that passes CI runs `.github/workflows/deploy.yml`:

1. build the core image (`services/core/Dockerfile`: static binary on
   distroless, non-root), and the intelligence (sample answers) and
   notifications images, for linux/amd64 and linux/arm64 on GitHub, each on
   a native runner, and push them to
   `ghcr.io/measured-health/mhai-multiagent-core` (and `-intelligence`,
   `-notifications`) as one multi-arch tag each (`latest` and
   `sha-<commit>`): an arm64 (Graviton) or x86_64 host pulls its own
   architecture from the same tag;
2. if `DEPLOY_ENABLED` is `true`: SSH to the instance, copy this commit's
   `compose.yml` and `nats/nats.conf` into `/srv/mhai-backend`, pull, run the
   migrations, restart core, and check `GET /api/v1/healthz` and
   `GET /api/v1/readyz` on 127.0.0.1 (Postgres, NATS and every KV bucket).

It runs only for a push to this repository's `main` (never a fork), checks
the instance's SSH host key against `DEPLOY_HOST_FINGERPRINT`, logs in
to GHCR with the job's own `GITHUB_TOKEN` (logged out at the end), and never
touches the EMR containers, the web demo or nginx.

## What runs

| Service | Image | Role | Memory cap |
|---|---|---|---|
| `postgres` | `postgres:16.15-alpine`, uid 70 | the database; no published port | 256 MiB |
| `nats` | `nats:2.10.29-alpine`, uid 1000 | JetStream (KV buckets, the `JOBS` and `DEAD` streams) on a volume; NKey users `gateway` (core), the clinical BFF (`NATS_BFF_NKEY`: `api.v1.clinical` only) and the platform BFF (`NATS_BFF_PLATFORM_NKEY`: `api.v1.platform` only) | 128 MiB |
| `core-migrate` | core | one-shot each deploy: goose migrations, then the `core_app` and `core_provision` logins | 128 MiB |
| `core-keys` | core | one-shot: the access-token signing key, made once (`core keys ensure`) | 64 MiB |
| `core-request-key` | core | one-shot: the request-token signing key, made once, and its public half in the `mhai-backend-trustedkeys` volume | 64 MiB |
| `core` | core, uid 65532 | `core serve` on 127.0.0.1:8092 | 320 MiB, CPUs 0-1 |
| `core-cli` | core | the seed CLI, run by hand (profile `tools`) | 128 MiB |
| `egress-proxy` | `ubuntu/squid` by digest, uid 13 | the cloud providers' only way out (profile `cloud`, off by default; see "Cloud models") | 128 MiB |
| `pipeline` | `mhai-multiagent-pipeline-pilot` (weights baked in) | the pipeline worker, for the pilot only (profile `pilot`, off by default; see `pilot/README.md`) | 1600 MiB |
| `core-demo-seed` | core | one-shot each deploy: the synthetic demo hospital and its published demo accounts (profile `demo`; "Public demo: sample answers") | 128 MiB |
| `sample-ai` | `mhai-multiagent-intelligence` | the pipeline worker with labelled sample answers, for the demo hospital only (profile `demo`) | 192 MiB |
| `notifications` | `mhai-multiagent-notifications`, uid 65532 | email for invites, reset links, notices and reminders (profile `notifications`, off by default; see "Turn on email for the demo") | 64 MiB |
| `mail-relay` | `alpine/socat` by digest, uid 65534 | the notifications service's only way out: TCP to the SMTP host and port, nothing else (profile `notifications`) | 32 MiB |

Every container has a read-only root filesystem, no Linux capabilities and
`no-new-privileges`. The pipeline worker runs only for the pilot (one real
hospital with cloud answers, `pilot/README.md`; `pilot/enable-pilot.sh`
sets it up); echo never comes here. Until the trained weights arrive, the
public demo's questions are answered by `sample-ai` instead (below). Without
either, core starts and serves everything else; a question (`POST /queries`)
is queued and fails at its 10-minute deadline. `CORE_HTTP_API` stays `on`.

### Before merging Plan 3 (the boundary): the demo's `clinical_queries`

Plan 3 adds migrations 00007 to 00009. 00007 adds CHECK constraints that
validate the rows already in `clinical_queries` (status one of `queued`,
`running`, `completed`, `failed`, `cancelled`; `error` a short code). The
deploy runs `premigrate-check.sh` first and stops, before anything
migrates, if a row would fail; each migration is one transaction, so
nothing half-applies either way. Check on the instance before merging,
and expect `6|0` (migrations up to 00006, no questions stored):

```bash
cd /srv/mhai-backend
docker compose exec -T postgres psql -U pgadmin -d mhai -tAc \
  "SELECT (SELECT max(version_id) FROM goose_db_version WHERE is_applied), (SELECT count(*) FROM clinical_queries)"
```

If the count isn't 0, look at the rows (no PHI is expected on the demo:
synthetic data only) before merging:
`SELECT status, error IS NULL, count(*) FROM clinical_queries GROUP BY 1, 2`.

### Database logins

As in `services/core/README.md`:

- `pgadmin`, the owner (a superuser in this Postgres). Only the one-shot
  `core-migrate` connects as it. Migration 00003 makes it a member of
  `mhai_migrate`, the NOLOGIN role that owns every table, view and function.
- `core_app`, the core service's login: a member of `mhai_app` only. It is
  not a superuser and has no BYPASSRLS, CREATEROLE, CREATEDB or REPLICATION.
  `core serve` checks this at start-up (`db.CheckLoginRole`) and refuses
  any other login.
- `core_provision`, the seed CLI's login: a member of `mhai_provision` only.

### Secrets

All of them are made on the instance by `setup-host.sh`, never in the repo
or an image:

| Where | What |
|---|---|
| `/srv/mhai-backend/.env` (mode 600) | `POSTGRES_PASSWORD`, `CORE_DB_PASSWORD`, `PROVISION_DB_PASSWORD`, `MFA_ENCRYPTION_KEY`, `BFF_SERVICE_KEYS`; the NATS public keys `NATS_GATEWAY_NKEY`, `NATS_BFF_NKEY`, `NATS_BFF_PLATFORM_NKEY` (no seed is ever in `.env`); the security contact `SECURITY_CONTACT_NAME`, `SECURITY_CONTACT_EMAIL` (not secrets) |
| `/srv/mhai-backend/secrets/nats-gateway.nk` (mode 400, uid 65532) | the core service's NATS NKey seed, mounted into `core` only |
| `/srv/mhai-backend/secrets/bff/nats-bff-clinical.nk`, `nats-bff-platform.nk` (mode 400, uid 1001; folder mode 750) | each frontend BFF's NATS NKey seed, mounted by mhai-web's compose into that app's container only |
| volume `mhai-backend-corekeys` | the access-token signing key (Ed25519) |
| `/srv/mhai-backend/secrets/smtp_password`, `nats-notifications.nk` (mode 400, uid 65532) | only with email on: the mailbox password (written by the owner) and the notifications service's NATS seed, mounted into `notifications` only; its login password `NOTIFY_DB_PASSWORD` is in `.env` |

Each container gets only its own secrets: `core` never sees the owner
password, `core-migrate` never sees the core secrets, and NATS sees only
public keys.

## One-time setup (product owner)

A whole new server (backend and clinical app on one domain) in one script: [NEW-SERVER.md](NEW-SERVER.md).

### 1. The `preview` environment in this repository

Copy it from mhai-web (GitHub, Measured-Health/mhai-web, Settings,
Environments, `preview`): the same instance and the same key.

In Measured-Health/mhai-multiagent, Settings, Environments, New environment
`preview`, add these secrets:

| Secret | Value |
|---|---|
| `DEPLOY_HOST` | the instance's address, as in mhai-web (3.239.87.6) |
| `DEPLOY_USER` | `ubuntu` |
| `DEPLOY_SSH_KEY` | the private key (`mh.pem`), as in mhai-web |
| `DEPLOY_HOST_FINGERPRINT` | the instance's SSH host key fingerprint, `SHA256:...` (below) |

Secret values can't be read back from mhai-web; paste them again. Or, with
the GitHub CLI:

```bash
gh api -X PUT repos/Measured-Health/mhai-multiagent/environments/preview
gh secret set DEPLOY_HOST    --env preview --repo Measured-Health/mhai-multiagent --body 3.239.87.6
gh secret set DEPLOY_USER    --env preview --repo Measured-Health/mhai-multiagent --body ubuntu
gh secret set DEPLOY_SSH_KEY --env preview --repo Measured-Health/mhai-multiagent < mh.pem
```

**Host key fingerprint.** The deploy checks the instance's SSH host key, so
nobody in between can pose as the instance and receive the GHCR token; it
refuses to run while the secret is missing. `appleboy/ssh-action` v1.2.5
compares the full `SHA256:...` string, and the key it is offered is the
host's **ECDSA** key (its SSH library asks for ECDSA before Ed25519), so
take that one. On the instance:

```bash
ssh-keygen -lf /etc/ssh/ssh_host_ecdsa_key.pub | awk '{print $2}'   # SHA256:...
```

Check it against what your own machine sees (the two must match, or stop):
`ssh-keyscan -t ecdsa 3.239.87.6 2>/dev/null | ssh-keygen -lf -`. Then:

```bash
gh secret set DEPLOY_HOST_FINGERPRINT --env preview --repo Measured-Health/mhai-multiagent --body 'SHA256:...'
```

If the instance has no ECDSA key, use the RSA one
(`/etc/ssh/ssh_host_rsa_key.pub`). If the instance is rebuilt, its host
keys change: update the secret, or the deploy fails with
`ssh: host key fingerprint mismatch`.

Optionally, limit the environment's deployment branches to `main`.

### 2. Run the setup script on the instance

From a checkout of this repository, on your machine:

```bash
scp -i mh.pem -r deploy ubuntu@3.239.87.6:/tmp/mhai-backend-deploy
ssh -i mh.pem ubuntu@3.239.87.6
bash /tmp/mhai-backend-deploy/setup-host.sh
rm -rf /tmp/mhai-backend-deploy
```

It prints each step. It creates `/srv/mhai-backend` (with `sudo`, once),
copies the compose file and the NATS config, writes `.env` (mode 600) and
the NATS seed, makes the volumes `mhai-backend-pgdata`, `-natsdata` and
`-corekeys` and the internal network `mhai-bff`, checks that ports 8092 and
4222 are free on the host, and validates the compose file. It starts
nothing and doesn't touch nginx or any other container.

It is safe to run again: it never replaces a secret, a key or a volume that
exists. If a port is taken (by the EMR, for example), re-run it with
another, which it saves in `.env` for the workflow:
`CORE_HOST_PORT=8093 NATS_HOST_PORT=4223 bash .../setup-host.sh`.

It pulls `natsio/nats-box` once, to make the NKeys inside a container with
no network.

**The security contact** (`SECURITY_CONTACT_NAME`, `SECURITY_CONTACT_EMAIL`)
must be in `/srv/mhai-backend/.env` for the demo's audit integrity page to
work (Plan 4b): the smoke test and the page both need them. They name who
the platform app's audit integrity page shows. They are required: core
refuses to start without them outside development. They are never generated (a
made-up contact would be shown to auditors as real): `setup-host.sh` takes it
from the environment, keeps it from `.env`, or asks for it at a terminal.
On this demo (development) a missing contact doesn't stop the deploy, but
`GET /platform/audit/verify/status` answers 503 until it is set. On a host
that is already set up, either run this repo's newer `setup-host.sh` again
(as in step 2) with the two variables in its environment, or add the two
lines to `/srv/mhai-backend/.env` yourself; then recreate core (the next
deploy does it too):

```bash
cd /srv/mhai-backend
printf '%s\n' "SECURITY_CONTACT_NAME=Measured Health security" "SECURITY_CONTACT_EMAIL=security@example.org" >> .env
docker compose up -d core
```

`setup-host.sh` checks the values as core does (a name of 1 to 120 characters,
trimmed, no control character and no pseudonym such as `P-AB12`; a plain
address of at most 254 characters). Values also hold no quote, backslash,
backtick, `$`, `#` or new line, because compose reads `.env`.

### 3. Turn the deploy on

Repository variable `DEPLOY_ENABLED=true` (Settings, Secrets and variables,
Actions, Variables), or:

```bash
gh variable set DEPLOY_ENABLED --repo Measured-Health/mhai-multiagent --body true
```

Then run the workflow once by hand (Actions, Deploy backend demo, Run
workflow, branch `main`), or merge to `main`. The first image push creates
the GHCR package `mhai-multiagent-core`, private and linked to this
repository, so the job's token can pull it.

### 4. Check it, on the instance

```bash
curl -s http://127.0.0.1:8092/api/v1/readyz      # {"status":"ready"}
cd /srv/mhai-backend && docker compose ps
```

## Day to day

```bash
cd /srv/mhai-backend
docker compose logs --tail=100 core
docker compose logs core-migrate          # the last migration runs
docker compose restart core
docker compose down                       # stops the stack; the volumes stay
```

Seed a synthetic hospital and user, as the provisioning login (synthetic
data only):

```bash
cd /srv/mhai-backend
docker compose --profile tools run --rm -T core-cli hospitals create \
  --name "Synthetic Hospital" --short-code SYN --time-zone Africa/Lagos
docker compose --profile tools run --rm core-cli users create \
  --email synthetic.clinician@syn.example --name "Synthetic Clinician" \
  --roles clinician --hospital SYN       # prompts for the password
```

The volumes are external, so `docker compose down -v` can't delete them.
Deleting the database for real is `docker compose down` then
`docker volume rm mhai-backend-pgdata`.

## The frontend and NATS (Plan 3)

The frontend's BFFs can reach core over NATS (spec §17.1). Two NKey users,
each with its own subject and inbox and nothing else:

| User | Public key in `.env` | Publishes | Receives |
|---|---|---|---|
| `bff_clinical` | `NATS_BFF_NKEY` | `api.v1.clinical` | `_INBOX_bff.clinical.>` |
| `bff_platform` | `NATS_BFF_PLATFORM_NKEY` | `api.v1.platform` | `_INBOX_bff.platform.>` |

(nats.go: `nats.CustomInboxPrefix("_INBOX_bff.clinical")`.) No BFF-direct
intelligence operation is open on the demo.

Each seed is a file written by `setup-host.sh`, never in `.env`:
`/srv/mhai-backend/secrets/bff/nats-bff-clinical.nk` and
`nats-bff-platform.nk`, mode 400, owned by uid 1001 (the `nextjs` user in
mhai-web's images), in a folder of mode 750. mhai-web's compose mounts the
file as a secret at `/run/secrets/`. An older `NATS_BFF_NKEY_SEED` in `.env`
is moved into its file on the next run (same key pair, so nothing re-keys)
and the line is removed.

### Before merging (once, on the instance)

The nats service now needs `NATS_BFF_PLATFORM_NKEY` (`:?`), and merging
deploys at once, so run the new `setup-host.sh` first or the deploy stops at
the compose check. From a checkout of this branch (a fresh folder on the
host each time, removed afterwards; never a fixed path in `/tmp`):

```bash
( H=ubuntu@3.239.87.6; K=mh.pem
  d=$(ssh -i $K $H 'mktemp -d') && trap 'ssh -i $K $H "rm -rf $d"' EXIT \
  && scp -i $K -r deploy "$H:$d/" \
  && ssh -i $K $H "bash $d/deploy/setup-host.sh" )
```

It adds the platform pair, moves the clinical seed into its file, and
rewrites `/srv/mhai-backend/nats/nats.conf` and `compose.yml`. **Merge
promptly after running it.** Until the deploy recreates `nats`, the running
container keeps its old environment but the file on disk is the new config
(which names `$NATS_BFF_PLATFORM_NKEY`): if NATS is restarted in that window
(a host reboot, `docker restart`) it fails to start and crash-loops. If that
happens, `cd /srv/mhai-backend && docker compose up -d nats` recreates it
with the new environment.

Checks: `grep -c SEED /srv/mhai-backend/.env` prints 0 and
`sudo ls -ln /srv/mhai-backend/secrets/bff` shows two `-r--------` files for
uid 1001.

**uid 1001.** The seed files are owned by uid 1001, the `nextjs` user inside
mhai-web's images. On the host that number should belong to nobody. Check
first with `getent passwd 1001`: it should print nothing. On a typical
Ubuntu host the deploy user `ubuntu` is uid 1000 and 1001 is free, but an AWS
instance with the SSM agent creates `ssm-user` as 1001 (a login with sudo).
If it prints a user, that user can read both seeds; `setup-host.sh` prints a
warning in that case. Fix it by giving mhai-web's images a different uid
(and `WEB_UID` in the script), not by ignoring the warning. It has not been
checked on the demo instance itself: do it before running the script.

The script also copies itself to `/srv/mhai-backend/setup-host.sh`
(mode 750), so later runs need no new copy of `deploy/`.

### Rotating a BFF's key

On the instance, as the deploy user:

```bash
bash /srv/mhai-backend/setup-host.sh --rotate-bff clinical   # or platform, or all
```

It makes a new pair (the seed goes to a temporary file in the folder and is
moved into place, so a failure leaves the old seed), replaces the public key
in `.env` and the seed file, and prints under "Done" what to restart:
`docker compose up -d --force-recreate nats` in `/srv/mhai-backend`, then the
app's container (it reads the file at start). Until both are restarted that
BFF cannot connect. Rotate after any suspected leak of a seed.

A normal run never makes a new pair when a key exists: if `.env` has a BFF
public key but there is neither a seed file nor a legacy seed to move, or a
legacy seed in `.env` differs from the seed file or from the public key, the
script stops and changes nothing. Use `--rotate-bff` only when you mean to
re-key.

- **From a container** (how mhai-web runs): add the shared network to the
  service in mhai-web's `deploy/compose.yml` and connect to
  `nats://mhai-nats:4222` with that app's seed file (mounted as a secret into
  that app's container only):

  ```yaml
  services:
    web:
      networks: [default, mhai-bff]
  networks:
    mhai-bff:
      external: true
  ```

  `mhai-bff` is internal (no route out), and only NATS is on it from this
  stack: the web container can't reach Postgres or core through it.
- **From the host:** `nats://127.0.0.1:4222`. A container's 127.0.0.1 is its
  own, so the frontend's containers can't use this.

## Live site (no public demo)

**Product owner, 22 Nov 2026: the public demo is dropped.**
`ai.measured-health.org` serves the live clinical app and
`platforms.measured-health.org` the live platform admin app (still behind
its nginx IP allow list). The demo code stays in the repository, off.

One repository variable switches a deployment between the two, in **both**
repositories (Settings, Secrets and variables, Actions, Variables):

| Variable | Values | Effect here |
|---|---|---|
| `SITE_MODE` | unset or `demo` (the default: the public demo, as before); `live` | `live`: "Deploy backend demo" runs `live-site.sh` instead of `demo/enable-demo.sh` (no demo seed, no sample answers), and "Reset backend demo" never runs (its schedule is skipped; a run by hand fails, saying why). Any other value stops the deploy. |

```bash
gh variable set SITE_MODE --repo Measured-Health/mhai-multiagent --body live
gh variable set SITE_MODE --repo Measured-Health/mhai-web --body live
```

`DEMO_SAMPLE_AI`, `DEMO_RESET` and `DEMO_RESET_HOURS` (and mhai-web's
`DEMO_MODE`) are ignored while `SITE_MODE=live`; delete them to keep the
settings plain.

**What `live-site.sh` does on every live deploy.** It runs twice: `live-site.sh
precheck` after the pre-migration check and **before core (re)starts**,
against Postgres alone, and `live-site.sh` again once core is healthy on the
migrated database. Between the two, any failure stops core (the deploy
script's EXIT trap), so a live core never keeps serving a database that
wasn't checked. Counts only in the log, never a name or a secret. It fails
closed:

1. writes the marker `/srv/mhai-backend/live-site` (a timestamp). From then
   on `demo/enable-demo.sh` and `demo/reset-demo.sh` refuse on this host,
   whatever runs them, and a deploy with `SITE_MODE` not `live` stops
   instead of seeding the demo. To turn a host back into a demo, an
   operator removes the marker by hand;
2. refuses an `.env` whose `COMPOSE_PROFILES` names `demo`;
3. stops and removes `sample-ai` if it runs, and removes the demo markers
   `demo/seeded` and `demo/reset`;
4. counts the demo hospitals (`is_demo`) that are **not deactivated**. Any
   such hospital has the seeded accounts with **published** passwords, so the
   script **stops core** and fails the deploy: the live apps then answer
   "unavailable" rather than let a published password sign in. A query that
   fails, or a Postgres that never answers, stops core too. Remove the demo
   data (below), then deploy again. The script writes nothing to the
   database: deactivating the hospital (route B) is what ends its accounts'
   sessions (core refuses their refresh at once, and their access tokens
   when the guard's short cache expires); route A removes them with the
   database.

mhai-web's deploy checks the marker before it points the public names at the
live apps (its deploy/README.md, "Live site"), so run "Deploy backend demo"
first.

### Live site: removing the demo data

A host that ran the public demo holds the demo hospital `DEM`, its seeded
accounts (published passwords), the sample content and the visitors'
consultations. Core has no command that deletes a hospital (the audit chain
is append-only and the provisioning login has no `DELETE`), so there are two
safe routes, both by an operator on the host. Nothing here automates them.

**A. Start from an empty database (recommended when the host holds only
demo data, as the new server does).** It removes everything in Postgres and
in NATS (sessions, sign-in counters, queued jobs); the keys, the `.env`
secrets and the NKey files stay.

```bash
cd /srv/mhai-backend
# Only this stack's containers; the volumes are external and stay.
docker compose --profile demo --profile notifications --profile tools down
# Check the names first: these two volumes are the database and NATS's store.
docker volume ls --filter label=org.measured-health.stack=mhai-backend
docker volume rm mhai-backend-pgdata mhai-backend-natsdata
sed -i '/^DEMO_SAMPLE_HOSPITALS=/d' .env
rm -rf demo
```

Then make the two volumes again, empty and owned by the right users, with
this repository's `setup-host.sh` (step 2 of "One-time setup": copy the
`deploy/` folder of `main` to the host and run it; it keeps every secret,
key and volume that exists and makes only what is missing). Then run
"Deploy backend demo" with `SITE_MODE=live`: the migrations run on the empty
database and `live-site.sh` passes.

**B. Keep the database and deactivate `DEM`.** Only when the database holds
something to keep. With a platform admin (below) signed in to the platform
app, deactivate the hospital `DEM` on its hospital page: its accounts lose
their sessions and can't sign in. Do it before setting `SITE_MODE=live`, or,
if a live deploy already stopped core, start it by hand
(`docker compose up -d core`), deactivate `DEM`, then deploy again. The
rows stay (marked `is_demo`, deactivated, named in the audit). The seeded
platform account `platform.bakare@measured-health.example` has a password
nobody knows; deactivate it on the platform app as well.

### Live site: the first platform admin

`core users create` with the role `platform_admin` and no `--hospital`, as
the provisioning login in the one-shot `core-cli` (the password is never an
argument: it prompts at the terminal):

```bash
cd /srv/mhai-backend
docker compose --profile tools run --rm core-cli users create \
  --email <their work email> --name "<their name>" --roles platform_admin
```

They sign in at `https://platforms.measured-health.org` from an address on
the allow list and finish the first sign-in steps the app asks for (terms,
two-step). Hospitals and their first hospital admin the same way:

```bash
docker compose --profile tools run --rm -T core-cli hospitals create \
  --name "<hospital name>" --short-code <CODE> --time-zone Africa/Lagos --require-mfa-for-clinicians
docker compose --profile tools run --rm core-cli users create \
  --email <email> --name "<name>" --roles hospital_admin --hospital <CODE>
docker compose --profile tools run --rm -T core-cli hospitals activate <CODE>
```

### Live site: what it is not yet

- **Real patient data still waits on the go-live sign-offs in TODO.md**
  (the cloud gate H1 to H6, the escalation guard's clinical sign-off H4, the
  Analysis privacy sign-offs, the pilot items). `SITE_MODE=live` only takes
  the public demo away; it signs nothing off.
- No answer worker runs: `sample-ai` is off and the pilot's pipeline is a
  separate switch (`deploy/pilot/`). Until one runs, a question waits and
  fails at its deadline.
- The host still runs core as `development` (NATS without TLS on one host,
  `CORE_DELIVERY=log` unless email is on): production hardening is a TODO.md
  item, not this switch.

## Public demo: sample answers

The public demo (ai.measured-health.org, mhai-web) runs **connected**: the
clinical app in live mode, its BFF over NATS to core, and core's jobs to a
pipeline worker. No trained model exists yet, so the worker answers with
**labelled sample answers** on **synthetic data only**. Every deploy of this
repository runs `demo/enable-demo.sh` (shipped with the bundle) after core
is healthy:

```
browser ─https─> nginx ─> web (live) ─mhai-bff─> nats ─> core ─JOBS─> sample-ai
                                                          │              │
                                                          └── postgres ──┘
```

1. **Not beside the pilot.** If the pilot's pipeline runs (or `.env` has the
   `pilot` profile), it stops `sample-ai` and does nothing else: both would
   be the `pipeline` NATS user on the same consumer, and the demo seed is
   for a database with no real hospital.
2. **The seed:** `core demo seed` (one-shot `core-demo-seed`, as the
   database owner, with `MHAI_DEMO_SEED=on` and `APP_ENV=demo`). It refuses
   a database holding **any hospital that is not a demo hospital**
   (`is_demo`, set at creation and never changed) or any real cloud
   agreement (`AGR-`/`DPA-`). It makes or
   resets the demo hospital `DEM` (`is_demo`, created with the flag, never a
   real hospital), the accounts below, the six shared sample guidelines
   (`KB#0001`–`KB#0006`) and sample content (past consultations, a
   correction waiting for review, an approved one, the Analysis demo
   cohort). Then it writes the marker `/srv/mhai-backend/demo/seeded`
   (a timestamp, not a secret), which mhai-web's deploy needs before it
   switches the public demo to live mode.
3. **sample-ai:** it answers the seeded demo hospital `DEM` only
   (`DEMO_SAMPLE_HOSPITALS` in `.env`, written by the script), never another
   hospital, even one marked as a demo. Its NATS
   NKey (`secrets/nats-pipeline.nk`, public key `NATS_PIPELINE_NKEY`) and
   the `intelligence_app` password (`INTELLIGENCE_DB_PASSWORD` and
   `secrets/intelligence-db-password`) are made on the host inside a
   container with no network, mode 0400, uid 1000, exactly as
   `pilot/enable-pilot.sh` makes them (so the pilot can take them over).
   NATS and core are recreated when the key is new. It is started only with
   at least 320 MiB available and 8 GiB free disk for its image; otherwise
   the deploy passes with a warning and questions wait and fail at their
   deadline. The script prints `free -m`, `df -h /` and each container's
   memory in the deploy log.

**What the sample answer is.** `INTELLIGENCE_MODEL_CLIENT=sample`
(`services/intelligence/intelligence/boundary/sample.py`): the real job path
(request token checks, the claim under the hospital's row-level security,
masking with the real PIIMasker, retrieval of the shared guidelines the
question may see, the **real escalation guard**: red flags, missing
information, dose scope, recital; the decision with its cited sources, the
audit fact). The text starts "Sample answer (synthetic demo data, not
clinical advice):" and quotes the guidelines sharing words with the
question, each cited (`[KB#0002]`). The HGA scores are fixed sample numbers
(trust 0.86): the NLI and embedding models need about 1 GiB and don't fit
the 3.7 GiB host beside MongoDB and the EMR. The model versions on every
decision say `sample`. No model is loaded (no PyTorch: about 65 MiB
resident, measured), nothing is downloaded, and `sample-ai` is on `backend`
only (no route out).

**Why it can't reach production.** The worker refuses the sample client
unless `APP_ENV` is `demo` or `development` (unset is production), refuses
it with a cloud provider or pinned roles, and refuses it without
`INTELLIGENCE_SAMPLE_HOSPITALS`; a job of a hospital not listed is refused
before its claim (dead letter `forbidden`), so a real hospital's question
never gets a sample answer. The seed refuses without `MHAI_DEMO_SEED=on`
and `APP_ENV=demo` (set on its one-shot only), and on a database with any
non-demo hospital. It raises Analysis and named access only when their
switch rows don't exist yet, so an operator's `core analysis switch off`
stays off across deploys. `pilot/enable-pilot.sh` refuses while the demo
hospital has active accounts, and `enable-demo.sh` removes the
`demo/seeded` marker when it finds the pilot.
`TestDemoSwitchesOnlyOnDemoServices` keeps both switches on the demo
services (the seed, the reset and sample-ai).

**The seeded accounts** (published demo values, for the synthetic demo
hospital only; they are reset on every deploy and by the scheduled reset):

| Account | Email | Sign-in |
|---|---|---|
| Clinician | `a.reyes@demo-hospital.example` | password and a code (the clinical app asks every account for two-step) |
| Clinician with two-step | `mfa.okafor@demo-hospital.example` | password and a code |
| New account | `new.bello@demo-hospital.example` | first sign-in steps (new password, terms) |
| New hospital admin | `newadmin.okoro@demo-hospital.example` | first sign-in steps, two-step set-up |
| Hospital admin | `admin.nwosu@demo-hospital.example` | password and a code |
| Medical director (clinician and hospital admin) | `director.obi@demo-hospital.example` | password and a code |
| Analyst (clinician with Analysis) | `analyst.adeyemi@demo-hospital.example` | password and a code |
| Platform admin | `platform.bakare@measured-health.example` | its password and two-step secret are random and thrown away (never published): the account exists with its role, and nobody can sign in with it |
| Locked account | `locked.eze@demo-hospital.example` | its password is random and thrown away: every try fails, and the fifth locks it for that visitor |

The demo password is `Sample-demo-only-2026` and the two-step secret is
`MHAIDEMOSAMPLEONLYNOTASECRET2345` (base32): **published demo values**,
shown by the demo's sign-in helper, valid only for these synthetic accounts.

**Limits.** Visitors share these accounts: they see each other's questions
(the banner and the helper ask for no patient details), one visitor can
change a shared password or enrol two-step on a "new" account, and repeated
wrong passwords from many places lock an account for an hour (the per-login
ceiling), each until the next scheduled reset (below) or deploy. A code
already used in the same 30 seconds is refused (wait for the next).

### Public demo: the scheduled reset

Every 6 hours (by default) the **"Reset backend demo"** workflow
(`.github/workflows/demo-reset.yml`) puts the shared demo back. It wakes at
:23 every hour and goes on only in an hour (UTC) that is a multiple of the
repository variable `DEMO_RESET_HOURS` (1, 2, 3, 4, 6, 8, 12 or 24; default
6), so changing the interval is one variable, not an edit. It needs
`DEPLOY_ENABLED=true`, and `DEMO_RESET=off` stops it. Run it by hand from
Actions, "Reset backend demo", Run workflow (always runs).

It uses the deploy's own SSH secrets and host-key check and shares its
concurrency group (never during a deploy). On the instance it runs
`demo/reset-demo.sh`, which the deploy ships in its bundle; it builds,
pulls and restarts nothing. The script passes without doing anything when
the pilot's pipeline is there, or when there is no `demo/seeded` marker (the
seed never ran or refused, for example beside a non-demo hospital). Then it
runs the one-shot `core-demo-reset` (`core demo reset`), and writes
`demo/reset` (a timestamp).

`core demo reset` is the seed with the same guards (`MHAI_DEMO_SEED=on`,
`APP_ENV=demo`, the database owner, no non-demo hospital and no real cloud
agreement, `DEM` the demo; under the seed's advisory lock), then the seed
itself. Before the seed puts the accounts and samples back, it:

- clears the sample accounts' **sign-in counters** in NATS: the per-login
  ceiling (the hour-long lock everyone shares) and the two-step failure
  counter, by the key names core derives from `MFA_ENCRYPTION_KEY`, and no
  other key. The per-(login, address) counter, which locks only that
  visitor's own address, ends by itself after 15 minutes. For this it reaches
  NATS as core (`secrets/nats-gateway.nk`, read-only); if NATS is set and
  fails, nothing is reset;
- removes, in `DEM` only, what visitors made: consultations other than the
  five samples (with their answers and evaluations), feedback and
  corrections other than the two samples as documented (a sample a visitor
  decided is made again, waiting), with their learning facts and finished
  learning jobs. A question or learning job still queued or running, or one
  whose audit facts the outbox hasn't drained, is kept for the next run;
- deactivates accounts visitors added to `DEM` (the rows stay; the audit
  names them);
- signs every sample account out (sessions revoked, trusted devices
  removed): a visitor's next request is refused and the app shows the
  sign-in page. The seed then restores passwords, first sign-in steps,
  two-step (none on the "new" accounts), roles and the hospital's sign-in
  settings, as on every deploy.

It is audited `demo.reset` (counts only) in the same transaction as the
deletions, and prints the hospital id and counts. Analysis and named access
are raised only as the seed raises them: an operator's switch off stays off.
Every statement names `DEM`'s id; another hospital's rows are never changed
(`TestDemoResetRestoresAccountsAndRemovesVisitorData`).

Why a scheduled workflow, and not a timer on the host: it needs no one on
the host (no SSH by hand, no systemd unit the deploy user may not be able
to install), the schedule and its runs are visible and auditable in
Actions, it can be stopped with one variable, and the reset's credentials
live only in a one-shot container for the few seconds it runs (a cron
container would hold the database owner's password and core's NATS key all
day).

**Before a real hospital joins this database** (the pilot): the seed then
refuses, but the published accounts stay as they were. Deactivate the
hospital `DEM` on the platform app first (its accounts lose their sessions
and can't sign in), set `DEMO_SAMPLE_AI=off`, and set mhai-web's
`DEMO_MODE=mock`; `pilot/enable-pilot.sh` refuses while `DEM` has active
accounts. A hospital must never be marked as a demo (`core hospitals
mark-demo`) unless it holds synthetic data only.

**Switching off.** Sample answers only: set the repository variable
`DEMO_SAMPLE_AI=off` and run "Deploy backend demo" (`gh variable set
DEMO_SAMPLE_AI --repo Measured-Health/mhai-multiagent --body off && gh workflow
run deploy.yml --repo Measured-Health/mhai-multiagent --ref main`). The web
demo back to sample data in the browser (mock mode): mhai-web's
`deploy/README.md`, one line. Before the pilot or the own model takes the
jobs, turn the sample answers off.

## Public API docs (api-dev)

The docs page is public at `https://api-dev.measured-health.org/api/docs`
(product owner, 28 Oct). It has two tabs: the Core API (HTTP, Redoc) and the
Intelligence API (NATS), the latter at `/api/docs/intelligence`, generated from
`proto/` and the operations table. `nginx/mhai-api-dev.conf` serves only
`/api/docs`, `/api/docs/intelligence`, the two scripts, the logo and
`/api/openapi.yaml`, GET only; every other path, the API
included, is 404. One-time setup on the instance, after the DNS A record
`api-dev.measured-health.org` → the instance exists:

```bash
# 1. Proxy settings and the allow list (`allow all;` for the demo, or
#    `allow <ip>;` lines then `deny all;`).
sudo cp deploy/nginx/mhai-api-dev-proxy.conf /etc/nginx/mhai-api-dev-proxy.conf
echo 'allow all;' | sudo tee /etc/nginx/mhai-api-dev-allow.conf
# 2. Serve the :80 block alone, so certbot can answer the challenge.
awk '/^server \{/{n++} n==1' deploy/nginx/mhai-api-dev.conf \
  | sudo tee /etc/nginx/sites-available/mhai-api-dev >/dev/null
sudo ln -sf /etc/nginx/sites-available/mhai-api-dev /etc/nginx/sites-enabled/mhai-api-dev
sudo nginx -t && sudo systemctl reload nginx
sudo certbot certonly --webroot -w /var/www/html -d api-dev.measured-health.org
# 3. The full site, then test before reloading: a bad file must never take
#    the EMR site down.
sudo cp deploy/nginx/mhai-api-dev.conf /etc/nginx/sites-available/mhai-api-dev
sudo nginx -t && sudo systemctl reload nginx
```

The core service must run with the docs on (`CORE_ENV: development` in
`compose.yml`); without them the page is core's 404.

When a release adds a docs path (`/api/docs/intelligence` was added with the
Intelligence tab), the instance's nginx needs the new `location =` line: copy
the file again, `sudo nginx -t`, then `sudo systemctl reload nginx`. Until
then the new path is 404 from nginx, even though core serves it.

## Optional: nginx

There is no public site for the backend, and none is needed while the BFF
reaches it over NATS. `nginx/mhai-backend.conf.disabled` is a disabled
example (HTTPS, an allow list, probes hidden) for the day something outside
the instance must call the HTTP API, after a security review. If you enable
it, test the configuration before every reload, because a bad file must
never take the EMR site down:

```bash
sudo cp deploy/nginx/mhai-backend.conf.disabled /etc/nginx/sites-available/mhai-backend
# edit server_name and the certificate paths, create /etc/nginx/mhai-backend-allow.conf
sudo ln -s /etc/nginx/sites-available/mhai-backend /etc/nginx/sites-enabled/
sudo nginx -t && sudo systemctl reload nginx
```

## Operations: backups, restore test, health alerts

The lean baseline for the pilot (`ops/`): a nightly encrypted backup, a
weekly restore test, and a five-minute health check that alerts through a
webhook. **Off until the operator runs `ops/install-ops.sh`**; the deploy
workflow never touches it and the compose project is unchanged. It runs on
the host as root, from systemd timers. Prometheus, Grafana and the rest are
on TODO.md's "Before scale: operations" list.

| Piece | When | What |
|---|---|---|
| `ops/backup.sh` | nightly, 01:30 UTC (`mhai-backup.timer`) | `pg_dump -Fc` of `mhai` and the roles (no passwords) from one snapshot, with every table's row count; each stream encrypted with `age` as it is written; `SHA256SUMS`; 7 daily + the newest of 4 ISO weeks kept; offsite copy if configured; `/var/lib/mhai-ops/backup.status` |
| `ops/restore-test.sh` | weekly, by the operator | the newest backup into a throwaway Postgres (internal network, removed after): checksums, decrypt, `pg_restore --exit-on-error`, every row count equal to the backup's, the goose version equal, `core migrate status`; prints PASS or FAIL |
| `ops/healthcheck.sh` | every 5 minutes (`mhai-healthcheck.timer`) | containers running and not unhealthy (`postgres`, `nats`, `core`, plus `pipeline` and `egress-proxy` when their profiles are on), core's `readyz`, disk free (≥ 15%), last backup ok and < 26 h old, last restore test passed < 8 days ago; one alert per change of failures (repeated hourly), one "recovered" |

**What is backed up.** The `mhai` database, which holds everything that
matters. Not the NATS JetStream store: its KV buckets live minutes to an
hour, `JOBS` is the work queue of questions in flight (each fails at its
10-minute deadline anyway), and `DEAD` keeps dead-letter ids for 30 days
for diagnosis; core recreates the streams and buckets at start. Losing it
loses in-flight questions and the revoked-token denylist until those tokens
expire. Not the signing keys (`mhai-backend-corekeys`): a new key signs
everyone out once, nothing more. Not `.env` and `secrets/`: keep their
values in the password manager.

### What the operator provides

All in `/etc/mhai-ops/` (root, mode 700). Never commit or paste any of them.

1. **The backup key pair**, made on the operator's own machine, never on
   the host: `age-keygen -o mhai-backup-key.txt`, store the file in the
   password manager, then delete it locally. Put **only the public key**
   (the `age1...` line `age-keygen` prints as "Public key") in
   `/etc/mhai-ops/backup-recipients.txt`. A second line can hold a second
   person's public key. The scripts refuse a private key there. Lose the
   private key and the backups can't be read.
2. **The alert webhook URL** (optional, off when missing), one line in
   `/etc/mhai-ops/alert-webhook-url`, mode 600: a Slack or Mattermost
   incoming webhook, an email relay that takes `{"text": ...}`, or
   `https://api.telegram.org/bot<token>/sendMessage?chat_id=<id>`.
   Alerts carry check names and numbers only, never patient data.
3. **The offsite bucket** (optional, off by default),
   `/etc/mhai-ops/offsite.env`, mode 600: `OFFSITE_ENDPOINT`,
   `OFFSITE_BUCKET`, `OFFSITE_PREFIX`, `OFFSITE_REGION`,
   `OFFSITE_ACCESS_KEY_ID`, `OFFSITE_SECRET_ACCESS_KEY` (`KEY=value` lines,
   no quotes). Any S3-compatible store; the copy runs in
   `rclone/rclone`. Give the key write access to that prefix only (no
   delete), and set a lifecycle rule on the bucket (e.g. expire after 35
   days): the script doesn't rotate offsite. The files are already
   encrypted.

### Switch it on (once)

```bash
sudo apt-get install -y age
sudo install -d -m 700 /etc/mhai-ops
sudo nano /etc/mhai-ops/backup-recipients.txt     # the age1... public key
sudo nano /etc/mhai-ops/alert-webhook-url         # optional; then: sudo chmod 600 it
sudo bash deploy/ops/install-ops.sh               # from a copy of deploy/ at the deployed release
sudo systemctl start mhai-backup.service          # the first backup now
sudo journalctl -u mhai-backup -n 20
```

`install-ops.sh` copies the scripts to `/usr/local/lib/mhai-ops/`, writes
`/etc/mhai-ops/ops.env` from `ops/ops.env.example` once (set `HOST_LABEL`
there), and enables the two timers. Run it again after a release changes
`deploy/ops/`. `--uninstall` turns the timers off and keeps the backups.

### Weekly: the restore test

The private key is needed, so the operator runs it, from the password
manager on standard input (kept in memory, never written or printed):

```bash
<password manager CLI> | sudo /usr/local/lib/mhai-ops/restore-test.sh --identity-stdin
# or paste it at the hidden prompt:
sudo /usr/local/lib/mhai-ops/restore-test.sh --identity-stdin
```

It ends with `PASS: backup <name> restored and checked`, or `FAIL: <why>`;
either way it is in `/var/lib/mhai-ops/restore.status`, and the health
check alerts if no test has passed for 8 days. It needs about 512 MiB for a
few minutes; with the pilot pipeline running, check `free -m` first. To run
it on another machine instead, copy one backup directory there and use
`--identity <file> --backup <dir>`, and set `RESTORE_MAX_AGE_DAYS=0` on the
host.

### Restoring for real

Stop core (and the pipeline), restore into the running Postgres, start
again. The key is pasted into a shell variable, never a file:

```bash
sudo -i                                   # the backups are root's
read -rs K                                # paste the AGE-SECRET-KEY-... line (hidden), then Enter
cd /srv/mhai-backend && docker compose stop core pipeline
B=/var/backups/mhai/<backup>
age -d -i <(printf '%s\n' "$K") $B/globals.sql.age | docker compose exec -T postgres psql -U pgadmin -d postgres
age -d -i <(printf '%s\n' "$K") $B/mhai.dump.age \
  | docker compose exec -T postgres pg_restore -U pgadmin -d mhai --clean --if-exists --exit-on-error
unset K
docker compose up -d core                 # core-migrate resets the logins' passwords from .env
```

"Role already exists" errors from the first command are expected. Run the
restore test on that backup first if there is time.

### When an alert comes

`sudo journalctl -u mhai-healthcheck -n 50` names each failed check.
`sudo /usr/local/lib/mhai-ops/healthcheck.sh` runs it now. Backup failures:
`sudo journalctl -u mhai-backup -n 50` and
`sudo cat /var/lib/mhai-ops/backup.status`.

## Turn on email for the demo

Off by default. With it on, core hands invites, reset links, two-step
notices and reminders to the notifications service
(`services/notifications/README.md`), which sends them from our own mailbox
over SMTP submission (TLS always, the server's certificate always checked).
No patient data is ever in a message. It runs as two containers in the
compose profile `notifications`:

- `notifications`: on the internal networks `backend` and `mail` only, with
  its own NATS NKey user and database login (`notify_app`), the mailbox
  password as a read-only file, nothing of core's. It runs as development,
  as core does here (NATS and Postgres without TLS on this one host).
- `mail-relay`: its only way out. It forwards TCP to `NOTIFY_SMTP_HOST`
  port `NOTIFY_SMTP_PORT` and nowhere else, and holds no secret. The
  service reaches it under the SMTP host's own name (`extra_hosts`), so
  TLS runs end to end and is checked against the real name. The `mail`
  network is `192.168.231.0/29` with the relay at `.2`; if that collides
  with a network on the host, set `NOTIFY_MAIL_SUBNET` and
  `NOTIFY_MAIL_RELAY_IP` in `.env`.

Every deploy publishes the image; the instance starts the profile only when
`.env` has `NOTIFY_ENABLED=true`, and stops it otherwise. The NATS store
(`max_file_store`) is 640 MB, room for the `NOTIFY` stream.

**Before you turn it on:** the demo's admin login is public, so anyone can
then make the demo send an invite or reset email (fixed text, our From) to
any address, within the caps (30 links a minute and 500 invites a day for
the hospital, 10 messages an hour per recipient). SPF, DKIM and DMARC on
measured-health.org help those emails land (TODO.md). The instance's
security group must allow outbound TCP 587 (the default allows all).

### 1. The settings (on the instance)

Add these lines to `/srv/mhai-backend/.env` (`nano` as the deploy user; none
of them is a secret). Leave `NOTIFY_SMTP_PASSWORD` out: the service refuses
a password in the environment.

```bash
NOTIFY_CHANNELS=email
NOTIFY_SMTP_HOST=mail.measured-health.org
NOTIFY_SMTP_PORT=587
NOTIFY_SMTP_USERNAME=no-reply@measured-health.org
NOTIFY_SMTP_FROM=no-reply@measured-health.org
NOTIFY_SMTP_FROM_NAME=Measured Health
NOTIFY_SMTP_REPLY_TO=info@measured-health.org
CORE_APP_URL=https://ai.measured-health.org
CORE_DELIVERY=notify
CORE_NOTIFY_ENABLED=true
```

### 2. The mailbox password (on the instance)

Typed at a hidden prompt, written straight to a file only the service's
user (uid 65532) can read. It is never echoed, never an argument, never in
`.env` or the shell history:

```bash
read -rsp 'SMTP password: ' pw; echo
printf '%s\n' "$pw" | sudo install -m 0400 -o 65532 -g 65532 /dev/stdin /srv/mhai-backend/secrets/smtp_password
unset pw
```

To change it later, run the same lines, then
`docker compose --profile notifications restart notifications`.

### 3. Switch it on (on the instance)

```bash
bash /srv/mhai-backend/notifications/enable-notifications.sh
```

The deploy copies the script there. It makes the service's NATS key
(`secrets/nats-notifications.nk`; only the public key goes to `.env` as
`NATS_NOTIFICATIONS_NKEY`), generates `NOTIFY_DB_PASSWORD` in `.env`,
checks the password file's owner and mode (never its content), sets
`NOTIFY_ENABLED=true`, recreates NATS when its key is new, restarts core
(which makes the `notify_app` login) and starts the profile. It ends with
the service's start line, which names the SMTP server and port. Run it
again at any time; it keeps what is there.

### 4. Send a test

1. Sign in to https://ai.measured-health.org as the demo hospital admin.
2. Staff, Invite: invite an address you can read (your two-step code is
   asked). The invite should arrive within a minute from "Measured Health"
   `<no-reply@measured-health.org>`, replies going to
   info@measured-health.org, with a link on ai.measured-health.org.
3. If the invite fails (503 `no_delivery`), or nothing arrives, read the logs (kinds, ids and codes only, never an
   address, a link or the password):

   ```bash
   cd /srv/mhai-backend
   docker compose --profile notifications logs --tail=80 notifications mail-relay
   ```

   A sign-in refusal from the server means the user or password; a
   certificate error means the host name doesn't match the server's
   certificate; a connect timeout means outbound 587 is blocked.
4. Remove the test person afterwards (Staff, Remove), or leave it to the
   scheduled reset.

### Switching off

Set `NOTIFY_ENABLED=false` and `CORE_DELIVERY=log` in `.env` (and
`CORE_NOTIFY_ENABLED=false`), then run the deploy workflow (or
`docker compose up -d core` and
`docker compose --profile notifications stop notifications mail-relay`).
The keys and the password file can stay; delete
`secrets/smtp_password` with `sudo rm` if the mailbox is retired.

## Cloud models (off by default)

A hospital a platform admin has opted in can have its clinical answers
drafted by a cloud model (Anthropic, OpenAI or xAI). Only masked text and
shared guideline text leave, through the pipeline's egress gate and scan,
and only through `egress-proxy` (design
`docs/specs/2026-11-03-cloud-model-providers-design.md`; ADR 0013). **Demo
and synthetic data only** until every human gate in `TODO.md` ("Cloud
models: before any real patient data") is done. Nothing here opens that
gate.

```
pipeline ──backend (internal)──> egress-proxy ──egress──> CONNECT api.<provider>:443 only
```

The proxy (`egress-proxy/squid.conf`, Squid pinned by digest, profile
`cloud`) allows `CONNECT` to port 443 of the hosts in
`egress-proxy/allowed-hosts`, matched by name with no reverse lookup, and
refuses IP literals, other ports, other hosts and plain requests. A name
off the list is refused before any DNS lookup (no leaking data in the
names asked for), and an allowed name that resolves to a loopback, private
or link-local address is refused. It never
sees inside the TLS tunnel. Its access log holds time, method, host, port
and status only. `egress-proxy/test-egress.sh` checks all of this (A15) on
a Docker host; never run it with a key, and never in CI.

### Enabling a provider (design §15, steps 1–5)

Needs a host big enough for the pipeline (the embedding and NLI models on
CPU, weights baked into the image or mounted read-only with
`HF_HUB_OFFLINE=1`: the pipeline has no route to Hugging Face), the
provider key and its zero-data-retention reference (TODO H1, H5).

On the demo host, `pilot/enable-pilot.sh` does steps 1 to 3 for one
hospital (`pilot/README.md`). By hand:

1. **Provision.** Start the pipeline (profile `pilot`) with its own NKey,
   database login and trusted keys, `backend` only, as `compose.yml` shows,
   and copy `egress-proxy/` to `/srv/mhai-backend/egress-proxy/` (the deploy
   workflow copies only `compose.yml` and `nats/nats.conf`). Create
   `egress-proxy/allowed-hosts` from `allowed-hosts.example` with the one
   provider's host (`api.anthropic.com`, `api.openai.com`, `api.x.ai` or,
   for Gemini, `generativelanguage.googleapis.com`),
   mode 0644. Start the proxy: `docker compose --profile cloud up -d
   egress-proxy`.
2. **The key file.** Write the key to `secrets/<provider>-api-key` from a
   password manager (never on a command line, never in `.env`), mode 0400,
   owned by the worker's user; mount it read-only at
   `/run/secrets/<provider>-api-key` (the block shows it). The worker refuses
   to start on a placeholder, a short key, a key with whitespace, or a mode
   other than 0400/0440. Set, on the pipeline only:
   `INTELLIGENCE_CLOUD_PROXY=http://egress-proxy:3128` (not `HTTPS_PROXY`,
   which every library would read), `INTELLIGENCE_CLOUD_PROVIDERS=<provider>`,
   `INTELLIGENCE_CLOUD_<PROVIDER>_MODEL`, `INTELLIGENCE_CLOUD_<PROVIDER>_KEY_FILE`
   and `INTELLIGENCE_CLOUD_<PROVIDER>_ZDR_REF` (`DEMO-SYNTHETIC` only with
   `APP_ENV=demo`, logged as a warning; production refuses it). Set
   `CORE_CLOUD_PROVIDERS=<provider>` on core. Never set any `OPENAI_*`,
   `ANTHROPIC_*`, `AZURE_OPENAI_*`, `XAI_*` or `GEMINI_*` variable: the worker
   refuses to start. For Gemini also set
   `INTELLIGENCE_CLOUD_GEMINI_PAID_TIER=verified` once the check under "Gemini:
   paid tier only" is done; until then the worker **refuses to start**
   (demo included). The image must carry a CA bundle at OpenSSL's paths; the
   worker refuses to start without one.
3. **Switch the demo hospital on:** `docker compose --profile tools run --rm
   core-cli hospitals model-policy set <hospital id or short code> --provider
   <provider> --agreement-ref DEMO-SYNTHETIC [--budget <tokens>]`, or the
   platform app's hospital page (MFA and step-up). Both are audited
   (`hospital.model_policy_changed`).
4. **Check:** ask a synthetic question. The result shows `generated_by`, and
   the `query.completed` fact shows `egress=<provider>` and the outcome.
5. **Alerts:** route these to the person H5 names:
   `mhai_cloud_alerts_total` by `kind` (`auth_failed`, `unconfigured`, and
   `usage_overrun` when a reply uses more than 10% over its reservation),
   `mhai_cloud_budget_alerts_total` (a hospital at 80% of its month),
   `mhai_cloud_settle_failures_total` (a reservation core's reaper will
   charge in full) and `mhai_cloud_usage_overrun_total` (any reply over its
   reservation: core settles at most the reservation, so the excess is not
   in the ledger). The rules, in PromQL over the metrics the collector
   receives:

   | Alert | Rule | Means |
   |---|---|---|
   | `cloud_settle_failures` | `increase(mhai_cloud_settle_failures_total[15m]) > 0` | The pipeline couldn't settle a reservation (logged `cloud.settle_failed` with ids). The reaper charges it in full within the hour. Several in a row: the pipeline can't reach core's database. |
   | `cloud_reservations_reaped` | `increase(cloud_reservations_reaped_total[1h]) > 0` | Core settled a stale reservation as fully used (audited `cloud.reservation_reaped`, ids in the log): a pipeline crashed mid-call, a reserve timed out, or a settle failed. Match the ids against the pipeline's logs. |
   | `cloud_reaper_failing` | `increase(cloud_reaper_failures_total[30m]) > 0` | The reaper itself failed (logged `cloudreap: step failed` with the hospital and SQLSTATE). Until fixed, stale reservations stay reserved, which fails toward sending less. |
   | `cloud_reaper_stuck` | `increase(cloud_reaper_empty_total[1h]) > 2` | Hospitals were listed with stale reservations but the reap found none, run after run (logged with the hospital id). Once is a settle racing the reaper; again and again is a fault (a database policy or role change). |

   **The reaper** runs on core, on by default, as `mhai_cloud_reaper`:
   every 5 minutes one core copy settles every reservation older than
   `CORE_CLOUD_RESERVATION_REAP_AFTER` (`1h` by default; `30m` to `168h`)
   as fully used. It never frees budget: a reaped reservation counts as
   used, and a settle that arrives later changes nothing (no refund). Set
   `CORE_CLOUD_RESERVATION_REAPER=off` to stop it; reservations then stay
   reserved until the month ends, as before 00047.

### At the provider

- One key per provider per environment, in a project or workspace used only
  by this deployment, with the provider's own spend limit (USD 50 a month
  for the demo).
- Scope it to the inference endpoints only (no admin, files, fine-tuning or
  batch) and to this host's egress address, where the provider offers it.
- The zero-data-retention terms are signed and their reference is the
  `ZDR_REF` setting.

### Gemini: paid tier only

Google's Gemini API terms differ by tier (re-read them before switch-on).
On the **free tier**, Google may use the
prompts and replies to improve its products, and people may review them.
On the **paid tier** (a Google Cloud project with **billing enabled**), it
does not; it keeps them for a limited time for abuse monitoring only. So:

- Create the key in a Google Cloud project used only by this deployment,
  with a **billing account linked and enabled**, never in a free-tier
  project. In Google AI Studio the key's project must show the paid tier.
- Check it, record the check (who, when, the project) as the agreement
  reference (`INTELLIGENCE_CLOUD_GEMINI_ZDR_REF`, the `AGR-`/`DPA-` code),
  and only then set `INTELLIGENCE_CLOUD_GEMINI_PAID_TIER=verified`. Unset or
  `unverified`, the worker **refuses to start** with gemini enabled, in
  production and in demo alike (the pilot runs on demo, with a real
  hospital). Only `APP_ENV=development` starts without it, with a warning;
  any other value is refused everywhere.
- Re-check after any billing change: a project whose billing lapses goes
  back to free-tier terms.
- Gemini has no per-reply retention header (xAI's
  `x-zero-data-retention`), so this project check is the only control on
  retention. Set the project's own spend cap too.

### Rotating a key (every 90 days, and at once on suspicion)

1. Create the new key at the provider (same scope and limit).
2. Write it over `secrets/<provider>-api-key` (mode 0400, the worker's
   user), from a password manager.
3. Restart the pipeline (rolling where there is more than one): it reads the
   key only at start.
4. Revoke the old key at the provider, and check that answers still show
   `ok` (not `auth_failed`).

### Switching off

1. Set the hospital's policy to none: `hospitals model-policy set <hospital>
   --provider none` (or the platform app). It takes effect at the next claim;
   a question already queued as cloud runs local.
2. Remove `INTELLIGENCE_CLOUD_*` from the pipeline and the provider from
   `CORE_CLOUD_PROVIDERS`, delete the key file, and restart both.
3. Remove the host from `egress-proxy/allowed-hosts` and restart the proxy,
   or stop it: `docker compose --profile cloud stop egress-proxy`.
4. Revoke the key at the provider.

## Own answer model (off by default)

The team's own answer model (own answer model plan,
`docs/plans/2026-11-13-own-answer-model.md`) runs on the local route as two
services under the profile `ownmodel`, next to the pilot pipeline:

| Service | What it does | Networks | Mounts |
|---|---|---|---|
| `model-fetch` | one-shot: copies what `models.lock` pins from the model store into the `modelmirror` volume, as the store's reader | `model-store` only (its own way out) | `modelmirror` (rw), `./models` (ro), the reader's two credential files (ro) |
| `llama` | llama.cpp's `llama-server` at the pinned commit (`llama/Dockerfile`), serving the verified GGUF under the alias `<name>@<version>#<sha12>` | `model` only (internal, shared with the pipeline) | `modelcache` (ro) |
| `pipeline` | verifies the pins offline from the mirror into `modelcache`, writes `active.json`, checks the sidecar's alias before every question | `backend`, `model` | `modelcache` (rw), `modelmirror` (ro), `./models` (ro) |

Fail closed: no verified pin, an `active.json` that doesn't match, a GGUF
whose hash changed (the sidecar refuses to start), a sidecar that is down or
serves another alias: every local question escalates with the outcome
`local_unverified`. There is no fallback to Ollama, the cloud or another
version. In production the plain Ollama route is refused unless
`INTELLIGENCE_LOCAL_UNPINNED=on` (never set it on the pilot host).

**Settings** (`.env`, all empty or defaulted):

| Variable | Default | What |
|---|---|---|
| `PILOT_MODEL_ROLES` | empty (none) | `cca` to load the pinned answer model |
| `PILOT_LOCAL_MODEL_MEM` | `4g` | the sidecar's memory cap (and swap) |
| `PILOT_LOCAL_MODEL_THREADS` | `2` | llama-server threads, and the sidecar's CPUs |
| `PILOT_LOCAL_MODEL_CTX` | `4096` | context size |
| `PILOT_MODEL_STORE_ENDPOINT`, `PILOT_MODEL_STORE_BUCKET`, `PILOT_MODEL_STORE_REGION` | empty, empty, `us-east-1` | the store, for `model-fetch` only |

Files on the host: `models/models.lock` and `models/trust-policy.json`
(copied from the repository at the deployed commit), and
`secrets/model-store-reader-access-key` and
`secrets/model-store-reader-secret-key` (mode 0400, owned by uid 1000).
The `llama` image is built on the host from `llama/` (copy the folder next
to `compose.yml`; `docker compose --profile ownmodel build llama`).

**Sizing** (CPU, Q4_K_M; set `PILOT_LOCAL_MODEL_MEM` to the sidecar column
plus 0.5 GB):

| Model size | GGUF on disk | Sidecar memory (ctx 4096) | Speed on 2 arm64 cores | Fit |
|---|---|---|---|---|
| 1-3B | 1-2 GB | about 2-3 GB | tens of seconds per answer | yes, if the host has the RAM |
| 7-8B | about 4.5 GB | about 5-6 GB | minutes per answer | a larger host or a GPU only |

The demo host has about 3.7 GiB in all, shared with MongoDB, the EMR and the
web demo, and the pilot pipeline already takes up to 1600 MiB: even a 1-3B
model does not fit beside them. Resize the instance (t4g.large, 8 GiB, fits
a 3B model) before turning the profile on.

**The switch** (pinning a passing `cca`, `enable-ownmodel.sh` with
`model-fetch`, the on-host evaluation `eval-local.sh`, `--provider none`,
and back) is the runbook in `pilot/README.md`, "Switch a hospital to your
own model".

`llama/test-entrypoint.sh` tests the entrypoint without Docker;
`llama/test-llama.sh` builds the image and checks, with a tiny public test
GGUF, that the sidecar serves exactly the verified alias, reaches nothing
outside its network, and refuses a GGUF with one flipped byte.

## Our own model on the GPU (off by default)

Our own fine-tuned answer model, served on the GPU server's NVIDIA card by
the pipeline's deploy, for the hospitals whose model policy says so. It is
**off for every hospital** until a platform admin switches one on, and the
service runs only when the host's `.env` turns it on.

| Piece | What it does |
|---|---|
| `gpu-model` (compose profile `gpu-model`) | llama.cpp's official `server-cuda` image, release `b12627`, pinned by digest; serves one GGUF from the host under the alias `<name>@<version>#<sha12>`. Its entrypoint (`gpu-model/entrypoint.sh`) re-hashes the file first and refuses to start on any mismatch. Read-only root, no capabilities, no new privileges, uid 1000, memory cap, health check (`/health`). On the internal `gpu-model` network only (shared with the pipeline; no route out, no published port). The GPU through NVIDIA's CDI: `devices: [nvidia.com/gpu=all]`, the compose form of `docker run --device nvidia.com/gpu=all` (needs Docker Engine 25+ with the CDI spec, as on the GPU server; `deploy.resources.reservations.devices` with `driver: nvidia` would need the NVIDIA runtime registered instead) |
| `gpu-model/gpu-model.sh` | run by the deploy workflow on every deploy, after core is healthy: with `GPU_MODEL_ENABLED=true` it checks the settings, hashes `GPU_MODEL_FILE` against `GPU_MODEL_SHA256`, starts `gpu-model`, waits until it is healthy and serves exactly the expected alias, then brings a running pipeline up to date. Any failure stops `gpu-model` and fails the deploy. Without `true`, it stops `gpu-model` |
| `pipeline` | sends a question to `gpu-model` only when the question's route is `own` (core sets it from the hospital's policy), after checking the served alias; it asks in the model's own prompt format (`GPU_MODEL_PROFILE`). Retrieval, NLI, the HGA and the escalation guard are the local route's, unchanged |

Fail closed: a file that is missing, a link, outside `/srv/mhai-models` or
whose hash differs is never served; a service that is down or serves another
alias makes every `own` question escalate with the outcome
`local_unverified`. There is no fallback to the local route or the cloud.

**Settings** (`/srv/mhai-backend/.env`; none holds a secret):

| Variable | Example | What |
|---|---|---|
| `GPU_MODEL_ENABLED` | `true` | the switch; anything else is off |
| `GPU_MODEL_FILE` | `/srv/mhai-models/cca-v1/model-q4_k_m.gguf` | the GGUF, under `/srv/mhai-models`, a regular file (not a link), readable by uid 1000 |
| `GPU_MODEL_SHA256` | `896b75ef…4007` (64 hex) | the GGUF's SHA-256 (`sha256sum`) |
| `GPU_MODEL_NAME`, `GPU_MODEL_VERSION` | `gemma4-e4b-cca`, `1.0.0` | the alias's name and version (`a-z 0-9 -`; `MAJOR.MINOR.PATCH`) |
| `GPU_MODEL_PROFILE` | `gemma4-e4b-cca-v1` | the answer profile: how the model is asked and read (`default` or `gemma4-e4b-cca-v1`, `services/intelligence/intelligence/agents/answer_profile.py`) |
| `GPU_MODEL_MEM`, `GPU_MODEL_THREADS`, `GPU_MODEL_CTX`, `GPU_MODEL_GPU_LAYERS` | `8g`, `4`, `8192`, `999` | the container's memory cap, CPU threads, context, layers on the GPU (the defaults) |

**The first model** (`gemma4-e4b-cca-v1`; [intake](../docs/models/intake-2026-11-14-gemma4-e4b-cca-v1.md)):
Gemma 4 E4B-it (revision `4ecc2ba63bcac23ba4a767fc87025b1450f84c9c`) with
the researcher's QLoRA adapter merged, `q4_k_m`, converted with llama.cpp
`b1b770d9` (release `b12359`; no `server-cuda` image was published for it, so
the service runs `b12627`, the image the model was tried with on the host):
`/srv/mhai-models/cca-v1/model-q4_k_m.gguf`, 5,302,268,608 bytes, SHA-256
`896b75ef96037a7365126ba7f2161e64a3657ee9ea141e6a9f996b045ded4007`. The
researcher's own export of the same model is the same size with SHA-256
`8b2a145151b7509a6bba355909e1a791f0e89f3357bcdf1beec8f00ba49046de`; only
the file whose hash is in `.env` is served. **It failed our
`clinical_answer` gate** (escalation recall 0.056, grounded pass rate 0):
the product owner chose to serve it so the path works end to end, for a
hospital the policy names, while the researcher retrains. No gate or check
was relaxed for it.

### Place a new GGUF

`gpu-model/swap-model.sh` does all of this, with a shadow test first
("Swapping our own model" below). By hand:

1. Copy it to the GPU server under `/srv/mhai-models/<run>/` (for example
   `/srv/mhai-models/cca-v2/model-q4_k_m.gguf`), owned by the deploy user,
   mode 0644 (the folder 0755), never a link.
2. `sha256sum /srv/mhai-models/<run>/model-q4_k_m.gguf`, and check it
   against the hash the export recorded.
3. In `.env`: `GPU_MODEL_FILE`, `GPU_MODEL_SHA256`, a new `GPU_MODEL_VERSION`
   (the alias changes with the hash anyway), and `GPU_MODEL_PROFILE` if the
   model needs another answer profile (a new profile is a code change).

### Turn it on

1. Put the settings above in `/srv/mhai-backend/.env` with
   `GPU_MODEL_ENABLED=true`.
2. Run "Deploy backend demo" (or wait for the next merge). The deploy prints
   `gpu-model: serving <alias>`; a hash mismatch or a service that doesn't
   come up fails it, with the reason.
3. The pipeline worker must run (profile `pilot`): only it asks the model.
   With no cloud provider, see "Answering with our own model only" below;
   with `PIPELINE_ENABLED=true` every deploy starts or refreshes it.

By hand on the host (the same as the deploy does):

```bash
cd /srv/mhai-backend
bash gpu-model/gpu-model.sh
docker compose --profile gpu-model ps gpu-model
docker compose --profile gpu-model logs --tail=20 gpu-model
```

### Switch a hospital to it

Only on a host that allows it: `CORE_OWN_MODEL_ALLOWED=true` in `.env` (read
by core and `core-cli`; `enable-pilot.sh --provider none` writes it). Without
it, `--own-model on` is refused and the API answers 422; switching off is
always allowed.

The hospital's model policy (`own_model`, off by default). It goes with no
cloud provider (one route per hospital): setting a provider later switches
it off.

```bash
cd /srv/mhai-backend
docker compose --profile tools run --rm -T core-cli hospitals model-policy set <short code> --provider none --own-model on
# back to the local models:
docker compose --profile tools run --rm -T core-cli hospitals model-policy set <short code> --provider none --own-model off
```

or `PUT /platform/hospitals/{id}/model-policy` with `"cloud_provider": null,
"own_model": true` (a platform admin, step-up). Each change is audited
(`hospital.model_policy_changed`, with `own_model` and `previous_own_model`).
Questions already queued keep the route they were asked with only while the
policy still says so; the worker checks it again when it takes the question.
An answer from it reads `generated_by.route` `local` (our own hardware) and
`model_versions.cca` the alias.

### Switch it off

Per hospital: `--own-model off` (above). For the whole host: set
`GPU_MODEL_ENABLED=false` (or remove it) and deploy: the service is stopped,
and every `own` question escalates until a hospital is switched back.

### Replacing the hand-started container

Before this service, the model ran by hand as `mhai-llm-gpu` on
`127.0.0.1:8095`. Once the deploy prints `gpu-model: serving <alias>`, remove
it: `docker rm -f mhai-llm-gpu`. Nothing in this stack used it.

`gpu-model/test-entrypoint.sh` and `gpu-model/test-gpu-model.sh` test both
scripts without Docker (CI). `TestGPUModelService` checks the compose
service's hardening.

## Answering with our own model only (no cloud provider)

On the live site (`SITE_MODE=live`) there are no sample answers, so nothing is
answered until the `pipeline` worker runs. It can run with **no cloud provider
and no key**: our own model on the GPU is its only answering route.

| Route | On this host | A hospital whose policy names it |
|---|---|---|
| `own` | `gpu-model` serving the expected alias | answered by our own model, verified by the local route's retrieval, NLI, HGA and escalation guard |
| `cloud:<provider>` | none (no `PILOT_CLOUD_PROVIDER`, no key, no egress proxy) | every question escalates (`unconfigured`); nothing is sent |
| `local` | no pinned `cca` unless `PILOT_MODEL_ROLES` | every question escalates (`local_unverified`) |

Fail closed: no route falls back to another. The cloud path stays: run
`enable-pilot.sh --provider anthropic|gemini` later to add it (pilot/README.md).

**What the worker needs, and who makes it**

| Need | Where | Made by |
|---|---|---|
| NATS user `pipeline` | `secrets/nats-pipeline.nk` (seed, 0400, uid of the image), `NATS_PIPELINE_NKEY` in `.env` | `enable-pilot.sh` (inside nats-box, no network) |
| Database login `intelligence_app` | `INTELLIGENCE_DB_PASSWORD` in `.env`, the same in `secrets/intelligence-db-password` (0400) | `enable-pilot.sh`; core-migrate makes the login |
| Request-token trusted keys | volume `mhai-backend-trustedkeys` | `setup-host.sh` and every deploy (`core-request-key`) |
| Cloud key mount | `secrets/pilot-cloud-api-key`: an **empty placeholder** (0400) with no provider, so Docker never makes a folder there; the worker reads it only for an enabled provider | `enable-pilot.sh --provider none` |
| Embedding and NLI weights | **inside the pipeline image** (`ghcr.io/measured-health/mhai-multiagent-pipeline-pilot`, target `pilot`): pinned by repository and revision in `services/intelligence/Dockerfile`, loaded offline (`HF_HUB_OFFLINE=1`). Nothing to place by hand; `models.lock` pins only the `cca` role of the CPU llama sidecar (`PILOT_MODEL_ROLES`), not used here | the "Pilot pipeline image" workflow (Actions, on `main`; arch `both` or `amd64`) |
| The answer model | the GGUF under `/srv/mhai-models` (above) | the operator ("Place a new GGUF") |
| Memory | the worker's cap is 1600 MiB; the step wants 1700 MiB available before it starts | the host |

**Turn it on (once, on the host, as the deploy user)**

1. Our own model is serving: the `GPU_MODEL_*` settings with
   `GPU_MODEL_ENABLED=true`, and a deploy printed `gpu-model: serving <alias>`.
2. The pipeline image has the `own` route: run "Pilot pipeline image" on
   `main` after this release (the image is not rebuilt by the deploy).
3. From a copy of `deploy/` at the deployed release:

   ```bash
   bash deploy/pilot/enable-pilot.sh --provider none [--short-code <CODE>]
   ```

   It refuses unless `GPU_MODEL_ENABLED=true`, and while a cloud provider is
   set. It checks the image has the `own` route and core knows the
   allow-list; makes the NKey, the database password and the empty key
   placeholder; writes `PIPELINE_ENABLED=true`, `CORE_OWN_MODEL_ALLOWED=true`,
   `PIPELINE_IMAGE` and `COMPOSE_PROFILES=pilot` (removing any `PILOT_*`
   cloud setting; `CORE_CLOUD_PROVIDERS` is left as it is); recreates nats
   if its key is new and core if the allow-list changed; starts the
   pipeline. With `--short-code` it switches that hospital to our own model
   (`--hospital-name` too, to create it). It takes no key, `--agreement-ref`,
   `--zdr-ref`, `--model` or `--budget`.
4. Switch hospitals on ("Switch a hospital to it", above).

**Every deploy** (`pilot/pipeline.sh`, after `gpu-model/gpu-model.sh`): with
`PIPELINE_ENABLED=true` it checks the files and settings above, pulls
`PIPELINE_IMAGE` (the copy on the host if the pull fails), starts or refreshes
the pipeline (and the egress proxy when a provider is set), stops the demo's
`sample-ai` if it runs, and waits for the worker's start line. Anything
missing, no answering route, too little memory or a worker that doesn't start
fails the deploy, naming what to run. Without `PIPELINE_ENABLED=true` it
touches nothing (a pipeline started by hand keeps running).

| Variable (`.env`) | Value | What |
|---|---|---|
| `PIPELINE_ENABLED` | `true` | the deploy starts and refreshes the pipeline |
| `CORE_OWN_MODEL_ALLOWED` | `true` | a hospital may be switched to our own model (default `false`) |
| `PIPELINE_IMAGE` | the pilot image | optional; the default is the `latest` tag |

**Switching off:** `PIPELINE_ENABLED=false` and
`docker compose --profile pilot stop pipeline`; `CORE_OWN_MODEL_ALLOWED=false`
and a deploy stops new switches (hospitals already on stay on and escalate
once the model is gone; switch them off).

`pilot/test-pipeline.sh` tests the deploy step and `enable-pilot.sh`'s
arguments without Docker (CI);
`tests/boundary/test_own_model_only.py` starts the worker's settings from
this compose file with no provider.

## Swapping our own model

`gpu-model/swap-model.sh` puts a new model on the GPU in one command, with the
checks, a shadow test against the `clinical_answer` gate and a rollback. Run
it on the GPU server, as the deploy user, in `/srv/mhai-backend` (the deploy
ships it, its builder and its policy files, `gpu-model/lock/`). The
pipeline image (`PIPELINE_IMAGE`) must be one built after this script
(it holds `mhai-models swap`).

```bash
cd /srv/mhai-backend
# the researcher's adapter folder (docs/models/handing-over-a-model.md):
bash gpu-model/swap-model.sh --adapter /srv/mhai-models/incoming/cca-v2 \
  --name gemma4-e4b-cca --version 2.0.0 --profile gemma4-e4b-cca-v1
# or a GGUF already built:
bash gpu-model/swap-model.sh --gguf /srv/mhai-models/incoming/model.gguf \
  --name gemma4-e4b-cca --version 2.0.1 --profile gemma4-e4b-cca-v1
# build, check and shadow-test only:
bash gpu-model/swap-model.sh --adapter DIR --name ... --version ... --profile ... --check-only
# back to the model served before the last swap (again to undo):
bash gpu-model/swap-model.sh --rollback
```

| Step | What happens |
|---|---|
| Checks | the name, a **new** version (no folder for it, never swapped to, above the served one for the same name), the profile, one swap at a time (a lock in `/srv/mhai-models`) |
| Adapter | copied, then read as data only (`mhai-models swap check-adapter`, no network): only adapter, tokenizer, template, training manifest and README files (no pickle, no code, no link, no folder); a LoRA adapter with no remote-code key; its base an allowed one, at its pinned revision (`google/gemma-4-E4B-it` at `4ecc2ba6…`, Apache-2.0, `gemma4`; `intelligence/modelstore/swap.py`, changed by PR only); its `training_manifest.json` a `cca` run on that base, the licence in `models/licences.json`, the datasets in `models/approved-datasets.json` |
| Build | `gpu-model/builder/` (pinned: base image by digest, llama.cpp `b1b770d9`, CPU torch and exact package versions). The base is downloaded once into `/srv/mhai-models/.cache/hf` (no token; the only step with a network). The merge (PEFT `merge_and_unload`, bfloat16, CPU; `trust_remote_code` never set) and the conversion (f16 GGUF, then `Q4_K_M`) run with **no network and no secret**, capped at `GPU_MODEL_BUILD_MEM` (32g) and `GPU_MODEL_BUILD_CPUS` (4); it needs `GPU_MODEL_BUILD_MIN_FREE_GB` (60) free. Output: `/srv/mhai-models/<name>/<version>/model-q4_k_m.gguf` and `manifest.json` (SHA-256, sizes, tool versions, base, adapter file hashes), read-only. A `--gguf` is copied to `<name>/<version>/model.gguf` |
| GGUF checks | promotion's GGUF header checks, an allowed architecture (the base's for an adapter), the adapter's chat template byte for byte |
| Shadow test | a second gpu-model (`mhai-gpu-model-shadow`: the same pinned image, entrypoint and hardening) serves the candidate on an internal network of its own, as `gpu-model`; the live one keeps serving. The GPU's free memory must cover the file plus `GPU_MODEL_SHADOW_HEADROOM_MIB` (2048). The gate (`mhai-models swap eval-shadow`) asks every question through the own route's client, in the candidate's answer profile: the locked split when `models/testsets.lock` names one, else the **dev split, said loudly and logged as `dev`** |
| Switch | only on a pass. `.env`'s `GPU_MODEL_*` are kept as `GPU_MODEL_PREVIOUS_*`, the new ones written, and `gpu-model/gpu-model.sh` re-hashes, restarts and waits for the new alias. If it doesn't come up, the old `.env` is put back and served again. Hospitals' `own_model` switches are never touched |
| Log | one JSON line per swap, rollback or refusal in `/srv/mhai-models/swaps.log`: UTC time, operator, from and to (alias and SHA-256), the gate's verdict and metrics, the split, forced or not |

A gate that fails refuses the switch and keeps the model's folder for the
record (the next try is a new version). `--force-without-gate` switches
anyway, only after you type the version again; it is logged as
`"forced":true`. Use it only on the product owner's decision, as for the
first model.

`gpu-model/test-swap-model.sh` (a fake docker, the real `gpu-model.sh`),
`gpu-model/builder/test_merge.py` and `tests/modelstore/test_swap.py` run in
CI. Not yet run on the GPU server: the builder image, the merge and the
shadow on the L4.

## Intelligence on its own server (split mode, off by default)

The pipeline worker, the llama sidecar, `model-fetch` and the egress proxy can
run on an **intelligence host** (GPU optional), with core, Postgres, NATS and
the web apps on this **app host**, in one private network. NATS and Postgres
then have TLS from our own private CA. The design, threat model, firewall
rules, sizing and runbook are in
[docs/deploy/remote-intelligence.md](../docs/deploy/remote-intelligence.md).
**Never on the demo host:** the demo stays single-host, with TLS off.

| File | What it is |
|---|---|
| `tls/make-ca.sh`, `tls/issue.sh` | the private CA and its certificates (keys 0600, never printed) |
| `compose.split.yml` | the app host's override: `nats/tls.conf` (mutual TLS, `handshake_first`), Postgres TLS with `postgres/pg_hba.conf`, core's `NATS_TLS_*`, both published on the private address only |
| `postgres/pg_hba.split.conf`, `postgres/render-hba.sh` | `intelligence_app` over TLS from the intelligence host only, nothing else from it |
| `enable-split.sh` | the app host: issues and places the certificates, writes `pg_hba.conf` and `.env` (`COMPOSE_FILE=compose.yml:compose.split.yml`, the switch), recreates postgres, nats and core, prints the firewall rules |
| `intelligence-host/compose.yml`, `compose.gpu.yml`, `app-link.cfg` | the intelligence host: the worker on internal networks only, `app-link` (a TCP relay to the app host's 4222 and 5432) its only way there |
| `intelligence-host/enable-intelligence-host.sh`, `put-secret.sh` | the intelligence host: `init` (its NKey and TLS key, made there), `up` (the database password on stdin, the certificate checked, start) |

Tests: `tls/test-tls.sh`, `tls/test-split-lib.sh`, and
`intelligence-host/test-split.sh` (two hosts stood in for by Docker networks;
CI's boundary-smoke job).

## Known limits

- **No TLS inside the demo instance.** Postgres (`sslmode=disable`) and NATS
  run without TLS, on Docker networks and loopback only. The pilot pipeline
  runs on this host with `INTELLIGENCE_SINGLE_HOST=on`, which the worker
  accepts only for the Docker service names `nats` and `postgres`. A worker
  on another host needs the split mode above (TLS for NATS and Postgres);
  core's own Postgres link stays plaintext on the internal network even
  then.
- **No JetStream encryption at rest** on the demo (dev compose has it,
  `NATS_JS_KEY`): turning it on needs the store converted or recreated.
- **NATS is on a non-internal network** (`edge`), because Docker publishes
  ports only from one; that gives the NATS container a route out. It needs
  none.
- **The migrations run as the superuser.** A non-superuser owner with
  CREATEROLE would be better, but on Postgres 16 `core migrate up` then fails
  on its second run: `EnsureLogin` sends `ALTER ROLE ... NOBYPASSRLS
  NOREPLICATION`, which only a role holding those attributes may send. Fix
  `EnsureLogin` first, then switch.
- **Single instance.** The database volume is on the instance's disk.
  Backups and alerts are opt-in ("Operations": `ops/install-ops.sh`), and
  until they're on there are none; the backups stay on the same disk unless
  the offsite bucket is set. Run `sudo systemctl start mhai-backup.service`
  (or, before install, `docker compose exec postgres pg_dump -U pgadmin mhai`)
  before anything risky.
- **The gateway permissions are copied** from `services/nats/nats.conf` into
  `nats/nats.conf`; change both together.
- **Config changes in `compose.yml` and `nats/nats.conf` reach the instance
  with the next deploy**, not before; `setup-host.sh` copies them too.
