# Backend demo deployment

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

1. build the core image for arm64 on GitHub (`services/core/Dockerfile`:
   static binary on distroless, non-root) and push it to
   `ghcr.io/measured-health/mhai-multiagent-core` (`latest` and `sha-<commit>`);
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
| `nats` | `nats:2.10.29-alpine`, uid 1000 | JetStream (KV buckets) on a volume; NKey users `gateway` (core) and `bff` (placeholder) | 128 MiB |
| `core-migrate` | core | one-shot each deploy: goose migrations, then the `core_app` and `core_provision` logins | 128 MiB |
| `core-keys` | core | one-shot: the access-token signing key, made once (`core keys ensure`) | 64 MiB |
| `core` | core, uid 65532 | `core serve` on 127.0.0.1:8092 | 320 MiB, CPUs 0-1 |
| `core-cli` | core | the seed CLI, run by hand (profile `tools`) | 128 MiB |

Every container has a read-only root filesystem, no Linux capabilities and
`no-new-privileges`. The intelligence service isn't deployed yet: it isn't
wired to NATS until the boundary plan lands (placeholder in `compose.yml`).

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
| `/srv/mhai-backend/.env` (mode 600) | `POSTGRES_PASSWORD`, `CORE_DB_PASSWORD`, `PROVISION_DB_PASSWORD`, `MFA_ENCRYPTION_KEY`, `BFF_SERVICE_KEYS`; the NATS public keys `NATS_GATEWAY_NKEY`, `NATS_BFF_NKEY`; and the BFF's seed `NATS_BFF_NKEY_SEED` |
| `/srv/mhai-backend/secrets/nats-gateway.nk` (mode 400, uid 65532) | the core service's NATS NKey seed, mounted into `core` only |
| volume `mhai-backend-corekeys` | the access-token signing key (Ed25519) |

Each container gets only its own secrets: `core` never sees the owner
password, `core-migrate` never sees the core secrets, and NATS sees only
public keys.

## One-time setup (product owner)

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

The frontend's BFF will reach the backend over NATS. Until Plan 3 builds
the subjects, its NATS user `bff` may sign in but publish nothing.

- **From a container** (how mhai-web runs): add the shared network to the
  service in mhai-web's `deploy/compose.yml` and connect to
  `nats://mhai-nats:4222` with the BFF seed (`NATS_BFF_NKEY_SEED` from
  `/srv/mhai-backend/.env`, given to the web container only):

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

## Public API docs (api-dev)

The docs page is public at `https://api-dev.measured-health.org/api/docs`
(product owner, 28 Oct). `nginx/mhai-api-dev.conf` serves only `/api/docs`,
its script and `/api/openapi.yaml`, GET only; every other path, the API
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

## Known limits

- **No TLS inside the instance.** Postgres (`sslmode=disable`) and NATS
  run without TLS, on Docker networks and loopback only. NATS TLS needs TLS
  settings in the core service's NATS client first (`bus.Connect` has none);
  that is a follow-up, and the BFF's connection should get it at the same
  time (Plan 3).
- **NATS is on a non-internal network** (`edge`), because Docker publishes
  ports only from one; that gives the NATS container a route out. It needs
  none.
- **The migrations run as the superuser.** A non-superuser owner with
  CREATEROLE would be better, but on Postgres 16 `core migrate up` then fails
  on its second run: `EnsureLogin` sends `ALTER ROLE ... NOBYPASSRLS
  NOREPLICATION`, which only a role holding those attributes may send. Fix
  `EnsureLogin` first, then switch.
- **Single instance, no backups.** The database volume is on the instance's
  disk. Take `docker compose exec postgres pg_dump -U pgadmin mhai` before
  anything risky.
- **The gateway permissions are copied** from `services/nats/nats.conf` into
  `nats/nats.conf`; change both together.
- **Config changes in `compose.yml` and `nats/nats.conf` reach the instance
  with the next deploy**, not before; `setup-host.sh` copies them too.
