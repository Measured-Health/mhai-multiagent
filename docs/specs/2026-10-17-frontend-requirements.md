# What the clinical app needs from the API

- **Status:** Draft, for backend review
- **Date:** 2026-10-17
- **Related:** the API gateway design
  (`docs/specs/2026-10-08-api-gateway-phase1-design.md`, on
  `feat/8-api-gateway`), ADR 0005 (reviewed corrections), ADR 0007
  (contract-first API), ADR 0009 (many EMR systems); `SKILLS.md`
- **Sources (mhai-web):**
  - `docs/specs/2026-10-13-ui-design-v3.md` §5.4–5.6, §10, §11
  - `ARCHITECTURE.md` §2, §4.3, §4.7, §5
  - `docs/specs/2026-10-17-notifications-design.md` §8, §9
  - `docs/specs/2026-10-17-analysis-design.md` §2–6, §9
  - `docs/adr/0003-many-emr-systems.md`
  - `docs/specs/2026-10-17-platform-admin-design.md` §13, §16 (platform
    admin app, §14 below)

## 1. Purpose

The backend is now the core service (Go) plus Python model services (ADR 0010).

The clinical app (`mhai-web`) is being built ahead of the API. Where an
endpoint doesn't exist yet, the frontend calls an assumed shape. This
document lists everything it needs, grouped by area, so the backend can
match it or say what changed.

It adds to the gateway design; it doesn't replace it. Where the two
disagree, §13 lists the conflict. Nothing here is decided until the
gateway spec or an ADR says so.

### Status labels

- **Built (Plan 2)**, **Built (Plan 3):** served by the core service today,
  with the shape in `services/core/openapi.yaml` (the committed contract; a
  test keeps it in step with the routes).
- **Gateway:** in the gateway design. Designed, not built yet.
- **Gateway + change:** in the gateway design, but the frontend needs a
  different or larger shape.
- **Assumed:** the frontend calls this exact shape today. Please match it
  or tell the frontend what changed.
- **New:** needed, with no agreed shape yet.

### Conventions

All paths are under `/api/v1`. Errors use the gateway's problem format
(`{type, title, status, code, detail, request_id}`). Codes in **bold** below
are not in the gateway catalogue (§4.3) yet and need adding. People are
returned as `{display_name, full_name, specialty, hospital}`, never as ids,
unless the field is a staff id the admin acts on.

## 2. Auth and sessions

Source: UI design §5.5, §10 items 4–8 and 14; `ARCHITECTURE.md` §4.3.

### 2.1 How the frontend holds the session (BFF)

The browser never sees the API token. The clinical app's Next.js server
calls the API, keeps the access token server-side, and gives the browser
an httpOnly session cookie. It refreshes the token while the user is
active. The browser only reaches the API through a same-origin proxy that
forwards exact, known shapes. Status: **Built (Plan 2)** (15-minute JWT,
rotating refresh token, `X-MHAI-Service` BFF key, `X-MHAI-Client-IP`). The backend needs nothing extra, except:

- CORS stays strict; the BFF calls the API server-to-server.
- Rate limits are per user, not per IP, because every request comes from
  the BFF's address. Login lockout keys on (login name, client IP), so the
  BFF must pass the client IP, and the API must trust that header only
  from the BFF.
- Login and the two-step challenge also return a `refresh_token`, for the
  BFF only; it is never sent on to the browser. The BFF refreshes with
  `POST /auth/refresh {refresh_token}` (§2.2). **Built (Plan 2).**

### 2.2 Sign-in

| Item | Shape | Status |
|---|---|---|
| Login | `POST /auth/login {username, password, device_token?}` → `{access_token, expires_in, user}` or `{mfa_required: true, mfa_token}` | **Built (Plan 2)** |
| Work email as login name | accept an email in `username` (or rename the field) | **Built (Plan 2)** |
| Login `user` | `id`, `role` (primary), **`roles`** (all), `email`, `display_name`, `full_name`, `role_title`, `specialty`, `hospital`, `mfa_enabled`, `must_change_password`, `terms_accepted_at`, `recovery_codes_left`, `idle_timeout_minutes`, `review_overdue_hours` | **Built (Plan 2)** |
| Login `refresh_token` | login and the two-step challenge also return `refresh_token`, for the BFF only (never sent to the browser) | **Built (Plan 2)** |
| Refresh | `POST /auth/refresh {refresh_token}` → `{access_token, expires_in, refresh_token}`; the refresh token rotates on every use, reusing an old one more than 10 seconds after its rotation revokes the whole refresh-token family (that device's session); within 10 seconds it returns 409 `conflict` and revokes nothing, so the BFF retries with its newer token | **Built (Plan 2)** |
| Accept terms | `POST /auth/terms {version}` → 204; records version and time | **Built (Plan 2)** |
| Lockout | the same `login_locked` (429) reply for an unknown login as for a locked one, so it can't reveal which accounts exist | **Built (Plan 2)** |

Until `idle_timeout_minutes` and `review_overdue_hours` exist, the app uses
15 minutes and 4 hours.

### 2.3 Two-step verification (TOTP)

| Item | Shape | Status |
|---|---|---|
| Challenge | `POST /auth/mfa/challenge {mfa_token, code, trust_device?}` → token, plus `device_token` when `trust_device` was set and the hospital allows it | **Built (Plan 2)** |
| Enrol, verify | `POST /auth/mfa/enroll`, `POST /auth/mfa/verify` | **Built (Plan 2)** |
| Backup codes | 10 codes at enrolment (the gateway's recovery codes; the UI calls them backup codes); each works once | **Built (Plan 2)** |
| Regenerate backup codes | `POST /auth/mfa/recovery-codes` → 10 new codes; the old ones stop working | **Built (Plan 2)** |
| Per-hospital policy | `require_mfa_for_clinicians` (hospital setting); hospital admins, platform admins and auditors always need it | **Built (Plan 2)**: enforced; set with the CLI until the settings endpoint (§5.2) exists |
| Trust this device | `device_token` bound to the user it was issued to, refused for anyone else; allowed only when the hospital setting `allow_trust_device` is on | **Built (Plan 2)** |

Answered (design 2026-10-26 §6.4): a trusted device never skips the code
for `hospital_admin`, `analyst`, `platform_admin` or `auditor`.

### 2.4 Single sign-on (OIDC)

`GET /auth/oidc/login`, `/auth/oidc/callback`, `POST /auth/oidc/exchange`.
Status: **Gateway**. Change: `return_to` on the SSO start must be checked
against an allowlist, so it can never send the browser to another site.

### 2.5 Password

| Item | Shape | Status |
|---|---|---|
| Change | `POST /auth/password` | **Built (Plan 2)** |
| Forgot | `POST /auth/password/forgot {email}` → 202 for any email | Assumed |
| Reset | `POST /auth/password/reset {token, new_password}`; link valid 30 minutes; checked against leaked passwords; signs out every device | Assumed |

The `users` table needs an `email` column for this.

### 2.6 Devices

| Item | Shape | Status |
|---|---|---|
| List | `GET /auth/sessions` → active devices | **Built (Plan 2)** |
| Sign out one | `DELETE /auth/sessions/{id}` → 204 | **Built (Plan 2)** |
| Sign out everywhere | `POST /auth/sessions/revoke-all` → 204 | **Built (Plan 2)** |

Built (Plan 2) with a session (refresh family) record per device; sign-out
denylists the session and its access tokens. `POST /auth/logout` signs out
the current session.

## 3. Roles and tenancy

Source: UI design §5.6, §10 items 2 and 13; `ARCHITECTURE.md` intro and §2;
analysis design §2.

| Role | App | Can do | Status |
|---|---|---|---|
| `clinician` | clinical | queries, feedback, review corrections, alerts | Gateway |
| `hospital_admin` | clinical | staff, hospital settings, usage, activity, review oversight, clearances, analysis; **one hospital**; no consultations or patient data | New |
| `analyst` | clinical | analysis for their own hospital; granted by a hospital admin; audited | New |
| `admin` (platform admin) | platform admin app | hospitals, users, health, DLQ, holds, settings, audit; every hospital, no clinical content | Gateway + change |
| `auditor` | platform admin app | audit log and verify; named-access audit export | Gateway + change |

- **Roles combine** in the clinical app: `clinician`, `hospital_admin` and
  `analyst` can be held together by one account. Platform roles stay
  separate accounts, as the gateway says.
- The session carries `roles` (every role) and `role` (the primary one).
  Each route checks that the needed role is in `roles`.
- A hospital admin who isn't a clinician gets **403** on every clinical
  endpoint.
- **Hospitals:** a `hospitals` entity; every clinical-app user belongs to
  one. Every staff, settings, usage, activity, oversight, alert, analysis
  and clearance query is filtered by the hospital in the session, never by
  a hospital id from the request. Another hospital's resource is a **404**.
- Every hospital admin action is audited with the actor and the hospital.
- **Platform admin, hospitals:** add, edit, deactivate (reversible; staff
  can't sign in, data kept), reactivate, and remove (permanent, only
  through a retention-aware process because of legal holds); configure a
  hospital's SSO; invite its first hospital admin. Status: **New**.

## 4. Corrections

Source: UI design §5.4, §10 item 1. Status: **Assumed** (Plan 3) unless
marked. Builds on ADR 0005.

**Correction object:** `id`, `query_id`, `status` (`pending_review`,
`approved`, `rejected`, `withdrawn`), `corrected_diagnosis`, `reason`,
`submitted_by`, `decided_by` (people), `submitted_at`, `decided_at`,
`rejection` (`{reason_code, note}` or null). For the review view it also
has `original` (`decision`, `degraded`, `diagnosis`, `icd10_code`,
`response_text`, `citations[{source_title, claim_text, action}]`) and
`consultation` (`age`, `sex`, `symptoms`, `clinical_question`, `urgency`,
`specialty`). The reviewer isn't the query owner, so
`GET /queries/{id}/result` can't serve this.

| Endpoint | Behaviour | Status |
|---|---|---|
| `GET /corrections?status=pending` | others' waiting corrections, never your own | Gateway + change (shape) |
| `GET /corrections/mine` | your own, every status, newest first | Assumed |
| `GET /corrections/reviewed` | your decisions as reviewer, last 30 days, at most 50 | Assumed |
| `POST /corrections/{id}/approve` | 204; **403** `self_approval` on your own | Gateway + change (204) |
| `POST /corrections/{id}/reject {reason_code, note?}` | 204; `reason_code` one of `not_supported`, `needs_rewording`, `unsafe`, `out_of_scope`; `note` ≤500 chars; the submitter sees it; **403** `self_approval` on your own | Assumed |
| `POST /corrections/{id}/withdraw` | 204; your own waiting correction only; **403** `forbidden` otherwise | Assumed |
| `GET /feedback/stats/mine` | `agreed`, `corrections_waiting`, `corrections_approved`, `corrections_rejected`, `corrections_withdrawn` | Assumed |

- **409 `already_decided`** when approve, reject or withdraw meets a
  correction that is no longer waiting. **404** `not_found` for an unknown
  id.
- Approve, reject and withdraw honour `Idempotency-Key` (§10).
- An approval may arrive as a browser `keepalive` request when the
  reviewer leaves during the 60-second undo. Treat it like any request.
- Decisions on your corrections appear in notifications (§11). Polling is
  enough at first.
- Open: whether reviewers are limited to their specialty
  (`reviewers_share_specialty`). Until decided, every clinician sees the
  whole queue.

## 5. Hospital admin endpoints

Source: UI design §5.6, §10 item 3; `ARCHITECTURE.md` §4.7. Status:
**Assumed** (Plan 4). None returns question text, results or patient
details. All need `hospital_admin` with two-step on (§6).

### 5.1 Staff

| Endpoint | Behaviour |
|---|---|
| `GET /hospital/staff` | everyone at the hospital: `id`, `full_name`, `display_name`, `email`, `roles`, `specialty`, `can_review`, `review_specialties`, `status` (`active`, `invited`, `deactivated`), `issues` (`mfa_not_set_up`, `invite_expired`), `sign_in` (`sso`, `password_mfa`, `password`, null), `last_active_at`, `joined_at`, `invited_at`, `invite_expires_at`, `is_you`, `version` |
| `POST /hospital/staff/invites {emails, roles, specialty}` | at most 200; → `{invited, skipped: [{email, reason}]}`, `reason` one of `already_staff`, `domain_not_allowed`, `invalid_email`, `duplicate`. With SSO, invitees just sign in; otherwise an email invite valid 7 days |
| `PATCH /hospital/staff/{id} {version, roles?, specialty?, can_review?, review_specialties?}` | → the person |
| `POST /hospital/staff/{id}/deactivate` | reversible; they can't sign in → the person |
| `POST /hospital/staff/{id}/reactivate` | → the person |
| `POST /hospital/staff/{id}/password-reset` | sends a reset link; signs them out everywhere → the person |
| `POST /hospital/staff/{id}/reset-mfa` | they set it up again at next sign-in → the person |
| `POST /hospital/staff/{id}/remind` | reminder to set up two-step → the person |
| `POST /hospital/staff/{id}/resend-invite` | 7 more days → the person |
| `DELETE /hospital/staff/{id}` | body `{mfa_code}` or `{backup_code}`, plus `Idempotency-Key` → 204. Deletes the account but keeps past consultations, feedback and audit entries, attributed under the retention rules. For an invite, cancels it. See §6 |

Refusals for the staff endpoints:

- **403 `self_change`**: an admin deactivates or removes themselves, or
  drops their own `hospital_admin`.
- **409 `last_admin`**: no active hospital admin would be left.
- **409 `conflict`**: stale `version`, or an action that doesn't fit the
  status.
- **404** `not_found`.

### 5.2 Settings and usage

| Endpoint | Behaviour |
|---|---|
| `GET /hospital/settings` | `hospital_name`, `review_overdue_hours`, `reviewers_share_specialty`, `require_mfa_for_clinicians`, `idle_timeout_minutes`, `allow_trust_device`, `sso` (`{status, domain}`, read-only), `allowed_email_domains` (read-only), `specialties`, `limits` (allowed choices), `version` |
| `PATCH /hospital/settings {version, …}` | → the same; **409** `conflict` on a stale version |
| `GET /hospital/usage?period=this_month\|last_month\|last_90_days` | counts only: `consultations`, `active_clinicians`, `agreement_rate`, `median_review_hours`, `confidence` (`high`, `caution`, `review`, `failed`), `by_specialty` |

### 5.3 Activity

`GET /hospital/activity?type&person&staff_id&from&to&flagged&cursor&limit`
→ `{events, next_cursor}`, newest first, kept 12 months. Each event is
structured, never free text: `id`, `at`, `type` (`sign_in`,
`consultation`, `feedback`, `review`, `account`, `admin`, `security`),
`kind`, `actor` and `target` (`{display_name}`), `specialty`,
`confidence`, `device`, `count`, `flag` (`policy`, `alert` or null). A
consultation event carries only its specialty and confidence level.

Required event: **`security` / `self_role_change`**, flag `alert`, with
the actor, the target (themselves) and `roles_added`, whenever an admin
adds a role to themselves (§6).

### 5.4 Review oversight

| Endpoint | Behaviour |
|---|---|
| `GET /hospital/review-oversight` | `overdue_after_hours`, `waiting`, `median_decision_hours`, `decided_this_week`, `items` (`id`, `ref`, `suggested_by`, `specialty`, `submitted_at`, `eligible_reviewers`, `assigned_to`, `reminded_at`), `reviewers_by_specialty`; no clinical content |
| `GET /hospital/corrections/{id}/eligible-reviewers` | staff who may review it |
| `POST /hospital/corrections/{id}/remind` | 204 |
| `POST /hospital/corrections/{id}/assign {staff_id}` | 204; **409** `already_decided`; **422** `not_eligible` |

An admin can't approve or reject unless they're also a clinician.

## 6. Security rules

Source: UI design §10 items 3, 7 and 14; `ARCHITECTURE.md` §2, §4.7, §5.
These are requirements, not suggestions. Each needs a test in
`tests/security`.

1. **Two-step is required for hospital admins and platform admins.** The
   API refuses every `/hospital/*` endpoint, and every platform admin and
   auditor endpoint, with **403 `mfa_required`** for a session whose user
   hasn't turned two-step on. The login `user` reports `mfa_enabled` so
   first sign-in can ask for setup. Status: Gateway + change (the gateway
   enforces this for `admin` and `auditor` with 401; it must cover
   `hospital_admin`, and the frontend expects 403 on these routes).
2. **Removing staff needs the admin's current code.** `DELETE
   /hospital/staff/{id}` takes `{"mfa_code": "123456"}` or
   `{"backup_code": "ABCD-1234"}` in the JSON body. Its own refusals:
   - **403 `mfa_required`**: the admin has no two-step set up;
   - **403 `mfa_invalid`**: wrong code;
   - **429 `mfa_locked`**: too many tries.
3. **Guards come before the code.** `not_found`, `self_change`,
   `last_admin` and `conflict` are checked first, so a refused request
   doesn't use up the code or an attempt.
4. **Codes are single-use.** A TOTP code can't be replayed within its time
   window, here or at sign-in. One `last_used_step` per user, shared by
   every place that checks a code. (The gateway already stores it for
   sign-in.)
5. **The attempt counter is atomic.** Parallel requests can't get extra
   tries (for example a Redis `INCR` with expiry, checked after the
   increment).
6. **Backup codes are shared with sign-in.** They are the account's own
   codes; one used here can't be used again anywhere.
7. **Admins may add roles to themselves**, for example give themselves
   `clinician`, but it is audited and flagged (`self_role_change`,
   `roles_added`, flag `alert`). Dropping their own `hospital_admin` stays
   refused (`self_change`). Product decision, 2026-10-17.
8. **Idempotency keys** are honoured for at least 72 hours (§10).
9. **No clinical content on admin routes.** Hospital admin and platform
   admin responses carry counts, specialties and confidence levels only.
   The one exception is named drill-down under a verified clearance (§12).

Open question: a hospital admin who signs in only through SSO may have no
MHAI two-step code. The backend must decide whether IdP MFA counts for
rule 1, and how such an admin confirms a removal (rule 2).

## 7. Results and history

Source: UI design §10 items 9–12.

| Item | Need | Status |
|---|---|---|
| Ask a question | `POST /queries {patient_data, clinical_question, urgency, specialty?, client_created_at?}` → **202** `{query_id, status: "queued"}`; clinicians only; 429 `rate_limited` past 10 in flight per hospital; `Idempotency-Key` | **Built (Plan 3)** |
| Status | `GET /queries/{id}` → `{query_id, status, progress, current_stage, error_code?}`; `status` is `queued`, `processing`, `completed` or `failed` (a cancelled question is `failed` with `error_code: "cancelled"`); another clinician's or hospital's id is 404 | **Built (Plan 3)** |
| Result | `GET /queries/{id}/result` → `ClinicalResult`, 404 until completed; adds `trust_band` (provisional) and `cited_chunk_ids`; `citations` stays empty until the pipeline stores citation details | **Built (Plan 3)** |
| Intelligence unavailable | **503 `unavailable`** (title "Unavailable", detail "Couldn't get an answer. Try again."): intelligence didn't answer in time, no worker is running, or core's NATS API pool is full; `not_ready` stays for readiness | **Built (Plan 3)**: used |
| Confidence breakdown | all four measures: evidence match, internal consistency, values and dosages, answer stability. The UI hides a bar until its value exists; it never invents one | Gateway + change (L1, L2 are `null` today) |
| ICD-10 | `icd10_code` nullable | Gateway |
| Consultation history | persistent, per clinician, searchable; stores clinical text, so needs retention and access rules. Session-only until then | New |
| Stage timings | per-stage durations; elapsed time until then | New |

## 8. Weak networks

Source: UI design §10 items 15–19.

| Item | Need | Status |
|---|---|---|
| Retry-safe writes | `Idempotency-Key` on `POST /queries`, `POST /feedback`, the correction actions, every hospital admin write, alert actions and clearances. A repeat with the same key returns the first response and does no new work | **Built** for `POST /queries` (Plan 3) and the Plan 2 writes; New for the rest |
| Key lifetime | keep results for **at least 72 hours** (offline outbox) | New |
| `client_created_at` | optional ISO 8601 on `POST /queries` and `POST /feedback`, so the record shows when the clinician asked | **Built (Plan 3)** on `POST /queries` (checked, not stored); New for `POST /feedback` |
| Compression | gzip or brotli on JSON | New |
| Cheaper polling | `ETag` on `GET /queries/{id}` with `If-None-Match` → 304; optionally `?wait=20` long poll | New |
| Tolerant sessions | keep rate limits loose enough for backed-off retries | Gateway |

## 9. Patient alerts and notification settings

Source: notifications design §1, §5, §8, §9. Status: **Assumed** (Plan 5a).
Needs clinical and privacy sign-off before live use: it is the first place
MHAI shows patient names and MRNs.

| Endpoint | Behaviour |
|---|---|
| `GET /alerts?status=new\|handled\|dismissed&severity=…&source=…&cursor=…` | the signed-in clinician's alerts, newest first, scoped to their hospital and routing |
| `GET /alerts/unread-count` | one number |
| `POST /alerts/{id}/acknowledge` | `Idempotency-Key`; **409** when already handled by someone else, returning who and when |
| `POST /alerts/{id}/dismiss {reason, note?}` | `reason` one of not clinically relevant, already known, wrong patient, other; `Idempotency-Key`; **409** as above |
| `GET` / `PATCH /me/notification-settings {alert_popups, correction_popups}` | saved with the account. Critical alerts always pop up, whatever the setting |

**Alert fields:** `id`, `severity` (`critical`, `urgent`, `info`),
`finding` (`name`, `value`, `unit`, `flag`, `reference_range`,
`recorded_at`, `code_system`, `standardised`), `likely_causes` (up to
three: `label`, `confidence`), `patient` (`name`, `mrn`, `location`:
admitted ward and bed, outpatient clinic, emergency, or not recorded),
`age`, `sex`, `source_label`, `emr_link`, `status`, `handled_by`,
`handled_at`, `created_at`.

- Alerts come from the EMR feed (§12, ADR 0009). A clinician is always in
  the loop: alerts go to clinicians or the hospital, **never to
  patients**.
- Routing (which units and alerts, who receives them) is set by the
  hospital admin later. Shape: **New**.
- Admin-only accounts get no alerts.

### 9.1 Patient alert numbers on Usage

Source: mhai-web `docs/specs/2026-10-17-usage-alert-analytics-design.md`
(§4, §5, §13). Status: **Assumed** (Plan 5b). Shape: **New** field on an
existing endpoint.

`GET /hospital/usage?period=this_month|last_month|last_90_days` gains an
`alerts` field, for the caller's own hospital only, **counts only** (no
patient names, MRNs or single alerts):

- `null` (or absent) when alerts aren't switched on for the hospital: the
  group is hidden. `{ "unavailable": true }` when the numbers couldn't be
  worked out: the page shows "Couldn't load", never zeros.
- **Volumes:** total, by severity (Critical, Urgent, Info), still unhandled.
- **Response:** median minutes to acknowledge (all, and Critical), and the
  share of Critical alerts handled within `critical_target_minutes`
  (hospital setting, 15 by default).
- **Outcomes:** acknowledged and dismissed shares; dismissal reasons with
  counts.
- **Top findings and top locations:** up to 5 each. Findings use the
  canonical display label, unmapped ones grouped as "Not standardised";
  locations at ward or clinic level, never a bed.
- **Series:** alerts over time, daily for the month periods and weekly
  (from the period's first day, in 7-day steps) for the last 90 days;
  hospital-wide totals only.
- **Trend changes, all worked out by the backend** (the frontend computes
  no statistics): each measure's change against the previous period of the
  same length (`direction`, `amount`, `previous`); unusually busy days or
  weeks (`usual_multiple`); findings and locations rising sharply; a
  worsening flag when the Critical-within-target share drops.
  `has_previous: false` when there is no previous period.

**Small-group rule (server side, required):**

- Any number from a group of fewer than 5 is sent as
  `{ "value": null, "suppressed": true }`. Raw 1 to 4 are never sent.
  Zero is sent as 0.
- In a set that adds up (total and severities, dismissal reasons,
  acknowledged and dismissed), hide enough groups that the hidden ones
  together are at least 5. Never exactly one hidden member.
- Shares, medians and the worsening flag over a hidden group (Critical, or
  the total) are hidden too.
- A change is sent only when both periods' values are shown. A hidden group
  is never flagged as rising, and "rising" needs at least 5 in both periods.
- The series totals are not suppressed (no split by severity, finding or
  location).

The frontend fails the whole group closed ("Couldn't load") when any of
these rules is broken, so a backend slip blanks the numbers rather than
leaking them. **New dismissal reasons must ship in the frontend before the
backend sends them**, for the same reason.

## 10. Analysis, clearances and audit

Source: analysis design §2–6, §9; the clinical app's Plan 6
(`apps/clinical/lib/api/analysis-contract.ts`). Status: **Assumed** (Plan 6
built on sample data; clearances, hospital side: Plan 11a below; platform
verification and named drill-down: Plan 11b and 11c). Needs
clinical, privacy and legal sign-off before live use, because it changes
the rule that hospital admins see no clinical content.

### Analysis, as built in Plan 6

| Endpoint | Behaviour |
|---|---|
| `GET /analysis/filters` | the filter options: `{updated_at, conditions: ConditionRef[], locations: {id, label, setting}[], specialties: string[], sources: {id, label}[]}`. The app shows Source only when there is more than one |
| `GET /analysis/{view}?from&to&…` | one of `prevalence`, `affected`, `co-occurring`, `findings`, `quality`. Query: `from`, `to` (`YYYY-MM-DD`, required), and optional `condition`, `age_band` (`0-17`, `18-39`, `40-64`, `65-79`, `80+`), `sex` (`female`, `male`, `other`, `not_recorded`), `location`, `setting` (`admitted`, `outpatient`, `emergency`), `specialty`, `source`. Omitted means all. Unknown view: 404 `not_found` |
| `GET /analysis/patients?drill=…&level=deidentified&from&to&…` | the patients behind one shown count. `drill` is the opaque key from a `Drillable`; the same filters as the view. Only `deidentified` for now; `named` is a later plan (Plan 11) |

Shapes (ids are opaque, `[A-Za-z0-9_-]+`):

- `GuardedOut`: `{value: number | null, suppressed: boolean}`. Hidden is
  exactly `{value: null, suppressed: true}`. `value: null` with
  `suppressed: false` means "nothing to measure" and is allowed only for a
  share or a median.
- `Drillable`: a `GuardedOut` plus `drill: string | null`.
- `ConditionRef`: `{id, name: string | null, icd10: string | null}`. `name`
  null reads "Not standardised": uncoded records are counted, never dropped
  or guessed.
- Every view starts with `Meta`: `{updated_at, from, to, bucket: "week" |
  "month", patients: GuardedOut}`.
- `prevalence`: `total {cases, new, existing}`; `by_condition[] {condition,
  cases: Drillable, new, existing}`; `series[] {start, cases, new}` (`start`
  a day inside the range).
- `affected`: `by_age[] {band, patients}`, `by_sex[] {sex, patients}`,
  `by_setting[] {setting | null, patients}`, `by_location[] {location:
  {id, label, setting} | null, patients}`, `by_specialty[] {specialty |
  null, patients}`; every `patients` is a `Drillable`.
- `co-occurring`: top 20 `pairs[] {a, b, a_patients, patients: Drillable,
  share_of_a}`. `patients` is "with both"; a pair appears once, whichever
  way round.
- `findings`: `findings[] {name | null, flag: low | high | abnormal | null,
  patients: Drillable, results}`; `alerts {total, by_severity {critical,
  urgent, info}, median_ack_minutes, critical_median_ack_minutes,
  dismissed, dismiss_reasons[] {reason, count}}`. Reasons: `not_relevant`,
  `already_known`, `wrong_patient`, `other`.
- `quality`: `by_condition[] {condition, consultations, confidence {high,
  caution, review, failed}, agreement_rate (0 to 1 of rated), rated,
  corrections {total, approved, rejected, waiting}}`.
- Patients: `{level: "deidentified", count, patients[]}`; a row is exactly
  `{pid, age_band, sex, setting | null, conditions: ConditionRef[],
  findings: {name | null, flag | null, source_label | null}[]}`.

**Who and when**

- Hospital admins, and clinicians who also hold the `analyst` role, for
  their own hospital only. The hospital is never sent by the browser; scope
  by session. Platform accounts never use these endpoints.
- Two-step must be on: otherwise 403 `{code: "mfa_required"}`.
- `analyst` is a `StaffRole` beside `clinician` and `hospital_admin`. An
  analyst is always also a clinician. Granting and removing it are logged
  as `analyst_granted` and `analyst_removed`.
- The live gate: the user's `features.analysis` from `/me` must be true
  **and** the deployment's `ANALYSIS_SIGNED_OFF` must be exactly `true`. The
  BFF checks the second on every call and returns 404 `not_found` before it
  reads the session.

**The small-group rule (the server applies it; the app only checks)**

- Every count of 1–4 is hidden, in every field. 0 is sent as 0. A count is
  a whole number.
- A hidden value carries no number: `{value: null, suppressed: true}`.
- A share, rate or median over a hidden count is hidden. A share of a shown
  count must not give away a numerator, or its complement, of 1–4. Send
  shares at full precision, not rounded: a rounded share can stand for
  several counts, and the app cannot tell what it hides.
- A median is a whole number or a half (12.5).
- Order never reveals a hidden size: in a list, shown rows may be sorted by
  value, but hidden rows come after them in a fixed order (by name, with
  "Not standardised" and "not recorded" last), never by their true count.
  Age bands, sexes and settings always come in their fixed order (the app
  re-sorts them anyway).
- Secondary suppression: in every set that adds up, hide enough that no
  hidden count can be worked out. The app fails a set closed when exactly
  one member is hidden, when the total is hidden beside a shown member, or
  when the shown members leave 1–4 over (the total minus the shown members
  is under 5 while any member is hidden). The sets, as the app checks them:
  - `total.cases` against `new` + `existing`, and each condition row the
    same way;
  - each `series` point: `cases` against `new` (the rest is existing);
  - `patients` against each of `by_age`, `by_sex`, `by_setting`,
    `by_location`, `by_specialty` as its own set;
  - a co-occurring pair: `a_patients` against "both" (the rest is "first
    only"), nothing shown beside a hidden `a_patients`, and `share_of_a`
    hidden when either count is;
  - `alerts.total` against `critical` + `urgent` + `info`;
  - `alerts.total` against `dismissed` (the rest is not dismissed);
  - `dismissed` against its `dismiss_reasons`;
  - `consultations` against the four `confidence` levels;
  - `consultations` against `rated`, and `agreement_rate` hidden when
    `rated` or `consultations` is;
  - `corrections.total` against `approved` + `rejected` + `waiting`;
  - `median_ack_minutes` hidden when `alerts.total` is;
    `critical_median_ack_minutes` hidden when `total` or `critical` is.
- `patients` against `cases` is not a set: one patient can have several
  conditions.
- `drill` is set only on a shown count of 5 or more; otherwise null.
- No row repeats its key (condition id, band, day, pair, finding name and
  flag). Two rows for one group would let a viewer add a hidden group twice.
- The app fails a view closed ("Couldn't load {view}") on any violation,
  and never shows zeros in its place. Each view fails on its own. Unknown
  extra fields are dropped by the app, not shown.

**Patients (de-identified)**

- At most 200 rows; `count` is the number matched (5 or more, and not under
  the rows sent). No `pid` repeats. Rows carry exactly the keys above, and
  the app refuses the list if any other field appears (a name, an MRN, a
  date of birth, an address).
- `pid` matches `^P-[A-Z0-9]{4,12}$`: pseudonymous, stable within the
  hospital, never the MRN.
- Each call is audited as `analysis_patients_viewed`: type `security`,
  unflagged, with `count` (the rows sent). It appears in the hospital's
  activity.
- An unknown drill, or one that is hidden or under 5: 422 `too_small`. A
  level other than `deidentified`: 422 `level_not_supported`.

**Errors**

- 422 `invalid_range`: `from` or `to` is not a real day, `from` is after
  `to`, or the range is more than 24 months.
- 404 `not_found`: unknown view, or the gate is off. 503 `unavailable`:
  the app shows that view as failed and offers Try again.
- 403 `mfa_required`, as above.

**Not logged.** The filters travel in the query string; the BFF does not
log it, and neither should the gateway or the API. They are not kept in the
page URL or in browser storage. The patient list is never cached, exported
or printed.

### Clearances, named lists and audit: hospital side (Plan 11a)

Source: the clinical app's Plan 11a (`apps/clinical/lib/api/clearances-contract.ts`,
`mock-clearances.ts`, the BFF route). Status: **Assumed** (Plan 11a built on
sample data; platform verification is Plan 11b, §14.10; named rows in
Analysis are Plan 11c).

**The gate.** Named access is live for a person only when all three hold:
the user's `features.named_access` from the API is true, Analysis is on for
them, and the deployment has `NAMED_ACCESS_SIGNED_OFF=true`. The key can
only keep it off; the backend switch stays the authority.

- Key off: the BFF answers **404** for every clearance path
  (`/hospital/clearances…`, `/analysis/named-access`, `/analysis/clearance/…`)
  and **403** `named_access_off` for a named patient list.
- Key on, but the person lacks the gate: **403** `named_access_off`. Letters
  are for hospital admins with the gate; the position, your clearance and
  named lists are for anyone with it.
- Platform accounts are refused.

All writes carry an `Idempotency-Key` and the admin's two-step code
(`mfa_code` or `backup_code`). A replay returns the first reply and records
nothing again.

| Endpoint | Behaviour |
|---|---|
| `GET /hospital/clearances` | `{clearances, today, max_end_date}`. `today` and `max_end_date` are the hospital's days (`YYYY-MM-DD`), sent by the server so the browser does no time-zone arithmetic. `max_end_date` is today plus 12 months |
| `POST /hospital/clearances` | **Multipart only**: `file`, `signed_by`, `signer_title`, `letter_date`, `covered` (staff ids, repeated), `covered_platform` (emails, repeated, optional), `end_date`, `renews` (letter id, optional), `mfa_code` or `backup_code`. Body at most 10 MB + 64 KB (**413** `file_too_large`); not multipart (**415** `file_type`). Returns the letter, status `checking` |
| `GET /hospital/clearances/{id}` | One letter |
| `GET /hospital/clearances/{id}/pages/{n}` | One server-made page image, **PNG or JPEG only** (anything else is **502** `bad_gateway`), `Cache-Control: no-store`, `Content-Disposition: inline`. **409** `not_ready` while `pages` is 0. The original never reaches the browser. Each view is an admin audit event |
| `POST /hospital/clearances/{id}/revoke` | `{reason, note?, version, mfa_code}`. Takes effect at once |
| `POST /hospital/clearances/{id}/narrow` | `{remove: [ids from covered or covered_platform], reason, note?, version, mfa_code}`. Removes covered people, hospital or platform |

**The letter** (`ClearanceOut`): `id`, `status` (`checking`, `refused`,
`waiting`, `active`, `rejected`, `ended`, `revoked`), `refused_reason`,
`signed_by`, `signer_title`, `letter_date`, `covered` (`{id, display_name}`),
`covered_platform` (`{id, display_name}`; Measured Health staff the letter
names, entered as emails on upload), `end_date`, `uploaded_by`, `uploaded_at`,
`verified_by`, `verified_at`, `rejection`, `revocation`, `renews`, `pages`
(0 until ready), `history` and `version`.

- `renews` is the id of the letter this one replaces, or null.
- `revocation` is `{by, side, at, day, reason, note}`. **`day` is required**
  with every revocation: `at` as the hospital's `YYYY-MM-DD` day.
- `version` rises on every change. Revoke and narrow send it; a letter
  changed since is **409** `conflict`. Narrow requires it; revoke may omit it
  but the clinical app always sends it.

**Refusals**

- **422** `would_empty` on narrow: removing everyone is a revocation. The
  server still ends a letter's access when its last covered person loses
  their role.
- **422** `not_eligible` with `ids` (staff ids), and **422**
  `not_eligible_platform` with `emails`. Each list is at most 20 entries.
  The BFF forwards only these two lists, checked (ids match
  `[A-Za-z0-9_-]+`, emails are well formed), and otherwise only `{code}`.
- **422** `end_date_too_late` past `max_end_date`; **422** `invalid` for bad
  shapes; **403** `mfa_invalid` / **429** `mfa_locked` for the code.
- **404** `not_found` for an unknown letter or page.

**Who is covered.** Only a verified, unexpired, unrevoked letter counts. An
"active" letter whose `end_date` has passed counts as ended: the server ends
it at the end of that day in the hospital's zone and sets `status: ended`.
A person can be named on a letter only if they are eligible: `status` active,
role `hospital_admin` or `analyst`, and two-step set up (`issues` has no
`mfa_not_set_up`). The server has the last word (`not_eligible`).

**Audit.**

- A named view of patients is a **security** event: the count of rows opened
  and the `purpose`; never names, MRNs or the free-text note.
- Other clearance events are **admin** events: `clearance_uploaded`,
  `clearance_refused`, `clearance_verified`, `clearance_rejected`,
  `clearance_narrowed` (one per person removed), `clearance_revoked`,
  `clearance_ended`, `clearance_viewed` (page images) and
  `clearance_reminder_sent`. They name people only by `display_name`, with
  no free text.
- The frontend tolerates unknown kinds and shows them generically.

### Clearances, platform side and named lists (later: Plan 11b and 11c)

| Endpoint | Behaviour |
|---|---|
| `GET /analysis/patients?…&level=named` | adds name and MRN, only with a verified, unexpired clearance for the caller. Every call audited; named calls flagged as security events |
| `POST /hospital/clearances` | multipart: the letter (PDF or image), covered people, end date (capped at 12 months) |
| `GET /hospital/clearances` | the hospital's clearances and their status |
| `POST /hospital/clearances/{id}/revoke` | hospital admin or platform admin; takes effect at once |
| platform admin: verify, reject | two-person control: a hospital admin uploads, a platform admin verifies before names unlock |
| `GET /audit/named-access?…` and export with letters | platform admin and auditor |

- Who: hospital admins and `analyst` holders, own hospital; platform
  admins, all hospitals. Auditors read the audit, not the analysis.
- Filters include `source` (by `source_label`) when a hospital has more
  than one connected system. Unmapped codes count under "Not
  standardised".
- Reminder to the hospital admin 30 days before a clearance ends.
- The threshold (5) and the 12-month cap are constants only Measured
  Health can change.
- Letters and audit records follow the retention rules; letters are kept
  at least as long as the audit records that cite them.
- Named lists can't be exported.

## 11. Notifications for correction decisions

Source: UI design §10 item 1; notifications design §2. The bell counts
unread alerts plus decisions on your corrections. Polling
`GET /corrections/mine` is enough at first; a combined unread count may
follow. Status: **Assumed**.

## 12. EMR integration

Source: `docs/adr/0003-many-emr-systems.md` (mhai-web); notifications
design §8; analysis design §3. The backend decision is
[ADR 0009](../adr/0009-integrate-many-emr-systems.md). What the API owes the
frontend:

- only MHAI's canonical shape; no vendor names, codes or formats;
- `source_label` (hospital-chosen) on alerts, findings and analysis
  filters;
- `emr_link` on alerts and findings, built by the backend;
- codes, units and locations already normalised, with display labels;
  `standardised: false` for unmapped findings; a missing location as
  "not recorded";
- connection status for hospital admins, read-only, by label (shape:
  **New**); connection setup in the platform admin app, driven by the
  fields each connector declares (shape: **New**).

## 13. Conflicts with the gateway design

Each needs a decision, then an update to the gateway spec or this list.

Decided (design 2026-10-26 §8a): where the two contracts disagree, the
frontend contract wins. For example, `mfa_required` and `mfa_invalid`
return 403 (item 3).

1. **One role or many:** the gateway has one `role` per user and JWT; the
   frontend needs `roles` and combinable `clinician`, `hospital_admin`
   and `analyst`.
2. **Admin separation:** the gateway says admins can't call clinical
   endpoints and need a second account; that stays true for platform
   admins, but `hospital_admin` + `clinician` is one account.
3. **MFA status codes:** the gateway catalogue has `mfa_required` and
   `mfa_invalid` as 401; the frontend expects 403 on admin routes and on
   staff removal.
4. **Missing error codes:** `mfa_locked` (429), `already_decided`,
   `self_change`, `last_admin`, `conflict` (409) and `not_eligible` (422)
   aren't in the catalogue; the gateway has a generic `invalid_state`.
5. **403 versus 404:** the gateway returns 404 for someone else's
   resource; the frontend expects 403 `forbidden` when withdrawing another
   clinician's correction.
6. **Login name:** the gateway uses `username`; the frontend wants a work
   email, and the `users` table has no email.
7. **Correction states:** the gateway has `pending_review` and `approved`
   only; the frontend adds `rejected` and `withdrawn`, people objects
   instead of user ids, and 204 on approve.
8. **Idempotency:** the gateway takes `Idempotency-Key` only on DLQ retry;
   the frontend needs it on every write, kept 72 hours.
9. **Clinician two-step:** optional in the gateway; a per-hospital policy
   in the frontend.
10. **User management:** the gateway gives it to platform admins only
    (`/admin/users`, no delete); the frontend has hospital admins invite,
    reset, deactivate and delete staff.
11. **SSO admins:** the gateway requires IdP MFA for admin SSO logins;
    the staff-removal rule needs an MHAI two-step or backup code, which an
    SSO-only admin may not have.
12. **Patient data on admin and alert routes:** the gateway keeps admin
    responses to ids and counts; alerts and named drill-down return
    patient names and MRNs.
13. **EMR data:** the gateway has no hospitals, no EMR ingest and no
    alerts; ADR 0009 adds them.
14. **Platform admin audience:** the platform admin app sends
    `X-MHAI-Audience: platform` on every call and needs the `/platform/*`
    endpoints in §14; the gateway has neither.

## 14. Platform admin app

Source: mhai-web `docs/specs/2026-10-17-platform-admin-design.md` §13
(the contract) and §16 (what changed while building it). Status:
**Assumed** (Plan 7b). The app is separate from the clinical app, on its
own origin (`platforms.<domain>`) behind an IP allow list, for Measured
Health platform admins and auditors. No endpoint returns patient data.

### 14.1 Who can use it

- Every call (sign-in and API) sends `X-MHAI-Audience: platform`.
- The login `user` carries `roles` and `mfa_enabled`. The role `admin`
  (today's name) or `platform_admin` means platform admin; `auditor` means
  auditor. Any other account gets **403 `not_platform`** at sign-in.
- Two-step is always required (**403 `mfa_required`**). Auditors are
  refused every write (**403 `forbidden`**).
- Every id (hospitals, team members, invites, events) is opaque and
  matches `[A-Za-z0-9_-]+`. A team member's id is never an email or
  username.

### 14.2 Hospitals

| Endpoint | Behaviour |
|---|---|
| `GET /platform/hospitals?status&q&cursor` | → `{hospitals, next_cursor}`; each has `id`, `name`, `short_code`, `status` (`active`, `setup_incomplete`, `deactivated`, `removal_pending`, `removal_scheduled`, `removed`), `staff_count`, `sso_enabled`, `created_at`, `last_activity_at` |
| `POST /platform/hospitals {name, short_code, country, time_zone, email_domains, first_admin: {name, email}}` | → the hospital, `setup_incomplete` with a pending first-admin invite. **409 `duplicate`** for a used short code |
| `GET /platform/hospitals/{id}` | → the hospital, plus `country`, `time_zone`, `email_domains`, `version`, `deactivation` (`by`, `at`, `reason`, `note`), `first_admin` (`invite_id`, `name`, `email`, `status`, `sent_at`, `expires_at`), `removal` (§14.3) |
| `PATCH /platform/hospitals/{id} {version, …details}` | → the hospital; **409 `conflict`** when changed meanwhile |
| `POST /platform/hospitals/{id}/deactivate {reason, note?, mfa_code \| backup_code}` | `reason` one of `contract_ended`, `security_concern`, `hospital_request`, `other` (note required). Signs all staff out; pending invites stop working; no data deleted |
| `POST /platform/hospitals/{id}/reactivate {reason, mfa_code \| backup_code}` | staff can sign in again; old sessions aren't revived |
| `POST /platform/hospitals/{id}/invites {name, email}` | invites the first hospital admin (7 days) |
| `POST /platform/hospitals/{id}/invites/{invite_id}/resend` | 7 more days |

### 14.3 Removal (two people)

| Endpoint | Behaviour |
|---|---|
| `POST /platform/hospitals/{id}/removal {reason, mfa_code \| backup_code}` | only while `deactivated`. → the hospital with `removal`: `status`, `requested_by`, `requested_at`, `reason`, `approved_by`, `approved_at`, `checks` (`legal_holds`, `retention_until`, `export_required`, `export_done`), `blocking` (`legal_hold`, `retention`, `export_required`) and `requested_by_you` (boolean, decided by the server for the signed-in person) |
| `POST /platform/hospitals/{id}/removal/approve {mfa_code \| backup_code}` | a different platform admin. **409 `same_person`** for the requester; **409 `blocked`** with `{blocking}` while a check blocks; then `removal_scheduled`, and `removed` once done (only the audit trail remains) |
| `POST /platform/hospitals/{id}/removal/cancel` | back to `deactivated` |

### 14.4 Single sign-on

| Endpoint | Behaviour |
|---|---|
| `GET /platform/hospitals/{id}/sso` | `enabled`, `sign_in_url`, `client_id`, `issuer`, `allowed_domains`, `secret_saved`, `secret_ends` (last 4 characters only), `last_test` (`at`, `ok`, `message`), `version` |
| `PUT /platform/hospitals/{id}/sso {…, client_secret?, version}` | the secret is write-only. Resets `last_test` whenever the address, issuer, client id, domains or secret change, so a changed set-up must be tested before it can be switched on |
| `POST /platform/hospitals/{id}/sso/test` | tests the saved set-up → the SSO object |

**New:** testing a draft set-up before saving it, so a live set-up can
change without switching sign-on off for the hospital.

### 14.5 Team and activity

| Endpoint | Behaviour |
|---|---|
| `GET /platform/team` | → `{members}`: `id`, `name`, `email`, `role` (`platform_admin`, `auditor`), `status` (`active`, `invite_pending`), `mfa_enabled`, `last_sign_in_at`, `is_you`, `version` |
| `POST /platform/team/invites {name, email, role}` | → the member |
| `PATCH /platform/team/{id} {role, version}` | **409 `conflict`**, **409 `last_admin`**, **409 `own_role`** |
| `DELETE /platform/team/{id}` | body `{mfa_code}` or `{backup_code}` → 204. **409 `last_admin`** |
| `GET /platform/activity?hospital&cursor` | → `{events, next_cursor}`, newest first; each `id`, `at`, `actor`, `action`, `hospital`, `target`, `detail` |

`PATCH` and `DELETE` on `/platform/team/invites` are reserved: `invites`
is never a member id.

### 14.6 On every platform endpoint

- Every write honours `Idempotency-Key` and is audited (who, when, what,
  which hospital).
- A step-up action takes `mfa_code` or `backup_code`. A wrong code is
  **403 `mfa_invalid`**; too many wrong codes **429 `mfa_locked`** (15
  minutes).
- Errors carry only `code` (plus `blocking` on **409 `blocked`**).

### 14.7 Analytics across hospitals

Source: mhai-web `docs/specs/2026-10-22-platform-analytics-design.md` §5–§7.
Status: **Assumed** (Plan 7c; Plan 7d for notifications). Counts only: no
patient names, MRNs, question or answer text, nothing per person. A guarded
value is `{value, suppressed}`: hidden is `{value: null, suppressed: true}`,
nothing to show is `{value: null, suppressed: false}`.

| Endpoint | Behaviour |
|---|---|
| `GET /platform/analytics?period=7d\|30d\|90d` | → `{period, from, to, changes, use, alerts, quality, hospitals}`; each area is its data with its own `updated_at`, or `{unavailable: true}`. Active hospitals only |
| | `changes`: `{updated_at, window: {from, to}, items}`, ranked, always the last 7 days vs the 4 weeks before (whatever `period` is). Each item: `id` (stable while the change lasts), `hospital` (`{id, name}` or null for all hospitals), `area` (`use`, `alerts`, `quality`), `measure`, `direction` (`worse`, `better`, `rising`, `falling`), `serious` (Critical-type), `previous`, `current` (guarded), `summary` (plain words), `follow_up` (`{by, at, note, by_you, version}` or null), `version` |
| | `use`, `alerts`, `quality`: `{updated_at, rows, median, trend}`; one row per active hospital, all values guarded; `median` is the all-hospitals median under the same rules; `trend` is `[{start, value}]` with `value` guarded. Alert rows add `critical_target_minutes` and `critical_overdue` (unhandled Critical alerts past target, guarded); quality rows add `reviews` |
| | `hospitals`: one row each with `size` (`staff`, `active_clinicians`, `questions`) |
| `GET /platform/analytics/changes/{id}/series?period` | → `{measure, hospital, points: [{start, value, median}]}`; `value` guarded; `median` of the other active hospitals, or null when there is none |
| `GET /platform/analytics/hospitals/{id}/series?measure&period` | the same for any active hospital and one of `questions_per_clinician`, `critical_median_ack_minutes`, `critical_within_target`, `needs_review_share` |
| `PUT /platform/analytics/changes/{id}/follow-up {note?, version}` | platform admins; marks followed up or edits the note (≤ 500 characters, author only — others get 403); stale `version` → **409 `conflict`**; audited; → the change |
| `DELETE /platform/analytics/changes/{id}/follow-up {version}` | any platform admin undoes; stale or nothing to undo → **409 `conflict`**; audited |

`GET /platform/hospitals/{id}` (§14.2) gains `admins: [{name, email}]`, the
active hospital admins.

**Server duties** (the app fails an area closed when it can tell, but only
the server can keep these):

- Every count of 1–4 is sent hidden, including staff-activity counts across
  hospitals.
- A share or rate is hidden when its numerator or its complement (total minus
  numerator) is 1–4, including subsets (total − unhandled, Critical − Critical
  overdue, answers − reviews). Shares arrive rounded, so the app's own check
  is only a backstop, and it can't check series shares.
- A series median of a count measure between 0 and 5 is sent as null.
- When a change's `previous` or `current` is hidden, its `summary` says
  "fewer than 5", has no "%", and no digits other than the shown side's
  number (a hospital or measure name with a digit fails it closed in the app).
- What changed always includes a serious change for any hospital with
  `critical_overdue` above 0; otherwise the app shows "Couldn't load what
  changed", never "Nothing unusual".

**Plan 7d (notifications):** `GET /platform/notifications` → `{items, unseen}`, at most 20 newest first, where `unseen` counts **all** of this person's unseen notifications (it may be more than the items returned); each item `{id, change_id, hospital, measure, previous, current, summary, serious, direction, at, seen}` with `previous`/`current` guarded exactly as on the change (the app checks the summary with the same rules as What changed; `seen` is per person), `POST
/platform/notifications/seen`, `GET`/`PUT /platform/me/analytics-email
{mode, version}` (stale `version` → 409 `conflict`; auditors 403; `POST …/seen {ids?}` — omitted `ids` marks all — is the one write auditors may make) (`off`, `critical`, `critical_and_digest`; platform admins only). The
backend sends the emails: straight away for `serious` changes, at most one per
hospital per measure per day; a weekly digest on Monday 08:00 Lagos time;
plain words and a link, no patient details, no count under 5.
`Notification` has a `kind`: `analytics_change` or `audit_integrity` (a
missing `kind` is read as `analytics_change`). An `audit_integrity` item is
`{kind, id, run_id, first_broken_seq, first_broken_id, at, seen}`; it is sent
when an audit-log check comes back Broken (§14.9), and `seen` is per person.
The app polls `GET /platform/notifications` every 60 s while the tab is
visible. Its BFF sends that read with `X-MHAI-Background: 1` and does not
count it as activity for the 15-minute idle limit; the API should not either
(the BFF does not forward the header, so a poll must not refresh any
server-side idle clock the API keeps).

### 14.8 Platform operations: legal holds, system health, settings

Source: mhai-web `docs/specs/2026-10-22-platform-operations-design.md` §9,
as built (`apps/platform/lib/api/holds-contract.ts`, `ops-contract.ts`) and
as ruled while building. Status: **Assumed** (Plan 8a built on sample data;
8b and 8c built). The `/platform/*` shapes below are asked for instead
of the gateway's `/admin/*` ones; the backend may build them on the existing
handlers. Hold notes, job subjects and users, and error messages never
appear in a list, in Activity or in a health reading.

**Who and how**

- Platform admins read and write. Auditors read every `GET` below and are
  refused every write (**403 `forbidden`**) before anything reaches the
  upstream. Two-step is always required.
- The named writes take `mfa_code` or `backup_code` (**403 `mfa_invalid`**,
  **429 `mfa_locked`**): placing a hold, requesting and approving a lift,
  undoing a lift, cancelling jobs, and the settings writes in 14.8.4.
  Editing a hold, cancelling a lift request and retrying jobs take none.
- **Every write sends an `Idempotency-Key`** and is audited.
- Errors carry only `code`. Ids are opaque (`[A-Za-z0-9_-]+`).

#### 14.8.1 Legal holds

| Endpoint | Behaviour |
|---|---|
| `GET /platform/legal-holds?status&hospital&q&cursor` | → `{holds, next_cursor}`, newest first. `status` is `open` (default: active, review due, lift requested), `active`, `review_due`, `lift_requested`, `lifted` or `all`. `q` is part of the reference, any case |
| `GET /platform/legal-holds/{id}` | → the hold plus `history` (newest first): `at`, `actor`, `action` (`placed`, `edited`, `lift_requested`, `lift_cancelled`, `lift_lapsed`, `lifted`, `lift_undone`, `released`), `detail` |
| `POST /platform/legal-holds {hospital_id, from?, to?, reason, note?, reference, asked_by, review_on, mfa_code \| backup_code}` | → the hold. **409 `hospital_removed`**, **409 `removal_started`**, **422 `invalid_range`**; `to` without `from` is **422 `invalid`** |
| `PATCH /platform/legal-holds/{id} {review_on?, note?, version}` | no step-up. Only these two fields; any other field is **422 `invalid`**. All or nothing: if any part is invalid, none is applied. **409 `conflict`** on a stale `version` or a lifted hold |

A hold: `id`, `hospital` (`{id, name}`), `scope` (`{from, to}` calendar
dates, inclusive; both null means all records), `reason` (`court_case`,
`regulator`, `complaint`, `investigation`, `other`; `other` needs a note),
`reference`, `asked_by`, `note`, `placed_by`, `placed_at`, `review_on`,
`status` (`active`, `review_due`, `lift_requested`, `lifted`), `lift`
(below) or null, `stopped_removal` (placing it sent a scheduled removal back
to pending) and `version`. Scope, reason and reference never change after
placing. `review_due` is set by the server from `review_on`; a hold never
lifts itself and is never deleted.

**Two-person lift.**

| Endpoint | Behaviour |
|---|---|
| `POST …/{id}/lift {reason, note?, authority, mfa_code \| backup_code}` | `reason`: `case_closed`, `withdrawn`, `mistake`, `other`. → the hold, `lift_requested`. `lift`: `kind` (`hold_lift`), `requested_by`, `requested_at`, `reason`, `note`, `authority`, `requested_by_you`, `expires_at`, `records_due` (records past retention that only this hold keeps), `due_after`, `approved_by`, `approved_at`, `released_on` |
| `POST …/{id}/lift/approve {version, mfa_code \| backup_code}` | a different platform admin → `lifted`; `released_on` is 7 days on |
| `POST …/{id}/lift/cancel {version}` | either platform admin; back to `active` or `review_due` |
| `POST …/{id}/lift/undo {version, mfa_code \| backup_code}` | only before `released_on`; → `active`, or `review_due` if the review date has passed |

- `requested_by_you` is decided by the server for the signed-in person and
  is the only thing that decides who may approve: **409 `same_person`** for
  the requester.
- A request lapses 7 days after `requested_at` (`expires_at`): the hold goes
  back to `active` (or `review_due`), history gets `lift_lapsed`, and an
  approval is **409 `expired`**.
- Approve, cancel and undo send the hold's `version` as the person saw it,
  so they act only on that request. A mismatch (for example, the request
  was cancelled and a new one made meanwhile) is **409 `conflict`**
  ("Changed by someone else"). Undo after `released_on` is **409 `conflict`**.

**Hospitals.** `GET /platform/hospitals` items gain `legal_holds_active`;
`GET /platform/hospitals/{id}` gains `legal_holds` (short form: `id`,
`scope`, `reference`, `status`, `review_on`). Despite the name,
`legal_holds_active` counts the holds that **keep records**: active, review
due, lift requested, or lifted but before `released_on` (the 7-day grace
period). `removal.checks.legal_holds` (§14.3) counts the same set.

**Server duties** (the app can't keep these):

- The daily clean-up and every other deletion path (removal, a future
  erasure request) leave out every record covered by a hold that keeps
  records, in the deletion query itself.
- Removal blocking is checked at every read, not stored at request time:
  `removal.checks` and `blocking` reflect holds as they stand, including a
  hold placed after the removal was requested. The removal re-checks just
  before deleting. Placing a hold on a `removal_scheduled` hospital that
  hasn't started stops it (back to `removal_pending`, blocked).
- Activity and history carry a hold's reference and the reason in words,
  **never its note**.
- Every change is audited.

#### 14.8.2 System health

`GET /platform/health` → `{updated_at, overall, parts, queue, failed_jobs,
timing, errors, versions, triage, clean_up}`. No hospital, user or content
anywhere in it.

- `overall` and each part's `status`: `ok`, `slow`, `problem` (the server's
  thresholds). `parts`: `[{key, status, since}]`, `key` one of `app`,
  `workers`, `evidence`, `documents`, `guidelines`, `clean_up`.
- `queue`: `{waiting, oldest_wait_s}` (null when empty). `failed_jobs`:
  `{now, last_24h}`.
- `timing`: `{last_hour, last_24h}`, each `{overall: {p50_ms, p95_ms},
  steps: [{step, p50_ms, p95_ms}]}`, `step` one of `privacy`, `guidelines`,
  `answer`, `evidence`.
- `errors`: `[{kind, last_hour, last_24h}]`, `kind` one of `timeout`,
  `unreachable`, `unreadable`, `invalid`, `other`.
- `versions`: `[{part, label}]`. `triage`: `{enabled}`. `clean_up`:
  `{last_run_at, ok, paused, paused_by, paused_at}`.
- **503** when the reading can't be had.

The app fails closed: a failed reading, or one whose `updated_at` is more
than 2 minutes old, counts as no reading ("Couldn't check", never zeros). A
block with the wrong shape, or an enum value the app doesn't know (a new
status, key, step or kind), fails on its own. Send `updated_at` from the
moment the figures were taken, not the moment of the request.

The app polls every 30 s while the tab is visible. The poll sends
`X-MHAI-Background: 1`; the BFF does not count it as activity for the
15-minute idle limit and does not forward the header, so the API must not
let a health poll refresh any server-side idle clock either.

#### 14.8.3 Failed jobs

| Endpoint | Behaviour |
|---|---|
| `GET /platform/jobs?status&type&hospital&kind&cursor` | → `{jobs, next_cursor}`, newest failure first. `status` is `failed` (default), `retrying`, `retried`, `cancelled` or `all` |
| `POST /platform/jobs/retry {ids}` | 1–50 ids, no step-up. → `{retried, skipped}`, each an array of ids |
| `POST /platform/jobs/cancel {ids, reason, note?, mfa_code \| backup_code}` | 1–50 ids. `reason`: `no_longer_needed`, `cant_succeed`, `other` (`other` needs a note; note ≤ 500 characters). → `{cancelled, skipped}` |

A job: `id`, `type` (`run_query`, `submit_feedback`, `approve_correction`,
`ingest_document`, `run_triage`, `kb_search`, `feedback_buffer_reset`,
`retention_purge`), `hospital` (`{id, name}` or null), `created_at`,
`failed_at`, `attempts`, `kind` (as `errors`), `status` (`failed`,
`retrying`, `retried`, `cancelled`), `cancelled_by`, `cancelled_at`. Never
the subject, the user or the error message.

- `retried` means **attempted**, including a job that failed again (an
  unreadable document stays `failed` with `attempts` one higher). `skipped`
  holds ids that are unknown or no longer `failed`; neither is an error.
  Cancel skips the same way.
- A cancelled job's subject moves to its failed state (a question shows
  "Couldn't complete"). Retried and cancelled jobs stay listed for 30 days.
- The cancel `note` is optional detail (required for `other`). It is stored
  with the cancellation but **never logged**. Activity shows the reason in
  words (for example "No longer needed"), with the count of jobs.
- A bad id list (empty, over 50) or a bad reason is **422 `invalid`**.

#### 14.8.4 Settings

Built on sample data (Plan 8c): `apps/platform/lib/api/settings-contract.ts`
and `mock-settings.ts`, as spec §9 and the rulings while building.

| Endpoint | Behaviour |
|---|---|
| `GET /platform/settings` | → `SettingsOut`: `{retention, triage, feedback_buffer, pending}` |
| `GET /platform/settings/retention/{type}/preview?days` | → `{would_delete}` |
| `PUT /platform/settings/retention/{type} {days, reason, mfa_code \| backup_code, version}` | longer applies at once (**200**); shorter returns **202** with the pending request; **422 `below_minimum`**; **409 `conflict`** on a stale `version` |
| `PUT /platform/settings/retention-pause {paused, reason, mfa_code \| backup_code, version}` | `version` is `retention.version`, the pause's own; **409 `conflict`** on a mismatch |
| `PUT /platform/settings/triage {enabled, reason, clinical_signoff?, mfa_code \| backup_code, version}` | off applies at once; on needs `clinical_signoff` and returns **202** with the pending request; **409 `not_ready`** (re-checked at approval) |
| `POST /platform/settings/requests/{id}/approve {version, mfa_code \| backup_code}` | a different platform admin: **409 `same_person`**, **409 `expired`**, **409 `conflict`** |
| `POST /platform/settings/requests/{id}/cancel {version}` | either platform admin; no step-up |
| `POST /platform/settings/feedback-buffer/reset {reason, mfa_code \| backup_code}` | → **202** `{job_id}`; no confirm word |

- `retention`: `{paused, paused_by, paused_at, confirmed, version, items:
  [{type, days, minimum_days, last_run: {at, ok, deleted, kept_for_holds},
  next_run_at, changed_by, changed_at, version}]}`, `type` one of `records`,
  `documents`, `pending_corrections`, `audit_log`; `confirmed` is false until
  the compliance lead has confirmed the periods. `retention.version` is the
  pause's own; every pause and resume bumps it. Each item's `version` is that
  period's.
- `triage`: `{enabled, ready, not_ready_reason, changed_by, changed_at,
  version}`; `not_ready_reason` one of `missing_report`, `missing_files`,
  `checksum_mismatch` (null when ready).
- `feedback_buffer`: `{waiting, running: {job_id, started_at} | null,
  failed_job_id, last_reset: {by, at, reason, cleared}}`.
- `pending`: the two-person requests, each `{id, kind (`retention_shorten`,
  `triage_on`), requested_by, requested_at, reason, requested_by_you,
  expires_at, version, from, to}`, plus `type` on a `retention_shorten` and
  `clinical_signoff` on a `triage_on`. `version` is the version of the
  setting the request is about (the retention item's, or the triage's). Same
  rules as the lift: `requested_by_you` is server-decided, requests lapse
  after 7 days.

**Versions (the controller's rulings, for the same reason as the lift)**

- Approve and cancel send the request's `version`. A mismatch is **409
  `conflict`**, and so is a request already approved, cancelled or
  superseded, so they act only on the request the person saw. A lapsed
  request is still **409 `expired`** on approval.
- Pause and resume send `retention.version`; a mismatch is **409
  `conflict`**, so a stale Resume can't lift another admin's newer pause.
- Creating, approving, cancelling or lapsing a request bumps the setting's
  `version`.

**Responses**

- Every settings write answers with the whole `SettingsOut` (the `GET`
  shape): **200** if applied, **202** if a pending request. Only the reset
  differs: **202** `{job_id}`.

**Retention rules and refusals**

- A longer period applies at once and supersedes a pending shortening of
  the same type (that request can no longer be approved).
- **422 `invalid`**: a period equal to the current one; a missing or
  malformed field. A wrong code is **403 `mfa_invalid`**.
- **409 `conflict`**: a second pending shortening for the same type; triage
  on when already on or already requested; triage off when already off;
  pausing when already paused, or resuming when not paused; a reset while
  one is running.
- **404** for an unknown retention `type` or request id.
- The daily clean-up runs at **02:00 Lagos (01:00 UTC)**, which is when
  `next_run_at` falls.
- Hold-kept records are excluded from every retention run (14.8.1);
  `kept_for_holds` reports how many.

**Clearing learning data**

- `running` is set while a reset is under way and cleared when it ends.
- A failed reset sets `failed_job_id` to the reset's job. That job (type
  `feedback_buffer_reset`) appears in the failed jobs list (14.8.3).
- The settings line follows that job's state: once it is retried or
  cancelled in System, `failed_job_id` is null again. A started reset clears
  a stale `failed_job_id`.
- A successful reset sets `last_reset` and zeroes `waiting`.

### 14.9 Audit log

Source: mhai-web `docs/specs/2026-10-22-platform-audit-log-design.md` §13, as
built (`apps/platform/lib/api/audit-contract.ts`, `live-audit.ts`) and as
ruled while building (Plan 9). Status: **Assumed** (Plan 9 built on sample
data). These are the platform-audience forms of the gateway's
`GET /security/audit` and `GET /security/audit/verify`, over the same log. No
patient data in any entry, field or file; patients appear only as pseudonyms,
and only through the recorded reveal below. Ids are opaque
(`[A-Za-z0-9_-]+`); errors carry only `code`.

**Who and how**

- Platform admins and auditors read everything below. Two-step is always
  required.
- Auditors may make four audit writes: report an undisplayable entry,
  create an export, start a check, and add an outcome note to a broken
  check. Marking notifications seen (§14.7) stays theirs too. Every other
  write is **403 `forbidden`**. Only the outcome note is auditors alone.
- **Every write sends an `Idempotency-Key`** and is audited. A step-up
  action takes `mfa_code` or `backup_code` (§14.6).

**Events**

| Endpoint | Behaviour |
|---|---|
| `GET /platform/audit?hospital&person&type&outcome&flagged&from&to&q&cursor&limit` | → `{events, next_cursor, total, total_capped, oldest_kept_at, last_checkpoint}`. `hospital=platform` selects platform-only events. `type` is a comma list. `from`/`to` are instants, `[from, to)`. The app sends `limit=50` |
| `GET /platform/audit/{id}` | → the event plus `patients: {count}` (null when there is no drill-down) and `letter` (null unless a named-access event) |
| `GET /platform/audit/{id}/patients` | → `{pseudonyms}`. The only place a pseudonym is sent. Writes `audit_patients_viewed` |
| `GET /platform/audit/people?q` | `q` of 2 or more characters → `{people}`, at most 20, each `{id, name, email, role, kind, hospital}`. Backs the person picker |
| `GET /platform/audit/hospitals` | → `{hospitals: [{id, name, removed}]}`: every hospital the log names, removed ones included |
| `POST /platform/audit/undisplayable {ids}` | 1 to 50 ids → 204, recorded as `audit_undisplayable_reported`. The app reports each entry it can't display once |
| `GET /platform/audit/named-access?hospital&person&clearance&from&to&cursor` | → `{events, next_cursor}`: named drill-down events, each with its `letter` (`{id, status, valid_from, valid_to, verified_by}`) |

An event: `id`, `seq`, `hash` (64 hex), `at`, `actor` (`{id, name, role,
kind}`; `kind` is `staff`, `platform` or `system`), `hospital` (`{id, name,
removed}` or null), `type`, `kind`, `target` (`{kind, id, name?}` or null),
`outcome` (`succeeded`, `refused`, `failed`), `ip`, `device`, `flag`
(`security`, `policy`, null), `reason`, `note` (only on kinds that allow one),
`refs` (`clearance_id`, `export_id`, `request_id`, each or null), `facts` and
`after_break` (boolean: the entry is after the first broken one).

- **`facts`:** typed, optional, structured values that replace any free text.
  The keys are `specialty`, `confidence`, `view`, `location_type`, `from`,
  `to`, `patient_count`, `purpose`, `purged_count`, `before`, `checkpoint_id`,
  `incident_ref`, `filters`, `searched`, `searched_kind`, `rows`, `capped`,
  `checksum`, `state`, `first_broken_seq`, `list` and `job_count`. `view` and
  `location_type` are codes matching `^[a-z][a-z_]{0,39}$`.
  `jobs_retried` and `jobs_cancelled` (§14.8.3) carry no target; the count,
  when sent, is `facts.job_count`.
- **Unknown values fail closed.** An unknown `type`, `kind`, `reason`, role,
  outcome, letter status or `facts` key, or a key the app doesn't know at any
  level, makes the entry "can't be displayed": the app shows only its id and
  position, reports it once through `undisplayable`, and carries on. A new
  kind, type or `facts` key therefore needs a matching app change first.

**Reads are recorded**

- The first page of an applied filter set writes **one `audit_viewed`** entry
  per person per list per page load. Load more (a request with a `cursor`)
  writes none. Events and `named-access` dedupe separately (the key is the
  list and the filters), and each `audit_viewed` names its list in
  `facts.list`: `"events"` or `"named_access"`. Opening Named access is
  recorded even when its filters equal the Events filters.
- An exact-id search (`q`) **ignores the other filters**: the app sends none
  with it, and the server answers on `q` alone. It writes **`audit_searched`**
  instead, with `facts.searched` and `facts.searched_kind` (`event`,
  `request`, `export`, `clearance`, `pseudonym`).
- Every read, search, export and check is itself an entry. The log stays
  append-only: no endpoint changes or removes an entry.

**Pseudonyms never leave the server except through `/patients`**

- A pseudonym is the same pseudonymous patient id as Analysis (§10):
  `^P-[A-Z0-9]{4,12}$`, stable within the hospital, never the MRN. No other
  id may match that shape.
- A patient target is `{kind: "patient"}` with no id and no name.
- Where a pseudonym was searched or filtered on, the server sends the marker
  `{kind: "patient_id"}` in its place: as `facts.searched` (with
  `searched_kind: "pseudonym"`) and as `filters.q`, both in an entry's
  `facts.filters` and in an export record's `filters`.
- A pseudonym-search entry's detail answers `patients: {count: 1}`, and its
  `/patients` returns the pseudonym searched, so **Show patient ids** reveals
  it, recorded.
- The app fails closed: an entry or export record with a pseudonym anywhere
  (any field, any depth) is "can't be displayed".

**Integrity**

| Endpoint | Behaviour |
|---|---|
| `POST /platform/audit/verify` | → `{run_id}`; returns the running one if there is one. Platform admins and auditors |
| `GET /platform/audit/verify/runs?cursor` | → `{runs, next_cursor}` |
| `GET /platform/audit/verify/runs/{id}` | → the run |
| `GET /platform/audit/verify/status` | → `{latest, running, open_failure, security_contact: {name, email}, oldest_kept_at}`. One small read for the header, the footer and the home banner. The server decides `open_failure` (spec §8.3: it clears only when a check after a re-anchor is Intact and the outcome note exists) |
| `POST /platform/audit/verify/runs/{id}/outcome {note, version}` | **auditors only**: platform admins get **403 `forbidden`**. Broken runs only. `note` up to 1000 characters. → the run. The note is a new entry; the failed check is never changed |

A run: `id`, `started_at`, `finished_at`, `by` (person, or null for the daily
schedule), `state` (`running`, `intact`, `broken`, `error`), `entries_checked`,
`from_checkpoint`, `from_first`, `to_seq`, `to_at`, `first_broken_seq`,
`first_broken_id`, `first_broken_at`, `error_code` (`timed_out`,
`unreachable`, `unexpected_reply`), `outcome` (`{note, by, at}` or null) and
`version`. A checkpoint: `{id, at, purged_count, before, kind, incident_ref,
reason}`, `kind` one of `retention` or `re_anchor`.

- **Proof fields.** `first_broken_id` is the first broken entry's opaque id,
  set exactly when `first_broken_seq` is; a run that names one without the
  other fails closed. **Open entry** is an exact-id search for that id.
  `intact` needs proof or the app shows "Couldn't check", never Intact:
  `entries_checked` above 0, `to_at` set, `finished_at` set, and either
  `from_first: true` or a `from_checkpoint`. `intact` only when the whole
  range was walked; anything short of that is `error`.
- **Staleness, applied by the app.** A proven run is stale (never shown as
  Intact) when it finished more than 26 hours ago, or when it started more
  than 26 hours after the end of the range it covered. The backend should
  run the check daily (the design uses 06:00 UTC) so a healthy log is never
  stale.
- **Broken.** The app shows its banner for **Broken only** (on every tab of
  the audit view and on the home page), not for Error or stale. On Broken the
  server sends the bell (`audit_integrity`, §14.7) and an email to every
  auditor and platform admin, and sets `after_break` on every later entry
  (the app also marks them in the list, the detail and any CSV).
- **Background polls.** `GET .../verify/status` and `GET .../verify/runs/{id}`
  are polled and sent with `X-MHAI-Background: 1`. The BFF does not count them
  as activity for the 15-minute idle limit and does not forward the header, so
  the API must not let a poll refresh any server-side idle clock either.

**Exports**

| Endpoint | Behaviour |
|---|---|
| `POST /platform/audit/exports {kind, filters, reason, note?, reference?, mfa_code \| backup_code}` | `kind`: `events` or `named_access`. `reason`: `legal_request`, `hospital_request`, `regulator_request`, `internal_review`, `security_investigation`, `other` (`other` needs a note; note up to 200 characters, reference up to 60). → `{export_id, rows, capped, checksum, expires_at, file_name}`. **422 `too_many_rows` `{rows}`** over 50,000 rows: no file, no record of a file. The export entry is written before this returns |
| `GET /platform/audit/exports?cursor` | → `{exports, next_cursor}`: `id`, `kind`, `by`, `at`, `reason`, `note`, `reference`, `filters`, `filter_names` (`{hospital, person, clearance}`, names or null, so the filters can be put in words), `rows`, `capped`, `checksum`, `expires_at`, `downloaded_at` |
| `GET /platform/audit/exports/{id}/file` | the file, once, within 15 minutes (before `expires_at`) |

- The server builds the file, applies the patient rules once, and writes the
  record (who, when, reason, reference, filters, row count, checksum, export
  id) before the file is released; no record, no file. A `too_many_rows`
  reply carries the one extra field `rows`; nothing else extra.
- Up to 50,000 rows. A cap is never silent: `capped` is true and the file's
  header says so.
- `file_name` matches `^[A-Za-z0-9._-]{1,120}$`. The app names the download
  from it, so no `Content-Disposition` is needed. Suggested form:
  `audit-{events|named-access}-{export_id}-{YYYYMMDD-HHmm}Z.{csv|zip}`. The
  file's first lines carry who made it, the filters and the reason (key and
  value rows, a blank row, then the table). Every row carries the entry's
  position and fingerprint.
- **Owner only.** Anyone but the person who made the export gets **403
  `forbidden`**, recorded as `export_download_refused` with reason
  `not_owner`. The reply says nothing about whether the file is still there.
- **Once only.** After one download, or after `expires_at`, the answer is
  **410 `gone`**, recorded as `export_download_refused` with reason `gone`.
  The app asks upstream with `Accept: text/csv, application/zip` and passes
  the file through only as `text/csv` or `application/zip`, with
  `Cache-Control: no-store`.
- **Retry.** The app may retry a download that failed before the server
  answered (a network error) or with a 5xx. Any other answer spends the file.
  A body that breaks off after the server answered is not retried; the app
  says the file is spent.
- **Legal request, named access.** `kind: named_access` for one hospital and
  one or more letters, or one hospital and a date range, is a ZIP: the CSV
  of named-access entries (pseudonymous ids only) and a copy of each letter
  cited. Other exports are CSV. Named lists themselves can't be exported.

**Server duties** (the app can't keep these):

- No patient data in any entry, field or file; pseudonyms only as above.
- Platform actions show in a hospital's Activity as "Measured Health ·
  platform admin": the server substitutes that actor name for the hospital
  audience.
- Daily check at 06:00 UTC (a suggestion), each run its own entry; one run at
  a time.
