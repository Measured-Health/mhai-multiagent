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

The clinical app (`mhai-web`) is being built ahead of the API. Where an
endpoint doesn't exist yet, the frontend calls an assumed shape. This
document lists everything it needs, grouped by area, so the backend can
match it or say what changed.

It adds to the gateway design; it doesn't replace it. Where the two
disagree, §13 lists the conflict. Nothing here is decided until the
gateway spec or an ADR says so.

### Status labels

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
forwards exact, known shapes. Status: **Gateway** (15-minute JWT with
sliding refresh fits this). The backend needs nothing extra, except:

- CORS stays strict; the BFF calls the API server-to-server.
- Rate limits are per user, not per IP, because every request comes from
  the BFF's address. Login lockout keys on (login name, client IP), so the
  BFF must pass the client IP, and the API must trust that header only
  from the BFF.

### 2.2 Sign-in

| Item | Shape | Status |
|---|---|---|
| Login | `POST /auth/login {username, password, device_token?}` → `{access_token, expires_in, user}` or `{mfa_required: true, mfa_token}` | Gateway + change |
| Work email as login name | accept an email in `username` (or rename the field) | New |
| Login `user` | `id`, `role` (primary), **`roles`** (all), `email`, `display_name`, `full_name`, `role_title`, `specialty`, `hospital`, `mfa_enabled`, `must_change_password`, `terms_accepted_at`, `recovery_codes_left`, `idle_timeout_minutes`, `review_overdue_hours` | Assumed |
| Accept terms | `POST /auth/terms {version}` → 204; records version and time | Assumed |
| Lockout | the same `login_locked` (429) reply for an unknown login as for a locked one, so it can't reveal which accounts exist | Gateway + change |

Until `idle_timeout_minutes` and `review_overdue_hours` exist, the app uses
15 minutes and 4 hours.

### 2.3 Two-step verification (TOTP)

| Item | Shape | Status |
|---|---|---|
| Challenge | `POST /auth/mfa/challenge {mfa_token, code, trust_device?}` → token, plus `device_token` when `trust_device` was set and the hospital allows it | Gateway + change |
| Enrol, verify | `POST /auth/mfa/enroll`, `POST /auth/mfa/verify` | Gateway |
| Backup codes | 10 codes at enrolment (the gateway's recovery codes; the UI calls them backup codes); each works once | Gateway |
| Regenerate backup codes | `POST /auth/mfa/recovery-codes` → 10 new codes; the old ones stop working | Assumed |
| Per-hospital policy | `require_mfa_for_clinicians` (hospital setting); hospital admins, platform admins and auditors always need it | New |
| Trust this device | `device_token` bound to the user it was issued to, refused for anyone else; allowed only when the hospital setting `allow_trust_device` is on | Assumed |

Open question for the backend: whether a trusted device may skip the code
for a user who holds `hospital_admin`. The frontend docs don't say. We
suggest it may not.

### 2.4 Single sign-on (OIDC)

`GET /auth/oidc/login`, `/auth/oidc/callback`, `POST /auth/oidc/exchange`.
Status: **Gateway**. Change: `return_to` on the SSO start must be checked
against an allowlist, so it can never send the browser to another site.

### 2.5 Password

| Item | Shape | Status |
|---|---|---|
| Change | `POST /auth/password` | Gateway |
| Forgot | `POST /auth/password/forgot {email}` → 202 for any email | Assumed |
| Reset | `POST /auth/password/reset {token, new_password}`; link valid 30 minutes; checked against leaked passwords; signs out every device | Assumed |

The `users` table needs an `email` column for this.

### 2.6 Devices

| Item | Shape | Status |
|---|---|---|
| List | `GET /auth/sessions` → active devices | Assumed |
| Sign out one | `DELETE /auth/sessions/{id}` → 204 | Assumed |
| Sign out everywhere | `POST /auth/sessions/revoke-all` → 204 | Assumed |

The gateway denylists single token ids. Listing and revoking devices needs
a session (refresh family) record per device.

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
| Confidence breakdown | all four measures: evidence match, internal consistency, values and dosages, answer stability. The UI hides a bar until its value exists; it never invents one | Gateway + change (L1, L2 are `null` today) |
| ICD-10 | `icd10_code` nullable | Gateway |
| Consultation history | persistent, per clinician, searchable; stores clinical text, so needs retention and access rules. Session-only until then | New |
| Stage timings | per-stage durations; elapsed time until then | New |

## 8. Weak networks

Source: UI design §10 items 15–19.

| Item | Need | Status |
|---|---|---|
| Retry-safe writes | `Idempotency-Key` on `POST /queries`, `POST /feedback`, the correction actions, every hospital admin write, alert actions and clearances. A repeat with the same key returns the first response and does no new work | New (gateway: DLQ retry only) |
| Key lifetime | keep results for **at least 72 hours** (offline outbox) | New |
| `client_created_at` | optional ISO 8601 on `POST /queries` and `POST /feedback`, so the record shows when the clinician asked | New |
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

Source: analysis design §2–6, §9. Status: **Assumed** (Plan 6). Needs
clinical, privacy and legal sign-off before live use, because it changes
the rule that hospital admins see no clinical content.

| Endpoint | Behaviour |
|---|---|
| `GET /analysis/{view}?filters…` | aggregates; groups under 5 patients suppressed **on the server** ("Fewer than 5"), never sent raw. Views: prevalence and trends, who is affected, co-occurring conditions, findings and alerts, MHAI quality |
| `GET /analysis/patients?filters…&level=deidentified\|named` | de-identified: pseudonymous id (stable in the hospital, not the MRN), age band, sex, location type, findings. Named adds name and MRN, only with a verified, unexpired clearance for the caller. Every call audited; named calls flagged as security events |
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

**Plan 7d (notifications):** `GET /platform/notifications`, `POST
/platform/notifications/seen`, `GET`/`PUT /platform/me/analytics-email
{mode}` (`off`, `critical`, `critical_and_digest`; platform admins only). The
backend sends the emails: straight away for `serious` changes, at most one per
hospital per measure per day; a weekly digest on Monday 08:00 Lagos time;
plain words and a link, no patient details, no count under 5.
