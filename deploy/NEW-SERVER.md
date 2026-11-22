# A new server for the whole stack

`deploy/new-server.sh` sets up a fresh Ubuntu server to run the backend
(this repo) and the clinical app (mhai-web) on one domain, as on the demo
host. Use it to copy the demo to another server.

```
browser ──https──> nginx (host) ──> 127.0.0.1:8090 ──> clinical app ──mhai-bff──> NATS ──> core
```

You need: an **arm64** Ubuntu server (the images are built for arm64, e.g.
AWS Graviton) with about 4 GiB of memory, a user with sudo (`ubuntu`), ports
22, 80 and 443 open, and a domain whose DNS you control.

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
  --email ops@example.org --web-repo-dir /tmp/mhai-web --demo
```

`--demo` marks the server as a public demo (it copies the live-demo override
and lists the demo's settings). It asks for the security contact name and
email at the terminal, or takes `SECURITY_CONTACT_NAME` and
`SECURITY_CONTACT_EMAIL` from the environment (deploy/README.md).

It announces each step and stops at the first error:

1. checks Ubuntu, arm64 and sudo;
2. installs Docker Engine with the compose plugin, nginx and certbot, only
   if missing;
3. adds you to the `docker` group;
4. runs `setup-host.sh` (the backend's secrets, NATS keys, volumes and the
   `mhai-bff` network, in `/srv/mhai-backend`);
5. sets up `/srv/mhai-web` with mhai-web's compose files;
6. writes the nginx site `mhai-demo` for the domain, tests it, reloads;
7. checks the DNS record;
8. gets the certificate (`certbot --nginx`, http redirects to https);
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
