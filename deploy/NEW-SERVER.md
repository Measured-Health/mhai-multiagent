# A new server for the whole stack

`deploy/new-server.sh` sets up a fresh Ubuntu server to run the backend
(this repo) and the clinical app (mhai-web) on one domain, as on the demo
host, and optionally the platform admin app on a second domain. Use it to
copy the demo to another server, or to move it ("Moving the demo to a new
server", below).

```
browser ──https──> nginx (host) ──> 127.0.0.1:8090 ──> clinical app ──mhai-bff──> NATS ──> core
```

You need: an Ubuntu server, **arm64 or x86_64** (every image the Deploy
workflows publish is multi-arch: linux/amd64 and linux/arm64, e.g. AWS
Graviton or a g6 instance), with about 4 GiB of memory, a user with sudo
(`ubuntu`), ports 22, 80 and 443 open, and a domain whose DNS you control.
Docker may already be installed (AWS Deep Learning AMIs ship it): it is kept.
Otherwise the script installs it from Docker's apt repository, or, on an
Ubuntu release that repository does not cover yet (26.04 at first), from
Ubuntu's own `docker.io` and `docker-compose-v2` packages. An NVIDIA GPU is
only reported: the stack runs on CPU today and nothing configures the GPU.

## 1. DNS

Add an A record for the domain (e.g. `ai.example.org`) pointing at the
server's public IP. Wait until `dig +short ai.example.org` shows it. The
script checks this before asking Let's Encrypt for a certificate, and stops
if it doesn't point at the server.

## 2. Copy the deploy folders

From your machine, with checkouts of both repositories:

```bash
scp -r mhai-multiagent/deploy ubuntu@<server>:/tmp/mhai-backend-deploy
ssh ubuntu@<server> mkdir -p /tmp/mhai-web
scp -r mhai-web/deploy        ubuntu@<server>:/tmp/mhai-web/deploy
```

## 3. Run the script

On the server:

```bash
bash /tmp/mhai-backend-deploy/new-server.sh --domain ai.example.org \
  --email ops@example.org --web-repo-dir /tmp/mhai-web --demo \
  --platform-domain platforms.example.org
```

`--platform-domain` (optional) also sets up mhai-web's platform admin app as
mhai-web's deploy/README.md ("Platform admin app") does by hand:
`/srv/mhai-web-platform` with `compose.platform.yml` and its own
`PLATFORM_SESSION_SECRET` (made once, kept), the allow list
`/etc/nginx/mhai-platform-allow.conf` (created **empty** if missing, so
everyone gets 403 until you add `allow <ip>;` lines; never overwritten), the
nginx site `mhai-platform` (the `:80` block from mhai-web, then the `:443`
block with the allow list and `deny all;` once the certificate exists), and
the certificate by webroot (Let's Encrypt's check comes from addresses not on
the list).

`--demo` marks the server as a public demo (it copies the live-demo override
and lists the demo's settings). It asks for the security contact name and
email at the terminal, or takes `SECURITY_CONTACT_NAME` and
`SECURITY_CONTACT_EMAIL` from the environment (deploy/README.md).

It announces each step and stops at the first error:

1. checks Ubuntu, the architecture (arm64 or x86_64) and sudo, and notes an
   NVIDIA GPU if there is one;
2. installs Docker Engine with compose v2, nginx and certbot, only if
   missing;
3. adds you to the `docker` group;
4. runs `setup-host.sh` (the backend's secrets, NATS keys, volumes and the
   `mhai-bff` network, in `/srv/mhai-backend`);
5. sets up `/srv/mhai-web` with mhai-web's compose files (and
   `/srv/mhai-web-platform` with `--platform-domain`);
6. writes the nginx site `mhai-demo` for the domain (and `mhai-platform`),
   tests it, reloads;
7. checks the DNS records; it stops here until they point at the server,
   with everything above in place;
8. gets the certificates (`certbot --nginx` for the demo, http redirects to
   https; `certbot certonly --webroot` for the platform app);
9. prints the server's SSH host key fingerprint and what to set in GitHub.

Run it again at any time: it keeps every file, secret, key, volume and
certificate that exists. It never prints a secret, never prunes images,
never touches containers or nginx sites it didn't make, and starts no app.
Afterwards, `rm -rf /tmp/mhai-backend-deploy /tmp/mhai-web`.

## 4. GitHub secrets

In **both** Measured-Health/mhai-multiagent and Measured-Health/mhai-web,
environment `preview`: secrets `DEPLOY_HOST`, `DEPLOY_USER`,
`DEPLOY_SSH_KEY` and `DEPLOY_HOST_FINGERPRINT` (the script prints which
value goes where), and the repository variable `DEPLOY_ENABLED=true`.
Check the fingerprint from your own machine first (deploy/README.md,
"Host key fingerprint").

The `preview` environment names one server: changing these secrets moves
the deploys from the old server to the new one.

## 5. Deploy

Run **Deploy backend demo** in mhai-multiagent first (it seeds the demo
hospital), then **Deploy demo** in mhai-web (Actions, Run workflow, branch
`main`). Open `https://<domain>`.

## 6. Optional: email

See deploy/README.md, "Turn on email".

## Moving the demo to a new server

To move `ai.measured-health.org` (and `platforms.measured-health.org`) from
the old host to a new one, without touching anything else on the old host:
it also runs containers that are not ours (the EMR stack and others). Never
`docker compose down`, prune or stop anything there beyond what the steps
below name. The demo's data is synthetic and reset every few hours, so the
new server starts with a fresh database and freshly seeded demo hospital;
nothing is copied from the old database.

1. **Set up the new server**: steps 2 and 3 above, with
   `--domain ai.measured-health.org --platform-domain platforms.measured-health.org --demo`.
   Its DNS check (step 7) stops the script while the names still point at
   the old host; everything before it is in place and kept.
2. **Email** (only if it is on, `NOTIFY_ENABLED=true` in the old host's
   `/srv/mhai-backend/.env`): copy, from the old host to the new one, the
   mailbox password file `/srv/mhai-backend/secrets/smtp_password` (mode 400,
   owner uid 65532, as deploy/README.md "Turn on email" writes it) and the
   `NOTIFY_SMTP_*`, `NOTIFY_CHANNELS` and `CORE_*` email lines of `.env`
   (`CORE_APP_URL`, `CORE_DELIVERY`, `CORE_NOTIFY_ENABLED`), never
   `NOTIFY_DB_PASSWORD` or the other secrets `setup-host.sh` made for the new
   server. Then run `notifications/enable-notifications.sh` there as the
   README says. Copy over a private channel (scp between your own sessions),
   never into a repository or a chat.
3. **Platform allow list**: copy `/etc/nginx/mhai-platform-allow.conf` from
   the old host, then `sudo nginx -t && sudo systemctl reload nginx`.
4. **DNS**: switch the A records of `ai.measured-health.org` and
   `platforms.measured-health.org` to the new server's public IP. Wait until
   `dig +short` shows the new address for both.
5. **Certificates**: run `new-server.sh` again with the same options. It
   keeps what it made, passes the DNS check and runs certbot for both names.
6. **GitHub**: in both repositories (Measured-Health/mhai-multiagent and
   Measured-Health/mhai-web), environment `preview`: set `DEPLOY_HOST` to the
   new address and `DEPLOY_HOST_FINGERPRINT` to the new host key (the script
   prints it; check it from your own machine, deploy/README.md "Host key
   fingerprint"). `DEPLOY_USER` and `DEPLOY_SSH_KEY` change only if the new
   server uses another user or key.
7. **Deploy**: run **Deploy backend demo** in mhai-multiagent, then **Deploy
   demo** in mhai-web. Check `https://ai.measured-health.org` and, from an
   allowed address, `https://platforms.measured-health.org/sign-in`.
8. **The old host**: once the new one is healthy, stop only our stacks
   there, if you want them gone: `cd /srv/mhai-web && docker compose stop`,
   `cd /srv/mhai-web-platform && docker compose -f compose.platform.yml stop`,
   `cd /srv/mhai-backend && docker compose stop`. Leave every other
   container, volume and nginx site as it is.

The sessions of the demo and the platform app do not carry over (new session
secrets): everyone signs in again.
