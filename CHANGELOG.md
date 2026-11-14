# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- Patient Analysis PR 7: clearance letters, hospital side (migration
  **00034**, OpenAPI **1.23.0**; off by default: every letter route answers
  404 while the hospital's analysis switch is off and 403
  `named_access_off` while its named-access position is `off`, which it is
  everywhere until `core analysis named-access demo` raises the demo
  hospital to `letters_only`). `GET/POST /hospital/clearances`, `GET
  …/{id}`, `GET …/{id}/pages/{n}`, `POST …/{id}/revoke|narrow` for hospital
  admins with two-step (re-read in every transaction), step-up and a
  required `Idempotency-Key` on writes. The rules are triggers on
  `clearances` and `clearance_covered` for every writer (statuses and who
  may move them, the 12-month cap, narrowing only and never to nobody,
  version up by one, and the platform side's actor, `same_person` and
  `same_side` checks, T15), and the database refuses to commit a letter
  change without its `clearance.*` audit entry. A live upload is 503
  `unavailable` with nothing stored until the documents service exists; the
  demo keeps only the file's SHA-256 and serves fixture pages drawn in code,
  each view audited (`clearance.viewed`). The nightly run ends letters after
  their hospital's last day and records reminders at 30, 7 and 1 days
  (`clearance.reminder_sent`, delivery pending). New problem codes:
  `named_access_off`, `file_too_large`, `file_type`, `end_date_too_late`,
  `not_eligible`, `not_eligible_platform`, `would_empty`. Plan departures
  29–44. Security review: outside the demo every upload is the same 503
  whatever it holds; `not_eligible_platform` is one reply with no list;
  platform admins are shown only on verified letters; eligibility
  refusals are limited to 5 a day per person, reserved before the check
  so parallel requests can't pass the limit.
- EMR integration, PR 13 (`pids`; off by default: while `CORE_EMR_ENABLED`
  is false every `patient_id` is 404 `not_found`). `POST /queries` takes an
  optional `patient_id` (OpenAPI **1.22.0**, additive), accepted only for an
  active patient of the caller's hospital (`emr_patient_in_hospital()`);
  another hospital's, an excluded or merged patient, or none is 404
  `not_found`. The checked id is the only one the job token's `pids` claim
  carries, and `RunQueryJob` gains `patient_id` (additive), which the
  worker's check 8 holds to `pids`. The sweeper, retry and dead-letter
  re-mint check the patient again. `query.submitted` gains `patient: true`
  (never the id). Migration **00033** (after EMR PR 9's 00032):
  `clinical_queries.patient_id` keyed to the hospital's patients, an
  active-patient check under a share lock, and a trigger that unlinks a
  patient from its questions when it is excluded or merged.
- EMR integration, PR 9 (retention, the exclusion purge and Usage; off by
  default). Migration **00032** (after 00031): `legal_holds` (placed and
  lifted only by the provisioning login, never deleted), durable
  `emr.purge_requests`, and the purge, release, retention and removal
  functions, with a new role `mhai_emr_maint`. An exclusion still purges at
  once, now also the patient's findings and what core's consumer recorded
  about them; under a legal hold nothing is deleted, the request is held,
  and the alerts stop naming the patient until the hold is lifted. Core's
  maintenance loop (only with `CORE_EMR_ENABLED`, one copy) releases held
  requests after a lift and purges them, lists requests held over 90 days,
  and, with `CORE_EMR_RETENTION_SWEEP` (off by default until the privacy
  lead confirms the values), runs design §14.2's retention nightly; a
  hospital on hold is never swept. New commands: `core emr exclude` (as a
  login made from `EMR_ADMIN_DB_USER`, a member of `mhai_emr_admin` only;
  the MRN from stdin), `core emr remove-hospital-data` and `core hospitals
  legal-hold place|lift` (provisioning login). Every purge and hold change
  is audited with ids and counts (`emr.exclusion_purged`,
  `emr.exclusion_purge_held`, `emr.exclusion_purge_released`,
  `emr.retention_swept`, `legal_hold.placed`, `legal_hold.lifted`,
  `emr.hospital_data_removed`); `alert.not_raised` gains
  `patient_excluded`. OpenAPI 1.21.0 (additive): the exclusion's reply
  carries `purge_held`. `alerts.UsageBlock` counts alerts for Usage under
  the small-group rule (no route yet).
- EMR integration, PR 8b (the finding consumer; off by default: it runs only
  with `CORE_EMR_ENABLED`, and with no rule set activated for a hospital
  every finding is shadow and never reaches it). Core's `core_emr` consumer
  re-reads each finding under its subject's hospital as `mhai_emr_core`
  and, once per finding and status, raises a routed alert (hospital active
  with a connection on, the null-MRN gate `CORE_EMR_ALLOW_NULL_MRN`, the
  hospital's units, levels and recipients), updates the patient's open
  alert for the same rule and analyte, updates an alert in place for an
  amended result (`amended`) or withdraws it (`source_amended`,
  `source_retracted`), marking a handled alert without reopening it;
  historical and shadow findings never alert; a breaker trip is recorded
  as `emr.source_paused`. Every alert written is audited (`alert.raised`,
  `alert.updated`, `alert.not_raised`, `alert.withdrawn`). A message that
  fails five times is dead-lettered on `dead.v1.<hid>.core_emr` (the
  gateway NATS user may publish there), never dropped. Migration **00031**
  (after Analysis PR 6's 00030): `emr_consumed`, a handled alert may carry
  `withdrawn`, an alert must name an active patient (a trigger), the
  definers `alert_route_facts()` and `alert_source_known()`, and the
  consumer's audit append. No OpenAPI change.
- Patient Analysis PR 6: the analyst grant (migration 00030; off by
  default, no route). Triggers on `user_roles` enforce the analyst rules for
  every writer: the grantor an active hospital admin of the person's
  hospital and not the person, the hospital's analysis switch on, the person
  an active clinician with two-step, at most 10 analysts; an analyst row is
  never updated, and removing `clinician` removes `analyst`. Every grant and
  removal is recorded and the database refuses to commit one without its
  `analyst.granted` or `analyst.removed` audit entry. `core users roles
  <email> --add|--remove analyst --by <admin email>` grants and removes
  through the same functions, as the provisioning login; `core users create`
  no longer takes `analyst`. The step-up on `PATCH /hospital/staff/{id}`
  waits for the staff endpoints.
- EMR integration, PR 8a (alert settings and routes; off by default: nothing
  raises an alert until core's finding consumer, PR 8b). Migration **00029**
  (after Analysis PR 5's 00028): `alert_settings`, `alert_units`,
  `alert_levels`, `alert_recipients`, `alerts`, `alert_deliveries` and
  `user_notification_settings`, with row-level security and column grants;
  recipients must be active clinicians of the hospital (a trigger); a
  recipient deactivated, losing `clinician` or taken off the named list has
  their open alerts moved to the queue with an `alert.requeued` fact
  (drained to the audit chain); the merge re-points alerts and the
  exclusion withdraws them (`withdrawn: excluded`); `emr_reject_match()`,
  `emr_hospital_sources()`, `emr_patient_sources()`, `emr_alert_links()`.
  New routes (OpenAPI 1.20.0, additive): `GET`/`PATCH
  /hospital/alert-settings`, `GET /hospital/patient-matches`, `POST
  …/{id}/confirm` and `/reject` (step-up), `POST /hospital/patient-exclusions`
  (step-up), `GET /alerts` (with `sources_status`), `GET
  /alerts/unread-count`, `POST /alerts/{id}/acknowledge` and `/dismiss`,
  `GET`/`PATCH /me/notification-settings`; error codes `already_handled`
  and `no_recipients`. Switching a connection on now asks the emr service
  to poll it at once (`sync_now`). The emr service no longer announces
  shadow findings. Merges and exclusions retry a deadlock, then answer 503.
- Patient Analysis PR 5: the de-identified drill-down and the reveal
  (migration 00028, OpenAPI 1.19.0; off by default, demo hospital only).
  `GET /analysis/patients?drill&level=deidentified&period&start&…` lists the
  patients behind one shown count of 5 or more (`{level, count, patients}`,
  at most 200 rows of `{pid, age_band, sex, setting, conditions, findings}`).
  Drill keys are derived when a view is served (HMAC over the hospital,
  period, filters, view and row), only on the affected view's age band, sex
  and setting rows, co-occurring pairs and findings, under condition, age
  band, sex and setting filters; nothing is computed for a list, and a key
  of another hospital, period, filter set or key set is 422 `too_small`.
  Pids are random, per hospital, period and subject. Per person: 10 lists a
  minute, 50 a hospital day and 300 distinct patients a calendar quarter
  (429 `rate_limited`, one flagged entry a day). The role, two-step and
  account are re-read for each list. Every list is audited
  (`analysis.patients_viewed`), and the database refuses a list without its
  entry. `GET /platform/audit/{id}/patients` reveals a drill-down entry's
  pids to platform admins and auditors, each reveal audited
  (`audit.patients_viewed`); the entry's detail answers `patients: {count}`.
  New problem codes `too_small` and `level_not_supported` (422).
- EMR integration, PR 7 (the alert rules; off by default). Migration
  **00027** (after Gemini's 00024, Analysis PR 4's 00025 and EMR PR 6's 00026): `emr.findings` (one per rule set
  version, rule and observation; historical, shadow, preliminary,
  arrived_late, supersedes and withdrawn), `emr.breaker_trips`,
  `emr_rule_set_activation`, `emr_pause_connection()`,
  `emr_sources_status()`, the `emr_normalise` and `emr_rules` job types, and
  a paused source reported as `errors` at once. The emr service's rules
  engine (`internal/rules`): a strict, hashed YAML rule set, the seven rule
  kinds with golden tests, the `emr_rules` consumer (holds a failing
  batch, no partial findings) and the per-source breaker.
  Rule set v1 (`services/emr/rules/ruleset-1.yaml`): pilot, clinical
  sign-off deferred, conservative critical values with their sources in
  `REFERENCES.md`. New route `GET`/`PUT /platform/hospitals/{id}/emr-rule-set`
  (platform admins, step-up, audited `emr.rule_set_switched`; OpenAPI
  1.18.0, additive), `emr rules check`, `EMR_RULE_SET_FILE` and the breaker
  settings, and the `emr.v1.Finding` event.
- EMR integration, PR 6 (FHIR R4; off by default). Migration **00026**
  (after Gemini's 00024 and Analysis PR 4's 00025): `emr.sync_state`,
  `emr.raw_records.backfill`, and `emr_platform_health()` reporting a
  connection that is on and silent for three poll intervals as `errors`.
  The `fhir_r4` connector is built: SMART Backend Services sign-in (RS384
  or ES384, credential origin only), a read-only test with capability checks, and polling by `_lastUpdated`
  with a per-type cursor, page and record limits, references read from the
  same server only, and hostile-response refusals. It declares an optional
  `mrn_namespace` setting. The scheduler adds advisory locks, backoff and
  `Retry-After`, backfill flags, daily reconciliation, a stop after three
  sign-in refusals, and `sync_now`. Also: `gokit/recordlink`, the synthetic
  FHIR server (`emr synth-fhir`, development only), and
  `services/emr/scripts/e2e-fhir.sh`, which runs the sync end to end
  through egress-emr. egress-emr gains a development-only
  `EGRESS_EMR_DEV_EXEMPT`, and the emr service a development-only
  `EMR_DEV_CA_FILE`.
- Cloud model providers: Google Gemini as a fourth provider (OpenAPI 1.16.0,
  migration 00024; off by default). The worker's `gemini_adapter` calls
  Google's OpenAI-compatible endpoint
  (`generativelanguage.googleapis.com/v1beta/openai`) with the openai SDK,
  under the same gate, egress scan (exact header list, every leaf pinned),
  budget reservation, strict reply check, `max_retries=0` and no redirects
  as the other adapters; a bad key (400 `API_KEY_INVALID`) is `auth_failed`.
  The model comes from `INTELLIGENCE_CLOUD_GEMINI_MODEL`. The worker refuses to
  start with gemini enabled unless `INTELLIGENCE_CLOUD_GEMINI_PAID_TIER=verified`
  (the key must be on a paid, billing-enabled project: free-tier prompts may
  be used by Google); demo included, only `APP_ENV=development` warns
  instead. `gemini` joins every closed set: core's providers, the route,
  `mhai_cloud_reserve`, the audit and fact schemas, the CLI and the
  `PUT .../model-policy` request. Response enums can't widen in /api/v1
  (ADR 0012), so `ModelPolicy` and `generated_by` gain `provider_code` (a
  string with documented values, any provider); `cloud_provider` and
  `generated_by.provider` keep their enums and are null for gemini.
  Migration 00024 merges first; Patient Analysis PR 4 takes 00025 and EMR
  PR 6 00026.
- The pilot: cloud answers for one real hospital on the demo host
  (`deploy/pilot/`, product owner's decision of 12 Nov 2026; the deferred
  sign-offs are recorded in PR #134). `deploy/compose.yml` gains the
  `pipeline` worker under the `pilot` profile (off by default: the default
  deploy only gains empty `CORE_CLOUD_PROVIDERS` and
  `INTELLIGENCE_DB_PASSWORD` and a `NATS_PIPELINE_NKEY` default whose seed was thrown away (checked by `nats-server -t` in CI, `deploy/nats/check-configs.sh`)), and
  `deploy/nats/nats.conf` the `pipeline` NKey user (a test keeps its
  permissions equal to dev's). `deploy/pilot/enable-pilot.sh` makes the
  pipeline's NKey, login and key file, starts the egress proxy and the
  pipeline, and creates the hospital and its model policy; the provider is
  a parameter (anthropic, or gemini once its adapter merges). The
  intelligence Dockerfile gains a `pilot` target with the embedding and NLI
  weights pinned and loaded offline, built for arm64 by the new "Pilot
  pipeline image" workflow. The worker gains `INTELLIGENCE_SINGLE_HOST=on`:
  NATS and Postgres without TLS outside development, only to Docker service
  names on its own host (`tests/security/test_single_host_transport.py`).
- Document pipeline phase 0, part B: model promotion, the lean version
  (`services/intelligence/intelligence/modelstore/`, the `mhai-models` CLI).
  File checks for a trained run (safetensors only, a fixed list of JSON
  files, header validation, size caps, SHA-256), the training manifest with
  the approved-dataset and licence registries and the model card, canonical
  manifests signed with one Ed25519 key (purpose bound in the signed bytes;
  the public key in `models/trust-policy.json`, the private key a
  repository secret or a key file; `mhai-models keygen`), a plain
  S3-compatible model store (conditional writes, versioning used if on),
  the locked evaluation test split, run sources (a local folder, Google
  Drive with a read-only token, pinned Hugging Face revisions), `promote`
  with a dry run, `models.lock` with `pin`, `lock-check` (an ordinary PR
  check; one reviewer's approval is the sign-off) and rollback by pinning an
  earlier version, load-time verification and the pipeline's pinned roles
  (not ready until every pinned role verifies; `model_versions` now carries
  `name@version#hash`). Three workflows (`model-promote`, `model-lock`,
  `model-testset`), no GitHub environment or setting needed. CI rehearses
  the whole flow on MinIO (`model-promotion-e2e`). With no model pinned,
  nothing changes. The enterprise controls left out are listed in TODO.md,
  "Before scale".
- Patient Analysis, PR 4 (aggregate routes; off by default, the demo
  hospital only). Migration **00025** (00024 is the Gemini provider's, #137): `analysis_subject_attrs` (a
  subject's attributes, fixed by its first fact), `analysis_hospital_start`,
  `analysis_snapshots` (one guarded body per context, insert only, at most
  20,000 per period and 4 MiB, refusing a shown count of 1 to 4),
  `analysis_view_log` and `analysis_unusual`; the gateway's definer
  functions `mhai_analysis_{period, periods, lock, cells, filters,
  snapshot_get, snapshot_put, records, viewed}`, owned by
  `mhai_analysis_reader`, each refusing a hospital that is off; the sweep
  also sweeps bodies and the read log. `GET /analysis/filters` and
  `GET /analysis/{view}` (hospital admin or analyst, two-step; 404 when off;
  one frozen period by `period`/`start` or an exact `from`/`to`, else 422
  `invalid_range`; 422 `too_many_filters`; 503 `not_ready` for a period not
  frozen yet or a body past the 2 s cold budget; 429 past 30 reads a minute;
  `history` copied from stored bodies; `Cache-Control: no-store`). The
  engine computes parents first, one guard per hospital and period at a time
  (an advisory lock), against the period's record kept between requests
  (`disclose.Ledger`), with the source list given to the guard; audit
  `analysis.viewed` once per session, filter set and period, and
  `analysis.unusual_querying`. New problem codes `too_many_filters` and
  `invalid_range` (422). openapi 1.17.0 (additive). Drill fields are null
  until PR 5. Every opaque filter (condition, location, specialty, source)
  must be in the period's
  stored filter lists (else 422 before anything is computed), and a person
  may have at most 100 distinct contexts computed cold per period
  (`analysis_cold_budget`): every context a request would newly store, its
  child periods' and filter parents' included, is reserved before the
  engine runs, and at most 32 per request (else 429; a history point past
  them is sent hidden whole).
- EMR integration, PR 5 (canonical model, normaliser and patient index;
  off by default). Migration **00023** (the plan's 00022; 00021 and 00022
  were taken by Analysis and Plan 7c-3): `emr.raw_records` (a document
  record needs its review, A26), the canonical tables with provenance and
  versions, the per-hospital patient index (MRN and birth date nullable),
  `possible_matches`, `merges`, `unmapped_codes`; every key carries the
  hospital, RLS forced. Core's definers `emr_patient_in_hospital`,
  `emr_merge_patients` (locks both patients, re-checks, one wins) and
  `emr_exclude_patient`, which purges the patient's canonical rows, raw
  records and matches at once and keeps only the cleared patient row and
  its links; the emr role can't call them and can't delete. Audit schemas
  for `emr.patients_merged`, `emr.patient_match_rejected` and
  `emr.patient_excluded` (written by PR 8a's routes). In the emr service:
  `RawSink` (the allow-list, text cleaning, formula neutralising, one row
  per source version), synthetic code tables and maps (refused outside
  development), the normaliser (strict decimals and times, versions,
  unmapped kept unstandardised, dead letters after five deliveries, a sweep
  for unannounced rows), the patient index (by source id; matches only
  proposed, never merged), id-only signals, `proto/mhai/emr/v1/events.proto`
  and `emr unmapped`. The normaliser runs only with `EMR_CODES_DIR` and
  `EMR_MAPS_DIR` set.
- Platform analytics, part 7c-3: What changed and follow-ups (OpenAPI
  1.15.0, additive; Plan 7c Tasks 8 and 9). Migration 00022 (planned as
  00020; EMR and Patient Analysis took 00020 and 00021) adds
  `analytics_change_runs`, `analytics_changes` and `analytics_follow_ups`
  and adds the three follow-up actions to `mhai_platform`'s audit append
  policy (Down restores 00020's). Once a week, after the week's snapshot
  is stored, the scheduler compares the week with the month that finished
  before it from the two stored snapshots' shown cells only (Q3: questions
  at least 10 on both sides and a 50% change; the needs-review share and
  agreement rate with 20 on both sides and 15 points). Active clinicians
  and questions per clinician are not compared (ruling C9: a week's
  distinct clinicians aren't comparable with a month's); they come back
  later as like-for-like comparisons. Follow-up writes are limited to ten
  a minute per session (429 `rate_limited`). `GET /platform/analytics` now serves
  them in a new property, `what_changed {updated_at, window, items}`
  (`changes` stays `{unavailable: true}`: widening it would not be
  additive); `GET
  /platform/analytics/changes/{id}/series` serves an open change's
  measure per period. `PUT` and `DELETE
  /platform/analytics/changes/{id}/follow-up` (platform admins,
  `Idempotency-Key`, a version check) follow a change up with an optional
  note, edit the author's note or undo it; each write is audited
  (`analytics.followed_up`, `analytics.follow_up_edited`,
  `analytics.follow_up_undone`) with ids only, never the note.
- EMR integration, PR 4 (egress; off by default, profile `emr` only; no
  migration). `egress-emr`, the emr service's own CONNECT proxy
  (`services/emr/cmd/egress-emr`, same image): every CONNECT names its
  connection (`Proxy-Authorization`: the connection id and
  HMAC-SHA256(K_proxy, id), else 407) and gets only that connection's
  hosts (403); a name off the list, an IP literal or a port other than 443
  or 22 is refused before any DNS lookup; a listed name is resolved once,
  refused if any address is loopback, link-local or metadata, CGNAT,
  multicast, reserved, an IPv6 form carrying IPv4, inside the internal
  networks, or private outside the connection's declared ranges, and
  otherwise the checked address is dialled. No fresh allow-list (120 s):
  everything refused. The emr service writes the allow-list every 15 s
  (enabled, unpaused connections whose origin is unchanged, plus one under
  test), and its HTTP and SSH clients reach a hospital only through the
  proxy, only at the connection's origin: no cross-origin redirect, none
  with a credential, 20 MB bodies, 64 KiB headers, JSON depth 64, SSH host
  key pinned to the typed SHA256 fingerprint. `emr keys proxy-init` makes
  K_proxy. Compose: `emr` on the internal `backend` and `emr-proxy`
  networks only; `egress-emr` alone on `egress-emr`. In
  `docker-compose.yml` postgres and nats also join the internal `backend`
  network (the emr service's way to them); `deploy/compose.yml` without a
  profile is unchanged. `deploy/egress-emr/test-egress.sh` checks the
  proxy end to end on a Docker host. After review: the proxy token is
  bound to a 5-minute window; lookups are fully qualified; the host's
  addresses, the routing table's gateways and Docker's default bridge are
  always refused, and outside development the proxy needs
  `EGRESS_EMR_INTERNAL_CIDRS` to start (the dev `backend` network has a
  fixed subnet, `EMR_BACKEND_SUBNET`); tunnels pass half-closes, end after
  60 s idle or 30 minutes, and one connection holds at most 32 slots;
  overlapping ranges leave out only the connections involved, with an
  alert.
- Patient Analysis, PR 2: the data foundation, **off by default** (OpenAPI
  1.14.0, additive; analysis backend plan Tasks 2 to 4, ADR 0015).
  Migration 00021 (its number is fixed: EMR takes 00020) adds the roles
  `mhai_analysis_writer`, `mhai_analysis_reader` and `mhai_analysis_job`
  (NOLOGIN, reached by SET ROLE as 7c's job role), subjects, coded facts,
  first-seen rows, frozen nested periods, dictionaries, the analysis switch
  (global and per hospital; no row is off) and definer functions that refuse
  a switched-off hospital, the future, and any real ingest under a demo
  switch. A core loop (one copy at a time) ingests, sweeps and freezes per
  switched-on hospital after its local 00:30. Four optional key files
  (`ANALYSIS_*_KEY_FILE`) seal and match patient references.
  `core analysis seed-demo` switches the synthetic demo hospital (created
  with `core hospitals create --demo`; `hospitals.is_demo` never changes)
  on with `DEMO-SYNTHETIC` references and writes a seeded synthetic cohort;
  a switch that was ever signed off never takes demo references again;
  `core analysis switch off` lowers a switch; both are audited. The login
  and session `user` gains `features {analysis, named_access}`.
- Platform analytics reads, part 7c-2 (OpenAPI 1.13.0, additive; Plan 7c
  Tasks 6 and 7). `GET /platform/analytics?period=week|month|quarter`
  serves Use, Answer quality and Hospitals of the latest published period,
  with medians over shown values and trends over the latest 12 weeks, 12
  months or 8 quarters; `alerts` is null and `changes` unavailable until
  7c-3; `{not_yet, first_from}` before the first period.
  `GET /platform/analytics/hospitals/{id}/series` serves one hospital's
  stored figure per period (`questions_per_clinician`,
  `needs_review_share`); `GET /platform/analytics/changes/{id}/series` is
  404 until What changed exists. Every route reads stored snapshots only;
  30 reads a minute per session. No migration.
- EMR integration, PR 3 (connections; off by default). Migration 00020:
  the `emr` schema and roles, `emr_connections` (configuration only, no secret column or key),
  credential metadata, sync runs and errors, declared private ranges that
  can't overlap across hospitals. `services/gokit`: `emrurl` (address and
  credential-origin checks), `seal` (credentials sealed to the emr service,
  bound to connection, field and origin, 60 s), `emrtypes` (the connector
  types and fields), and the request token's `stp` claim, which the emr
  responder now requires on step-up ops. The emr service stores and wipes
  credentials and tests a connection with the development stub. Core serves
  `/platform/emr/connector-types`, `/platform/emr/connections` and
  `/platform/hospitals/{id}/emr/connections` (OpenAPI 1.12.0, additive; new
  codes `not_tested`, `hospital_removed`, `no_type`), sealing credentials as
  they arrive; a change of address origin wipes them and switches the
  connection off. With `CORE_EMR_ENABLED` false the writes answer 503
  `not_ready`; `CORE_EMR_CONNECTOR_TYPES` offers nothing unless named; the
  credential routes need `CORE_IDEMPOTENCY_HMAC_KEY_PATH`.
- Platform analytics on calendar periods, part 7c-1 (OpenAPI 1.11.0,
  additive; Plan 7c, rulings R41a to R41a-4). Migration 00019 adds the
  analytics scheduler's role (`mhai_analytics_job`) and reader, frozen
  per-period counts, `analytics_snapshots` and the release epoch. A core
  loop (one copy at a time) freezes each finished month-aligned week, month
  and quarter once, guards it with its parent set and the published
  history (`internal/smallgroup`), and stores one snapshot. New route
  `GET /platform/guardrails/periods?period=week|month|quarter` serves the
  latest published period's guardrail statistics from that snapshot
  (503 `not_ready` until the first one).
- EMR integration, PR 2 (skeleton; off by default): `services/gokit/`, a Go
  module core and the emr service share, with the request-token package
  (moved from core; its subject rule is now a table that adds
  `emr.v1.ctl.<op>` with audience `emr`, and new shared vectors), a
  bounded replay set, `dbx.WithHospital` over a closed role set, the log
  redactor (moved), the emr control-operations table and the generated
  `mhai.emr.v1` contract. `services/emr/`: `emr serve` answers
  `emr.v1.ctl.health` after verifying every request (token, reply subject,
  replay, caller rule, request id, strict schema); `emr keys seal-init`.
  The `emr` NATS user (dev config), core's rights on `emr.v1.ctl.>` and the
  `EMR` stream, created only when `CORE_EMR_ENABLED=true`; the compose
  profile `emr`. Core's Docker build context is now `services/` (`-f
  services/core/Dockerfile services`) in compose, CI, the deploy workflow
  and the Makefile.
- EMR integration, PR 1 (docs): ADR 0009 accepted; ADR 0014 records the
  separate `services/emr/` with its own credentials and egress, and the
  reviewed route for scanned records; the document pipeline design is
  amended (normalisation by the emr service, `emr.v1.<hid>.fetched`);
  frontend requirements §12.1 and §14.11 filed; TODO lists the people items
  H1–H9 and the live-use gates.
- Cloud model providers, part A: the per-hospital model policy (OpenAPI
  1.10.0, additive). Migration 00018 adds `hospital_model_policy` (off by
  default; no row means local), the model route set by core on each new
  question, and the monthly token ledger reached only through core's
  `mhai_cloud_reserve`/`mhai_cloud_settle` functions. Platform admins read a
  hospital's policy with this month's usage and change it (`GET`/`PUT
  /platform/hospitals/{id}/model-policy`; a change needs step-up MFA and is
  audited as `hospital.model_policy_changed`), and `core hospitals
  model-policy set` does the same from the CLI. `agreement_ref`
  takes only an agreement code (`AGR-`/`DPA-` plus digits, or
  `DEMO-SYNTHETIC`). `query.completed` gains the closed `egress` and
  `cloud_outcome` facts, and a result gains `generated_by` (`local` or
  `cloud` with its provider), read from the decision itself. No cloud call is
  made yet (parts B to E).
- Cloud model providers, part B (the gate; no network code): the worker's
  cloud settings, off by default (`INTELLIGENCE_CLOUD_PROVIDERS` empty), with
  start-up refused on an incomplete provider: no model, key file or
  zero-retention reference, a Fable or Mythos model, a key file not mode
  0400/0440 or holding a placeholder, `DEMO-SYNTHETIC` outside
  `APP_ENV=demo` (new: production in every other respect). Any `OPENAI_*`,
  `ANTHROPIC_*`, `AZURE_OPENAI_*` or `XAI_*` variable refuses start-up for
  every worker, cloud on or off. Keys are held as `ApiKey` (redacted). With a
  provider configured, logging changes: records are redacted at creation
  (`args=None`, exception text pre-rendered), warnings go through logging,
  uncaught exceptions are logged as `CRITICAL`, and the HTTP clients' INFO
  request lines are off. The gate types (`MaskedText`, `CloudContext`'s
  two-field allow-list, sealed `SharedChunk`s, `CloudPrompt`), the egress scan
  over the exact request bytes (shape, this patient's identifiers everywhere,
  number and date patterns on the patient-derived blocks, size), and the
  budget port over core's reserve and settle functions.
- Cloud model providers, part C (the client and the job): strict reply
  validation (citations only of chunks sent, no links), a closed retry table,
  `CloudModelClient` (local retrieval and verification, a gated, scanned,
  budgeted cloud draft, a per-attempt deadline, SDK retries off) and
  `ModelRouter`: each job runs on the route core's claim gives it, and
  records the provider, `egress` and the outcome. Every cloud failure
  escalates; there is no fallback.
- Cloud model providers, part D (adapters): Anthropic (Messages, strict
  structured output), OpenAI (Responses, `store: false`) and xAI (chat
  completions; the `x-zero-data-retention` header checked on every reply),
  against synthetic fixtures. The scan hook refuses any request header not
  on the provider's list (`scan.ALLOWED_HEADERS`), so an SDK update that adds
  one fails closed. A reply that reports more tokens than its reservation is
  logged (`cloud.usage_overrun`), counted (`mhai_cloud_usage_overrun_total`)
  and, above 10% over, alerted (`mhai_cloud_alerts_total{kind="usage_overrun"}`).
- Cloud model providers, part E (deployment, smoke and docs): the
  `egress-proxy` service in `deploy/compose.yml` (Squid pinned by digest,
  profile `cloud`, off by default) on `backend` and a new `egress` network,
  allowing only `CONNECT` to port 443 of the hosts in `allowed-hosts`, matched
  by name with no reverse lookup, IP literals refused;
  `deploy/egress-proxy/test-egress.sh` checks it (A15). The worker's proxy
  setting is `INTELLIGENCE_CLOUD_PROXY` (not the process-wide `HTTPS_PROXY`),
  and with a provider enabled start-up refuses without a CA bundle at
  OpenSSL's paths. `scripts/cloud_smoke.py`, a manual live smoke a person
  runs on their own machine (never the build server or CI) with a key from
  standard input, and `scripts/scrub_cloud_fixture.py`, whose `--check` runs
  on every committed fixture in the unit suite. `deploy/README.md` "Cloud
  models" (enabling, key rotation, switching off); TODO.md lists the human
  gate before real patient data and the people items H1–H7.
- Document pipeline phase 0, part A: the extraction schema
  `mhai.extraction.clinical_document` 1.0.0 and the evaluation harness
  (`services/intelligence/intelligence/doceval/`). Synthetic patients and
  documents rendered from 15+ layouts per type, scan and phone-photo
  degradation, handwriting-like and hostile pages, intake for real scans,
  the synthetic-data scanner, set v1 with its 30/70 split, every design §6.3
  metric, a runner over any extractor (stubs, a tiny safetensors model, an
  OpenAI-compatible endpoint), the evaluation report with pass thresholds per
  gate and slice (clinical change control), and the `mhai-doceval` CLI.
  Synthetic data only; no model is promoted yet (part B).
- Platform operations for platform admins and auditors (Plan 4, part 4c;
  OpenAPI 1.9.0, additive). `GET /platform/health` (the sampler's stored
  reading, one KV get per poll, 503 when none is fresh), `GET
  /platform/jobs` (HMAC-bound cursor), `POST /platform/jobs/retry` (platform
  admins, 10 calls a minute, creator and second-person rules re-checked),
  `POST /platform/jobs/cancel` (step-up code) and `GET
  /platform/guardrails?period=7d|30d|90d` (new: small-group rules, whole UTC
  days, one stored snapshot per period and day with a 15-minute grace, 6
  reads a minute). Migrations 00015 to 00017. New smoke test
  `TestSmokePlatformOps`. Reads of health, jobs and guardrails are not
  audit-recorded; retry and cancel write `job.retried` and `job.cancelled`.
- The audit log for platform admins and auditors (Plan 4, part 4b;
  OpenAPI 1.6.0, additive). `GET /platform/audit` (filters, `q`, HMAC-bound
  cursor), `/platform/audit/{id}`, `/hospitals`, `/people` and
  `POST /platform/audit/undisplayable`; every page read is recorded as
  `audit.viewed` or `audit.searched` (once per session and filter set per
  5 minutes, in the same transaction). Chain checks: `POST
  /platform/audit/verify` (auditors may write here and nowhere else),
  `GET /platform/audit/verify/runs`, `/runs/{id}`, `/status`, and an
  auditor's `POST .../runs/{id}/outcome`; a check also runs daily at
  06:00 UTC and sets the `audit_chain_broken` gauge. A chain row the
  catalogue can't map to an mhai-web kind is sent as the bare entry.
  `SECURITY_CONTACT_NAME` and `SECURITY_CONTACT_EMAIL` are required outside
  development (the status route is 503 without them);
  `deploy/setup-host.sh` asks for them and checks them as core does. Migration
  00014. New smoke test `TestSmokeAuditLog` (a synthetic auditor over
  `api.v1.platform`: start a check, poll it to `intact`, read the status,
  find clinician B's `review_submitted` under `type=review`); the smoke
  stack now has a `bff_platform` password and a synthetic security contact.
- `mfa_enrolled` on the `MFARequired` sign-in reply (`POST /auth/login`,
  OpenAPI 1.4.0). `false`: the account has no two-step set up, so the
  frontend sends the challenge with an empty code to get the enrolment-only
  session; `true`: ask the user for a code. It is the same enrolment value
  the sign-in check read and that the challenge uses, and is sent only
  after the password was proven. Wrong-password replies, audit rows and
  logs are unchanged. Additive (`oasdiff`).
- Feedback and corrections with a two-person rule (Plan 4, part 4a).
  `POST /feedback` (validate, or suggest a correction on your own completed
  question), `GET /feedback/stats/mine`, `GET /corrections`, `/mine` and
  `/reviewed`, and `POST /corrections/{id}/approve|reject|withdraw`. A
  correction is approved only by a different, active clinician of the same
  hospital (403 `self_approval` otherwise, audited); core and the database
  both hold the rule (migrations 00011 to 00013). An approval queues an
  `approve_correction` job in the same transaction; the pipeline worker's
  second consumer, `pipeline_learning`, stores one per-hospital
  `feedback_buffer` row and core records `correction.applied`. Nothing is
  applied to guardrail weights, the knowledge base or any model. The
  `pipeline` NATS user gains the learning consumer's three subjects only.
  New smoke test `TestSmokeCorrectionApproved`. The contract change is
  additive (`oasdiff`).
- `core hospitals activate <id or short code>`: moves a hospital made with
  the CLI from `setup_incomplete` to `active`, so its clinicians can
  `POST /queries`, and appends `hospital.activated` to the audit chain in
  the same transaction. It runs as the provisioning login (`mhai_provision`);
  an already active hospital is a no-op, and any other status is refused
  (a deactivated hospital is reactivated through the platform app).
  Migration 00010 gives `mhai_provision` `SELECT (status)` and
  `UPDATE (status)` on `hospitals`, held by the policy `provision_activate`
  to `setup_incomplete` -> `active` only, and lets it append
  `hospital.activated`. The smoke test activates its hospital with the
  command instead of an UPDATE as the owner.
- The demo's NATS has a `bff_platform` NKey user (publish `api.v1.platform`,
  receive `_INBOX_bff.platform.>`, nothing else), and both BFF seeds are now
  files under `/srv/mhai-backend/secrets/bff/` (mode 400, uid 1001, mhai-web's
  container user) instead of lines in `.env`. `deploy/setup-host.sh` makes the
  platform pair, moves the existing clinical seed into its file (same key
  pair) and removes it from `.env`, and takes `--rotate-bff
  clinical|platform|all`. `deploy/compose.yml` requires
  `NATS_BFF_PLATFORM_NKEY`: **run the new `setup-host.sh` on the instance
  before merging** (deploy/README.md, "Before merging"). A test holds the
  demo's BFF permissions equal to the dev config's.
- The intelligence boundary runs end to end (Plan 3 T15). Compose runs the
  Python workers as `pipeline` (`intelligence worker --service pipeline`, its
  own NATS user and password, its own database login `intelligence_app`, the
  request-token trusted keys read-only) and `echo` (profile `test`), in place
  of the one-shot `intelligence` service; core gets `CORE_HTTP_API`.
  `docker-compose.smoke.yml` (stub models, echo on) and
  `.github/scripts/boundary-smoke.sh` (`make smoke`) run
  `services/core/internal/smoke` as the clinical BFF over NATS: sign in,
  `POST /queries`, poll, the result, and `echo.ping` through the grant, with
  the audit chain verified. New CI job `boundary-smoke`; CI also checks every
  compose file parses and that the image ships `operations.json` and
  `catalogue.json`. New `.env.example` entries: `INTELLIGENCE_DB_USER`,
  `INTELLIGENCE_DB_PASSWORD`. **`INTELLIGENCE_DB_PASSWORD` is now required
  in the dev `.env`**: `core-migrate` (and so `make core-up` and `make up`)
  refuses to start without it.
- The demo deploy stops before migrating if `clinical_queries` holds rows
  migration 00007's constraints reject (`deploy/premigrate-check.sh`).
- Clinical questions end to end on core's side (Plan 3 T12): `POST /queries`
  (clinicians; 202 `{query_id, status}`; 429 `rate_limited` past
  `INTELLIGENCE_QUERY_CAP` per hospital; `Idempotency-Key`), `GET
  /queries/{id}` (`running` shows as `processing`, `cancelled` as `failed` /
  `cancelled`) and `GET /queries/{id}/result`; another clinician's or
  hospital's id is 404. Contract version 1.2.0 (additive; the result also
  carries `trust_band` and `cited_chunk_ids`). Core drains the workers' audit
  outbox onto the chain (identity from the job row, never the fact), consumes
  dead letters (`core_dead`: marks the job failed, audits `job.dead_lettered`,
  re-mints a job refused for a key an emergency rotation removed), and
  deactivating a user or hospital cancels their queued and running questions
  in the database (migration 00009). New optional setting
  `REQUEST_TRUSTED_KEYS_PATH` (re-read on SIGHUP); `core keys untrust` takes
  `--emergency`, and request-key rotations are audited
  (`intelligence.key_rotated`) through the provisioning login in
  `DATABASE_URL`.
- Core serves the frontend contract over NATS (spec §17.1, `internal/natsapi`):
  one HTTP request per message on `api.v1.clinical` and `api.v1.platform`, in
  queue group `core`, run through the same handler chain as HTTP (request id,
  body limit, principal, route rule, idempotency, RLS and audit). The BFF's
  NATS user replaces the `X-MHAI-Service` key there and the subject fixes the
  audience. A reply subject other than the sending app's own inbox gets no
  reply at all. New settings: `CORE_HTTP_API` (`on` by default until
  mhai-web's BFF switches; `off` serves only the probes, on loopback) and
  `CORE_NATS_API_CONCURRENCY` (default 256; past it 503 `unavailable`). New
  error codes, over NATS only: 405 `method_not_allowed`, 413
  `payload_too_large`. Contract version 1.1.0.
- The API docs are now one site for core and intelligence. `/api/docs` gains a
  second tab, `/api/docs/intelligence`: the NATS operations (subject, kind,
  request and response messages, who may call, `bff_direct`, timeout, hospital),
  every message in `proto/` with its comments, the closed error-code set and a
  note on how a call is checked. Generated by `cmd/permgen` (so
  `TestGeneratedFilesUpToDate` covers it) and embedded; same CSP, no script, no
  external origin. Add `location = /api/docs/intelligence` to the api-dev nginx
  (included in `deploy/nginx/mhai-api-dev.conf`) and reload it.
- Public API docs on the demo server at `https://api-dev.measured-health.org/api/docs`
  (`deploy/nginx/mhai-api-dev.conf`): only the docs page, its script and the
  spec, GET only, behind its own allow list; the API itself stays private.
- API docs on the core service: `GET /api/docs` (Redoc 2.5.4, vendored, no
  CDN) and `GET /api/openapi.yaml`, both embedded in the binary, outside the
  authenticated API chain and never served over NATS. Switch:
  `CORE_API_DOCS` (default on in development, off otherwise); strict CSP and
  other headers; on in the demo and dev compose.
- API versioning policy (ADR 0012): major version in the path and subject,
  additive-only changes within it, `Deprecation`/`Sunset` for retirement
  (90 days' notice at the demo stage), `info.version` as semver (now 1.0.1).
  CI fails a pull request with a breaking change to `openapi.yaml`
  (oasdiff 1.34.1). `httpx.Deprecated` sets the retirement headers.

### Deprecated

- `GET /platform/guardrails?period=7d|30d|90d` (ADR 0012, R41a-3): from the
  7c-1 release every count and share is hidden and the responses carry
  `Deprecation`, `Sunset` (90 days on) and `Link`
  (`/docs/notices/guardrails-rolling-windows`); after the sunset the values
  are 422. Use `/platform/guardrails/periods`. Migration 00019 empties
  `guardrail_snapshots` and revokes `mhai_guardrail_counts` from
  `mhai_platform`.

### Changed

- Go 1.26 for CI and the core and emr images (was 1.24, now out of
  support), with staticcheck 2026.2.2. Newer Go dependency updates require
  Go 1.25 or 1.26.
- The pipeline worker requires `sslmode=verify-full` for its database
  outside development. Core and the workers warn when
  `OTEL_EXPORTER_OTLP_INSECURE` is on outside development, and log why
  telemetry is off when an endpoint is refused.
- The demo's BFF NKey (`NATS_BFF_NKEY`) is now the clinical app's BFF
  (`bff_clinical`): it may publish `api.v1.clinical` and receive on
  `_INBOX_bff.clinical`. The intelligence workers are not deployed on the
  demo (documented in `deploy/compose.yml`); `CORE_HTTP_API` stays on there.
- The API docs show the Measured Health logo at the top of Redoc's sidebar
  (`info.x-logo`, served from `/api/docs/logo.png`, same origin). `info.version`
  1.0.2 (doc-only).

### Fixed

- The emr image's secret store folder (`/emr-secrets`) is now 0700, so a
  fresh `emr-secrets` volume takes that mode and `docker compose --profile
  emr` starts the emr service: before, the folder was 0755 (a folder COPY
  creates its target with the default mode) and the file store refused to
  start. The COPY sets it (`--chmod=0700`, that folder only);
  `services/emr/scripts/image-check.sh` checks the image's state folders'
  modes and owners and that every other top-level entry is root's. Found
  by the full live check (mhai-web `scripts/it/live-full.sh`).

- `GET /api/openapi.yaml` is now `text/plain` with `Content-Disposition: inline`,
  so a browser shows the spec instead of saving it (`application/yaml` was
  downloaded). Redoc's Download link still saves a file.
- The docs page no longer logs a CSP violation for Redoc's footer logo from
  `cdn.redoc.ly`: a small same-origin script (`/api/docs/redoc-shim.js`)
  drops that one image request (the footer link text stays). The CSP is unchanged and the Redoc bundle is
  not modified. The nginx allow list for api-dev gains the shim and logo paths.

### Removed

- The Python `RBACManager`, `AuditLogger`, `SecurePipelineWrapper` (and
  `UserRole`, `UserSession`, `demo_m4_part2`) in
  `intelligence.security.hipaa`, and the `intelligence/queue/` stubs (Plan 3
  T13). Core decides permissions and writes the audit chain, the worker
  verifies core's request token, and the database helper scopes by hospital.
  `PIIMasker`, `EncryptionManager` and the latency profiler stay.
- Redis: the `redis` compose service and volume, `REDIS_URL`, `REDIS_PORT`,
  the `QUEUE_*` settings in compose and `.env.example`, and the `fakeredis`
  dev dependency (with `redis` and `sortedcontainers` in `uv.lock`). Nothing
  in Go or Python needs it; NATS carries the jobs.

### Security
- Cloud model providers: `intelligence/boundary/cloud_repo.py` (the budget
  port's SQL: core's reserve and settle functions only) is added to
  `tests/boundary/test_no_direct_db.py`'s `REPOS`, the files allowed to hold
  SQL. A security-test change, reviewed in PR B.
- Cloud model providers: no real patient data goes to a cloud provider until
  the human gate in TODO.md is done (masker bar including names without a
  title, legal check, clinical evaluation, core's keyed name-token hashes).
  `test_todo_keeps_the_real_data_gate` fails if that entry is removed, and
  `test_every_abuse_case_has_a_test` checks every A/K case in the plan has
  its named test.
- The patient-scoping and two-person-review regression tests moved off
  `SecurePipelineWrapper` onto the orchestrator and the boundary
  (`chunk_visible_to`, `load_chunks`, `OrchestratorModelClient`) with the same
  assertions: a patient's chunks never reach another patient's answer, and a
  correction applies only after a second, different clinician approves. The
  role and audit checks the wrapper tested are core's and covered by core's
  tests; a new test asserts the boundary exposes no feedback or approval
  operation.
- `core migrate up` no longer lets a login's SCRAM verifier reach the
  Postgres server log when setting the password fails. Postgres logs a
  failed statement in full (`log_min_error_statement`). `EnsureLogin` now
  sends the attributes and the password in separate statements, probes
  with a secret-free statement that it may set the password before sending
  it, and, as a superuser, turns `log_min_error_statement` off for that
  transaction.
- Sign-in has a per-login ceiling across every client IP (20 failures an
  hour, keyed by hash, failing closed) on top of the (login, IP) lockout,
  and argon2 runs behind a process-wide gate (one slot per CPU; 503 when
  saturated).
- `core serve` refuses a database login that is a member of any role
  besides `mhai_app`, and refuses to start with the idempotency middleware
  missing.
- Retrieval and citation checks are scoped to the queried patient. Other
  patients' records are never returned, and citing one is redacted (#1).
- Reserved ids (`reference`, `unassigned`, `unknown`) can no longer be used
  as patient ids, and patient files without an id are skipped.
- The orchestrator fails closed. A missing or failing CCA/HGA escalates the
  result as `degraded` instead of inventing a score, and thresholds are
  passed per call instead of mutating shared state (#2).
- Doctor corrections need approval from a second, different clinician
  before they change the knowledge base, persona vector or trust weights.
  Approved corrections are PII-masked, patient-scoped and attributed (#3).
- NLI verifiers read the label order from the model. The configured model's
  order was being misread, so unsupported claims passed as entailed.

### Added
- Backend demo deployment on the shared preview instance (`deploy/`,
  `.github/workflows/deploy.yml`). After green CI on `main`, the core
  image is built for arm64, pushed to `ghcr.io/measured-health/mhai-multiagent-core`
  and deployed over SSH to `/srv/mhai-backend` (Postgres, NATS with
  JetStream, migrations, core), then checked with `/api/v1/healthz` and
  `/api/v1/readyz`. Off until the repository variable `DEPLOY_ENABLED` is
  `true`. Only core (127.0.0.1:8092) and NATS (127.0.0.1:4222, NKey users
  only) are published, on loopback; every container is memory-capped,
  read-only and without capabilities. `deploy/setup-host.sh` prepares the
  host once: secrets in `.env` (mode 600), the NATS NKeys, the volumes and
  the internal network `mhai-bff` for the frontend's BFF, which has a
  placeholder NATS user with no publish rights until Plan 3. The deploy
  checks the instance's SSH host key (`DEPLOY_HOST_FINGERPRINT`) and
  refuses to run without it.
- The core service (Go, `services/core/`, Plan 2): sign-in with lockout,
  rotating refresh tokens, two-step verification (TOTP, backup codes,
  trusted devices, step-up), password change, device sessions, terms
  acceptance, the permission table and principal middleware, hospital
  separation by row-level security, a hash-chained audit log and
  Idempotency-Key support. The contract is `services/core/openapi.yaml`,
  checked by a test against the served routes and the error codes.
- The core service runs in Docker Compose (`make core-up`): `core-migrate`
  (owner connection) also creates its two logins: `core_app`, a member of
  `mhai_app` only, for the service, and `core_provision`, a member of
  `mhai_provision` only, for the seed CLI (`core-cli`). `core-keys` makes
  the dev signing key. The core service's environment holds only its own
  secrets.
- `GET /api/v1/readyz`: 503 unless Postgres and NATS (with every KV bucket)
  answer. `core keys new|ensure` generates the access-token key.
- Expired Idempotency-Key rows are swept every 10 minutes
  (`mhai_sweep_idempotency()`, migration 00006).
- Docker Compose stack: Postgres, Redis, Ollama with model pull, the app, an
  optional OpenTelemetry collector, and a GPU override, with Make targets.
- The CCA can generate through Ollama (`CCA_BACKEND=ollama`).
- `scripts/e2e_models.py`: end-to-end check with real models on synthetic
  data.
- `SKILLS.md` security rules, contribution workflow for agents and humans,
  ADRs, PR and issue templates, `SECURITY.md`, CODEOWNERS and Dependabot.
- CI enforces lint, format, type checks, tests, coverage of changed lines and
  a secret scan. Tagged releases build and push the container image.

### Changed
- **Breaking:** the services moved under `services/`. The Go service is
  `services/core/` (module `.../mhai-multiagent/services/core`, binary
  `core`, compose services `core-migrate`; `GATEWAY_ENV` is now `CORE_ENV`).
  The Python package `mhai` is now `intelligence`, in
  `services/intelligence/` with its own `pyproject.toml` and `uv.lock`; the
  CLI is `intelligence` and the compose service `intelligence`. The NATS
  config is in `services/nats/`.
- **Breaking:** the package moved from `src/mhai` to `mhai`.
- **Breaking:** a `correct` feedback returns `{correction_id, status:
  "pending_review"}`; it is applied by `approve_correction`.
- **Breaking:** Python 3.12 only.
- Project managed with uv (`uv.lock`); CPU PyTorch by default.
- Two-stage Docker image (7.1 GB to 2.5 GB, CPU), with pinned base images.

### Fixed
- `core migrate up` can run more than once as a non-superuser CREATEROLE
  owner (the managed-Postgres path): resetting a login names only the
  attributes that are wrong, so Postgres 16 no longer refuses it for naming
  `NOBYPASSRLS` and `NOREPLICATION`.
- `PIIMasker` no longer corrupts text when patterns overlap (e.g.
  `[PHONE_REDACTED]TED]`), counts each masked span once, keeps labels such as
  "Patient:" and "MRN:", and masks a leading "+1" or "(" in phone numbers
  (#7).
- `mhai.agents` could not be imported (its `__init__` exported missing
  classes).
- Imports left over from the original notebooks.

## [1.0.0] - 2026-06

### Added
- Initial production release from Colab notebooks (M1–M4, ~18,500 lines).
- Five agents: DIA, QDA, CCA, HGA, Orchestrator.
- Four-layer hallucination cascade.
- HIPAA security layer: PII masking, AES encryption, RBAC, audit chain.
- 1000-case E2E regression suite.
