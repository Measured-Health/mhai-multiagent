# The pilot: cloud answers for one real hospital on the demo host

The product owner's decision of 12 Nov 2026: one willing hospital asks
clinical questions on the demo deployment (demo.measured-health.org), with
the answers drafted by a cloud model (Anthropic, or Gemini once its adapter
is merged). It is supervised, and clinicians stay in the loop. The formal
privacy, legal and clinical sign-offs are deferred, and that is recorded in
PR #134 (README). Nothing in this folder records them as done.

What stays exactly as it is: PII masking before anything leaves, the egress
gate and scan on the exact request bytes, the egress proxy (one API host,
port 443), tenancy (core's token and row-level security), the budget, and
failing closed. Any cloud failure escalates (a red "needs review" result,
no score). There is no fallback to another model.

```
clinical app ─BFF─> NATS ─> core ── JOBS ──> pipeline ──backend──> egress-proxy ──> api.<provider>:443
                                           (masking, scan,          (one host only)
                                            retrieval and NLI
                                            verification stay local)
```

## What runs, and what it costs the host

| Service | Profile | Memory cap | Notes |
|---|---|---|---|
| `pipeline` | `pilot` | 1600 MiB (measured: about 1 GiB with both models loaded) | `APP_ENV=production`, one question at a time, weights baked in the image, offline |
| `egress-proxy` | `cloud` | 128 MiB | allows `CONNECT <provider host>:443` only |

Both are off in the default deploy: `docker compose up` without the
profiles starts neither, and the deploy workflow's
`up -d --remove-orphans core` leaves them running (checked). The script
refuses to start them with less than 1800 MiB available. If the host
(3.7 GiB, shared with MongoDB, the EMR and the web demo) doesn't have it,
resize the instance (t4g.large, 8 GiB) first; never lower the caps of the
EMR's containers.

The pipeline runs as production with one exception, `INTELLIGENCE_SINGLE_HOST=on`:
NATS and Postgres without TLS, which the demo host doesn't have yet
(`deploy/README.md`, "Known limits"). The worker accepts it only for Docker
service names on its own host (`nats`, `postgres`: no dot, no address, not
localhost), still needs its NKey and a file password, and logs a warning at
every start. TLS is required before any worker runs off this host.

## Before you start (once)

1. **The release with the pilot is deployed** (this PR merged, the "Deploy
   backend demo" workflow green): it brings `compose.yml` with the
   `pipeline` service and `nats/nats.conf` with the `pipeline` NATS user.
2. **The pipeline image.** Run the "Pilot pipeline image" workflow (Actions,
   on `main`). It builds `ghcr.io/measured-health/mhai-multiagent-pipeline-pilot`
   for arm64 with the embedding and NLI weights pinned and baked in. Then,
   on the host, log in once with a GitHub token that has `read:packages`
   only, from stdin:
   `docker login ghcr.io -u <github user> --password-stdin`.
   Rebuild it when a release changes the worker's contract with core.
3. **The provider's API key**, in a workspace used only by this
   deployment, with a spend limit (USD 50 a month), restricted to the host's
   egress address where the provider offers it (TODO H1).
4. **The zero-data-retention reference** from the provider, for
   `--zdr-ref`. If the provider has not confirmed ZDR yet, the worker still
   needs a value: use `PILOT-ZDR-PENDING` and know that, until ZDR is in
   place, the provider's default retention applies to what is sent (masked
   question, two context fields, shared guideline text). That is one of
   the deferred sign-offs; record it in PR #134.
5. **The agreement reference**, for `--agreement-ref`. Core accepts only
   `AGR-YYYY-NN…` or `DPA-YYYY-NN…` (2 to 8 digits), so a reference can never
   hold a name. While legal is deferred, use **`AGR-2026-01`** and record in
   PR #134 that it is the pilot's placeholder, not a signed agreement. It
   goes into the audit chain (`hospital.model_policy_changed`); when the
   real agreement is signed, set the policy again with its reference.

## Run it

From a copy of this repo's `deploy/` folder at the deployed release, as the
deploy user, on the host:

```bash
bash deploy/pilot/enable-pilot.sh --provider anthropic \
  --hospital-name "<the hospital's name>" --short-code <CODE> \
  --agreement-ref AGR-2026-01 --zdr-ref <ZDR reference or PILOT-ZDR-PENDING> \
  [--time-zone Africa/Lagos] [--model claude-opus-5-5] [--budget 2000000] \
  [--seed-reference-guidelines]
```

It asks for the key with a hidden prompt (or reads one line piped from a
password manager). For Gemini: `--provider gemini --model <model id>
--gemini-paid-tier verified`. Pass the last flag only once you have checked the
key's Google project is paid and billing-enabled (free-tier prompts may be
used by Google); without it the script stops, and the pipeline would refuse
to start anyway. It also refuses until both the pipeline image and core know
`gemini`, before changing anything.

It is safe to run again: every step checks first and keeps what is there.
Each step, in order:

1. checks its arguments, the deployed release, the free memory, the image
   (its architecture, its non-root user, and that it knows the provider and
   its API host) and that core accepts the provider (a dry CLI call that
   writes nothing);
2. makes the pipeline's NATS NKey (seed in `secrets/nats-pipeline.nk`,
   public key `NATS_PIPELINE_NKEY` in `.env`), its database password
   (`INTELLIGENCE_DB_PASSWORD` in `.env` for core-migrate, the same in
   `secrets/intelligence-db-password`) and the key file
   `secrets/pilot-cloud-api-key`, each mode 0400 and owned by the image's
   user, written inside a container with no network; nothing secret is an
   argument or printed;
3. writes the settings to `.env`: `PILOT_CLOUD_PROVIDER`, the provider's
   `PILOT_<P>_MODEL`, `_KEY_FILE`, `_ZDR_REF`, `PIPELINE_IMAGE`,
   `CORE_CLOUD_PROVIDERS` and `COMPOSE_PROFILES=cloud,pilot`;
4. copies `egress-proxy/squid.conf` and writes `egress-proxy/allowed-hosts`
   with the provider's one host;
5. recreates `nats` if its pipeline key is new, runs `core-migrate` (which
   makes the `intelligence_app` login) and recreates `core` if the providers
   changed, then waits for `readyz`;
6. starts `egress-proxy` and `pipeline` and waits for the worker's start
   line;
7. creates the hospital (`core hospitals create`, a real hospital, never
   `--demo`, with MFA required for clinicians) and activates it;
8. sets its model policy (`core hospitals model-policy set`, audited
   `hospital.model_policy_changed`), unless it is already the same;
9. with `--seed-reference-guidelines`, adds the six shared reference
   guidelines (`services/intelligence/init-db/seed_kb.py`) if missing.

It never touches the EMR containers, the web demo or nginx.

## Then: the users

The script doesn't create people. For each clinician and the hospital admin
(their real email and name are personal data: type them on the host, never
in a ticket or chat):

```bash
cd /srv/mhai-backend
docker compose --profile tools run --rm core-cli users create \
  --email <email> --name "<name>" --roles clinician --hospital <CODE>   # prompts for the password
```

## Backups and alerts (before the first real question)

Real patient data needs the operations baseline on (deploy/README.md,
"Operations"). Once, before the hospital's first clinician signs in:

1. Make the backup key pair on your own machine (`age-keygen`), keep it in
   the password manager, and put only the public key (`age1...`) in
   `/etc/mhai-ops/backup-recipients.txt` on the host.
2. Put the alert webhook URL in `/etc/mhai-ops/alert-webhook-url`
   (mode 600), and optionally the offsite bucket in
   `/etc/mhai-ops/offsite.env`.
3. `sudo bash deploy/ops/install-ops.sh`, then
   `sudo systemctl start mhai-backup.service`. With the pilot on, the health
   check also watches `pipeline` and `egress-proxy`.
4. Run the first restore test and see `PASS`:
   `sudo /usr/local/lib/mhai-ops/restore-test.sh --identity-stdin` (paste the
   private key at the hidden prompt).
   Then trigger one alert to see it arrive:
   `sudo env DISK_MIN_FREE_PCT=101 /usr/local/lib/mhai-ops/healthcheck.sh`
   (and run it once more without the variable for the "recovered" message).

Every week: the restore test (step 4). Every day: glance at the alert
channel; no news is good news only because step 4 proved alerts arrive.

## Check it

- [ ] `docker compose ps` shows `pipeline` and `egress-proxy` running, and
      `docker compose logs pipeline | grep 'worker started service=pipeline'`
      prints the line. The log also shows `INTELLIGENCE_SINGLE_HOST=on`.
- [ ] Sign in to the clinical app as a pilot clinician and ask a test
      question with no patient details, for example "What confirms malaria
      before treatment?". The answer is marked **"Drafted by Anthropic
      (cloud AI)"**. (For Gemini, mhai-web needs its label first: today it
      names Anthropic, OpenAI and xAI.)
- [ ] `docker compose logs egress-proxy` shows `CONNECT` lines to
      `api.anthropic.com` port 443 only (status 200), and nothing else.
- [ ] The platform app's hospital page shows the policy, the agreement
      reference and this month's token use.

If answers escalate, the `query.completed` audit fact names the cloud
outcome (`cloud_outcome`): `auth_failed` is the key;
`unconfigured` means the pipeline has no provider (check
`PILOT_CLOUD_PROVIDER`); `egress_blocked` means the scan refused (by
design: no identifiers in questions); `insufficient` means the shared
guidelines don't cover the question (see "Known limits").

## Switching off

Fast, in this order:

1. `docker compose --profile tools run --rm core-cli hospitals model-policy set <CODE> --provider none`
   (or the platform app). New questions run local from the next claim; with
   no local model on this host they escalate.
2. `docker compose stop pipeline egress-proxy`.
3. Revoke the key at the provider, then delete `secrets/pilot-cloud-api-key`.
4. To remove it all: delete the `PILOT_*`, `CORE_CLOUD_PROVIDERS` and
   `COMPOSE_PROFILES` lines from `.env`, `docker compose up -d core`, and
   `docker compose rm -sf pipeline egress-proxy`.

Rotating the key: `--replace-key` with the new key on stdin, then
`docker compose restart pipeline`, then revoke the old key.

## Switch a hospital to your own model

Moves one hospital from the cloud to the team's own answer model (role
`cca`), and back, without editing any file but `.env`. Every hospital
stays where it is until step 8, and the cloud stays configured, so the way
back is one command. Clinicians stay in the loop: the own model's answers
are drafts, like the cloud's.

1. **Resize the host.** The demo host has about 3.7 GiB in all, and even a
   1-3B model doesn't fit next to the pilot (deploy/README.md, "Sizing").
   Stop the instance, change its type to t4g.large (8 GiB, fits a 3B
   model), start it. The scripts below refuse a host that is too small,
   before they change anything.
2. **Train and export in Colab**, following
   [docs/models/export-step.md](../../docs/models/export-step.md): role
   `cca`, a GGUF next to the safetensors, the run folder uploaded to Drive.
3. **Register the dataset** the run was trained on: its entry in
   `models/approved-datasets.json`, synthetic only, by its own PR. The
   export step refuses a dataset that isn't registered, so merge that PR
   before step 2's export.
4. **Promote** with the "Model promotion" workflow (Actions, on `main`),
   gate `answer`: first a dry run (no `expect_files`) and read its
   `evaluation.md`; then the real run with `expect_files` set to the dry
   run's files hash. It scores the GGUF itself on the locked
   `clinical_answer` split (the "Model test set" workflow builds it once,
   its line goes into `models/testsets.lock` by PR).
5. **Pin**: `mhai-models pin cca <name> <version> --gate answer` (as the
   store's reader) writes `models.lock`; open a PR with it. The "Model
   lock" workflow verifies it; a reviewer merges. Pin refuses anything
   that didn't pass on the locked split.
6. **Fetch the pinned model onto the host** (`mhai-models fetch-pinned`,
   in the `model-fetch` container). On the host, from a checkout of the
   repository at the deployed release, with the store reader's two
   credentials on stdin (access key, then secret key; hidden prompts on a
   terminal):

   ```bash
   bash deploy/pilot/enable-ownmodel.sh --store-endpoint https://<store host> \
     --store-bucket <bucket> [--mem 3g] [--threads 2]
   ```

   It checks memory and the lock, copies `models.lock`, `models/trust-policy.json`
   and `models/testsets.lock` into `/srv/mhai-backend/models`, writes the
   credentials (0400 files, never printed), sets `PILOT_MODEL_ROLES=cca`,
   `PILOT_LOCAL_MODEL_*`, `PILOT_MODEL_STORE_*` and
   `COMPOSE_PROFILES=cloud,pilot,ownmodel` in `.env`, builds the `llama`
   image, runs `model-fetch`, and starts `llama` and the pipeline until the
   pipeline logs `model.served role=cca`. It switches no hospital. Safe to
   run again; `--mem` is the sidecar column of the sizing table plus 0.5 GB.
7. **Evaluate on this host**, and go on only on PASS:

   ```bash
   bash deploy/pilot/eval-local.sh
   ```

   It checks memory, the lock and the images (the sidecar's llama.cpp is
   the gate's), brings up the `ownmodel` profile, requires the sidecar to
   serve exactly the alias pinned in `models.lock`, then scores the locked
   split through the worker's own client, one question at a time, and
   prints PASS or FAIL with the totals (the gate's thresholds, and a median
   answer time of at most 90 s: `--max-seconds`). The summary is kept in
   `/srv/mhai-backend/local-eval/`; put it in the tracker. Until a locked
   split exists it scores the dev split and says so in capitals: a dev PASS
   is not the gate. It exits non-zero on FAIL: don't switch.
8. **Switch one hospital** (audited `hospital.model_policy_changed`):

   ```bash
   cd /srv/mhai-backend
   docker compose --profile tools run --rm core-cli hospitals model-policy set <CODE> --provider none
   ```

   New questions from that hospital go to the local route from the next
   job.
9. **Watch the first answers** with the hospital's clinicians: they review
   every answer for the first days, as with the cloud. In the audit chain,
   each `query.completed` for the hospital has `model_versions` with
   `cca: <name>@<version>#<sha12>`, the pinned version. Count escalations: a few is the model being careful;
   many, or any `local_unverified`, is the next point.
10. **Switch back to the cloud**, one command; nothing else changes:

    ```bash
    docker compose --profile tools run --rm core-cli hospitals model-policy set <CODE> \
      --provider anthropic --agreement-ref AGR-2026-01
    ```

11. **`local_unverified` escalations** (the `query.completed` outcome) mean
    the worker could not show that the sidecar serves the pinned model:
    no verified pin, the sidecar down or still loading, or another alias.
    Nothing was generated; the question escalated to a clinician. What to
    do: switch the hospital back (step 10), then look at
    `docker compose logs pipeline llama` (fixed codes only:
    `model.verify_failed ... detail=`). Run `enable-ownmodel.sh` again
    (it restarts what needs it), then `eval-local.sh`, before switching
    again. Never set `INTELLIGENCE_LOCAL_UNPINNED`.

**A new model or a rollback:** pin it (step 5) or roll back
(`mhai-models rollback cca`, by PR), then on the host `enable-ownmodel.sh`
(it copies the new lock, fetches, restarts the pipeline and, if it still
serves the old alias, the sidecar) and `eval-local.sh` again. A hospital on
`--provider none` gets escalations, not answers, while the new pin loads.

`test-preflight.sh` tests the scripts' pre-flight without Docker.

## Known limits (the pilot accepts these; see PR #134)

- **The deferred sign-offs** (TODO.md, "Cloud models: before any real
  patient data"): the masker upgrade and its recall bar, the legal check,
  the clinical evaluation and core's name-token hashes. In particular
  **names without a title are not masked** (ruling C8): "Adaeze Okafor has
  chest pain" would be sent. Clinicians must be told not to type patient
  names, numbers or dates into questions.
- **Shared guidelines only.** Retrieval and the cloud prompt use only the
  shared reference guidelines; there is no hospital or patient context yet.
  The repo has six reference guidelines (`--seed-reference-guidelines`),
  which cover few questions: many answers will come back `insufficient`
  and escalate until a real guideline knowledge base is loaded.
- **No TLS inside the host** (`INTELLIGENCE_SINGLE_HOST=on`, above).
- **One question at a time**, about 1 GiB of memory, CPU only.
- **The image is rebuilt by hand** (the "Pilot pipeline image" workflow),
  not by every deploy.
