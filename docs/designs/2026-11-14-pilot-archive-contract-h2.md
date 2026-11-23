# Pilot hospital archive contract (H2): what the archive provides for MHAI

- **Status:** Draft for the pilot hospital's team and ours. The product owner delegated every decision on 14 Nov 2026; §11 lists the calls made, for later review. Nothing here is built.
- **Date:** 2026-11-14
- **For:** the team that runs the pilot hospital's document archive (DocFlow). It says what to build; it doesn't say how MHAI works inside, beyond what you need.
- **Context for MHAI readers:** EMR plan PR 12 (Task 46, the `document_archive` connector), H2 in the plan's people items, EMR design §5.3, and the [documents service design](2026-11-14-documents-service.md) §4.3 and §6.5.

## 1. What this is for

MHAI wants to read the scanned lab reports in the pilot hospital's archive so clinicians can review the values and see abnormal results. MHAI only **reads**: it never changes, moves or deletes anything in the archive.

To do that safely, MHAI needs four things the archive doesn't offer today:

1. a list of **files added since** a point MHAI last read, with a stable cursor;
2. a list of **files removed since** a point, because deletions leave no trace today;
3. a **key per integration**, read-only, limited to these endpoints, that the pilot hospital can rotate and revoke without touching anything else;
4. predictable **rate limits and errors**.

The existing deployment-wide service key and every existing endpoint stay as they are. MHAI will not use them.

## 2. Endpoints

All under the archive's existing API base, over HTTPS only.

| Method and path | What |
|---|---|
| `GET /api/v1/integration/files/added` | files added or changed since a cursor (§4) |
| `GET /api/v1/integration/files/removed` | files removed since a cursor (§5) |
| `GET /api/v1/integration/files/{file_id}/content` | one file's bytes (§6) |

Nothing else accepts an integration key. An integration key sent to any other endpoint (search, boxes, exports, batch download, admin, users) gets 403 `forbidden`.

## 3. Integration keys

### 3.1 What a key is

- **One key per integration** (MHAI is one integration; a second consumer gets its own). Each integration has a name (`mhai`), an id, and up to two active keys at once (for rotation).
- **Read-only, scoped** to the three endpoints above and, optionally, to a list of top-level boxes the pilot hospital chooses. A file outside the scope is treated as if it didn't exist (it never appears in a listing; its content is 404).
- **Format:** at least 256 random bits, URL-safe, with a recognisable prefix such as `dfi_`, so a leaked key can be found by secret scanners.
- **Sent as** `Authorization: Bearer <key>`. Never in a URL or query string.
- **Stored by the pilot hospital as a hash** (SHA-256 is enough for a random key), never in plain text. The key is shown once, at creation.
- **Recorded as its own actor** in the archive's audit log (`integration:mhai`, with the key's id, never the key), separate from users and the deployment-wide key. Each call logs the key id, endpoint, `file_id` where there is one, status and time.
- **Optionally bound to MHAI's outgoing IP addresses.** MHAI calls from a fixed set of addresses, given at set-up. We recommend the pilot hospital refuses the key from anywhere else.

### 3.2 Rotation

- A key lives at most **365 days**. The pilot hospital's admin screen (or a command) creates a second key for the same integration; both work until the old one is revoked.
- MHAI's side: the platform admin enters the new key, MHAI checks it with one listing call, then tells the pilot hospital to revoke the old one. Target: done within 7 days of creating the new key.
- Each key shows its creation time, expiry, and last use (time and source address), so the pilot hospital can see when the old one stops being used.

### 3.3 Revocation

- Revoking a key takes effect within **60 seconds** on every archive server.
- After revocation the key gets 401 `unauthorized`, like an unknown key.
- The pilot hospital may revoke at any time without asking us (for example on suspicion of a leak). MHAI then stops reading and raises an alert on our side; nothing is lost, because the cursor stays valid (§4.3).
- **Contacts:** each side names a security contact at set-up. Either side tells the other within 24 hours of suspecting a key leak.

## 4. Files added since a cursor

### 4.1 Request

`GET /api/v1/integration/files/added?cursor=<cursor>&limit=<n>`

or, to get a starting cursor, `GET /api/v1/integration/files/added?start=latest` or `?start=<RFC 3339 UTC time>`.

| Parameter | Rules |
|---|---|
| `cursor` | an opaque string from a previous reply, at most 256 characters. Exactly one of `cursor` or `start` |
| `start` | `latest`: no items, just the cursor for "from now on". A time such as `2026-11-01T00:00:00.000Z`: begins with the first entry added at or after that time (for a limited catch-up) |
| `limit` | 1 to 500; default 100 |

### 4.2 Response

`200 OK`, `Content-Type: application/json`, `Cache-Control: no-store`:

```json
{
  "items": [ <entry>, ... ],
  "next_cursor": "<opaque>",
  "has_more": true
}
```

Each entry:

| Field | Type | Rules |
|---|---|---|
| `file_id` | string, ≤ 128 | the archive's own id for the file: opaque, stable for the file's life, **not derived from its name**, never reused |
| `revision` | string, ≤ 64 | changes whenever the file's bytes change; opaque |
| `change` | `"new"` or `"changed"` | `changed` when an existing `file_id` gets new bytes |
| `name` | string, ≤ 255 | the file's name as the archive shows it (it usually names the patient) |
| `mime_type` | string | the archive's type for the file, e.g. `application/pdf` |
| `size_bytes` | integer | the exact size of this revision |
| `sha256` | string, 64 lower-case hex | the SHA-256 of this revision's bytes |
| `added_at` | string | when **the archive** first saw this revision, RFC 3339 UTC with milliseconds and `Z` |

Every file the archive indexes appears, whatever its type; MHAI skips types it doesn't read. A rename alone, or a move between boxes within scope, produces no entry.

### 4.3 Cursor rules

These are the rules that let MHAI never miss or double-read a file:

1. **Order is the archive's own sequence,** assigned when it records an entry, not the file's modification time. A file uploaded today with an old modification date must still appear after the cursor MHAI holds. (Today's search by modification time can miss such files; that's why this is needed.)
2. **No gaps behind a cursor.** Once a reply returns a cursor, no entry may later appear before it. If entries can be recorded out of order (for example by parallel indexers), only return entries older than a short settle time, such as 60 seconds, so every earlier entry is visible first.
3. **Stable:** asking again with the same cursor returns the same entries (except ones beyond the settle time that have since appeared at the end).
4. **`next_cursor` is always present.** With no new entries it equals the cursor sent (or the latest one), and `has_more` is `false`.
5. **Valid for at least 90 days.** After that the archive may answer 410 `cursor_expired`. MHAI then stops and alerts a person; it never restarts from the beginning by itself.
6. **Same entry twice is fine** (for example after a retry): MHAI ignores a `file_id` and `revision` it has already seen.

## 5. Files removed since a cursor

`GET /api/v1/integration/files/removed?cursor=<cursor>&limit=<n>` (or `start=latest` / a time, as in §4.1), with the same reply shape and the same cursor rules (§4.3), on its own cursor.

Each entry:

| Field | Type | Rules |
|---|---|---|
| `file_id` | string | as in §4.2 |
| `revision` | string | the last revision the archive held |
| `removed_at` | string | when the archive recorded the removal, RFC 3339 UTC |
| `reason` | `"deleted"` or `"out_of_scope"` | `deleted`: removed from the archive. `out_of_scope`: still in the archive, but moved out of the boxes this key may read |

No name in a removal entry: MHAI doesn't need it.

The archive must record a removal **before** it drops a file from its index, and keep removal entries at least 90 days. A file that was never listed (for example, added and deleted within the settle time) may be omitted.

## 6. Downloading a file

`GET /api/v1/integration/files/{file_id}/content?revision=<revision>`

- `200 OK` with the bytes, streamed. Headers: `Content-Type` (the file's type), `Content-Length`, `X-Content-SHA256` (hex), `Cache-Control: no-store`. No `Content-Disposition` file name: MHAI already has the name from the listing.
- **No redirects.** MHAI doesn't follow them. If the file lives in another store, the archive fetches it and streams it.
- `404 not_found` when the file was removed, is out of scope, or never existed: the same reply for all three.
- `409 revision_changed` when the file now has different bytes. MHAI will see the new revision in the next listing.

## 7. Rate limits

Per key:

| Endpoint | Limit |
|---|---|
| `added`, `removed` | 30 requests a minute |
| `content` | 10 requests a minute, at most 2 at once |

Over the limit: `429 rate_limited` with a `Retry-After` header in seconds. MHAI waits at least that long and backs off further on repeats.

MHAI keeps well under these on its own: it polls each listing about once every 15 minutes, and downloads at most 100 files a day for review (it may agree a higher figure later). If the archive needs MHAI to slow down during its own working hours or its nightly indexing, say so and we will add a quiet window.

## 8. Errors

Every error has this body, `Content-Type: application/json`:

```json
{ "error": { "code": "rate_limited", "message": "Too many requests.", "request_id": "01JD7Q6Q3X9W4S0C7M2V8B1N5T" } }
```

- `code` is one of the closed set below. `message` is a fixed sentence per code. **Neither ever contains a file name, a patient's name, a path or a stack trace.**
- `request_id` is the archive's own id for the request, so we can quote it to each other when something goes wrong.

| Status | `code` | When |
|---|---|---|
| 400 | `invalid_request` | a missing, extra or malformed parameter (both `cursor` and `start`, a `limit` of 0) |
| 400 | `invalid_cursor` | a cursor the archive didn't issue |
| 401 | `unauthorized` | no key, an unknown key, a revoked or expired key |
| 403 | `forbidden` | a valid key on an endpoint it may not use, or from an address it's not bound to |
| 404 | `not_found` | §6 |
| 409 | `revision_changed` | §6 |
| 410 | `cursor_expired` | §4.3 rule 5 |
| 429 | `rate_limited` | §7 |
| 500 | `internal` | anything unexpected on the archive's side |
| 503 | `unavailable` | maintenance, indexing in progress, or the file store unreachable; send `Retry-After` if known |

MHAI records only the status, the code and the `request_id`. It never stores an error's body beyond that.

## 9. Sample payloads

All names below are invented.

**Getting a starting cursor:**

```http
GET /api/v1/integration/files/added?start=latest HTTP/1.1
Host: archive.example-hospital.org
Authorization: Bearer dfi_EXAMPLEONLY0000000000000000000000000000000
```

```json
{ "items": [], "next_cursor": "c2VxPTQ4MjE3", "has_more": false }
```

**Reading what's new:**

```http
GET /api/v1/integration/files/added?cursor=c2VxPTQ4MjE3&limit=2 HTTP/1.1
```

```json
{
  "items": [
    {
      "file_id": "1QxEXAMPLEfile0001",
      "revision": "r1",
      "change": "new",
      "name": "SAMPLE ADAEZE TESTPATIENT.pdf",
      "mime_type": "application/pdf",
      "size_bytes": 1843321,
      "sha256": "3a7bd3e2360a3d29eea436fcfb7e44c735d117c42d1c1835420b6b9942dd4f1b",
      "added_at": "2026-11-20T09:14:03.512Z"
    },
    {
      "file_id": "1QxEXAMPLEfile0002",
      "revision": "r1",
      "change": "new",
      "name": "SAMPLE TUNDE TESTPATIENT.docx",
      "mime_type": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
      "size_bytes": 40211,
      "sha256": "b5bb9d8014a0f9b1d61e21e796d78dccdf1352f23cd32812f4850b878ae4944c",
      "added_at": "2026-11-20T09:14:04.020Z"
    }
  ],
  "next_cursor": "c2VxPTQ4MjE5",
  "has_more": true
}
```

**Reading removals:**

```json
{
  "items": [
    { "file_id": "1QxEXAMPLEfile0001", "revision": "r1", "removed_at": "2026-11-21T16:02:44.118Z", "reason": "deleted" }
  ],
  "next_cursor": "cj0xMjA0",
  "has_more": false
}
```

**Downloading:**

```http
GET /api/v1/integration/files/1QxEXAMPLEfile0001/content?revision=r1 HTTP/1.1
```

```http
HTTP/1.1 200 OK
Content-Type: application/pdf
Content-Length: 1843321
X-Content-SHA256: 3a7bd3e2360a3d29eea436fcfb7e44c735d117c42d1c1835420b6b9942dd4f1b
Cache-Control: no-store
```

## 10. What MHAI does with each field

| Field | What MHAI does | What MHAI never does |
|---|---|---|
| `file_id` | refers to the file by it everywhere: requests, its own records, logs, error reports | derive anything from it |
| `revision` | downloads that exact revision; treats a new revision as a new document for review | |
| `change` | counts new and changed files | |
| `name` | stores it with the document in the pilot hospital's own partition of MHAI's database (encrypted at rest), readable only on the review screen, as a hint to the person matching the document to a patient; clears it 30 days after the review is finished | log it, put it in a message, metric, alert or error report, or match a patient from it automatically |
| `mime_type` | downloads only `application/pdf`, `image/jpeg`, `image/png` and `image/tiff`; counts the rest by type | download any other type |
| `size_bytes` | skips files over 25 MB (counted, never downloaded); checks the download's length | |
| `sha256` | checks the downloaded bytes; refuses and reports a mismatch (by `file_id` and `request_id`) | |
| `added_at` | ordering and freshness reporting only | use it as the document's clinical date (that is read from the document and confirmed by a person) |
| `removed_at`, `reason` | for a file not yet reviewed: cancels it and deletes MHAI's copy within 7 days. For one already reviewed: flags it to the hospital's admin, who decides whether its values stay in the record | change anything in the archive |
| `next_cursor` | stores it after each page is processed, so a restart neither misses nor repeats | start from the beginning by itself |
| error `code`, `request_id` | logs the status, code and `request_id`; backs off on 429 and 503; stops and alerts on 401, 403 and 410 | log the message or body |

**Also, on MHAI's side:**
- every downloaded file is checked for type and malware, stored encrypted under a key for the pilot hospital only, and seen only by the pilot hospital's own staff who review documents;
- values reach the patient's record only after a person has checked them and matched the patient;
- the key is held in MHAI's secret store, used only by the one service that talks to the archive, and never logged;
- MHAI calls only from its fixed addresses, over HTTPS, and checks the archive's certificate.

## 11. Decisions for later review

Made on 14 Nov 2026 under the product owner's delegation, on the safe and private side:

1. **Separate added and removed listings, each with its own cursor,** rather than one change feed: simpler to build on the archive's side, and a removal never waits behind additions.
2. **The cursor follows the archive's own sequence, not file times** (§4.3), so back-dated files are never missed.
3. **The name is included** in the added listing (it's the reviewer's only hint, since the archive has no patient id), and kept out of removals, downloads and errors.
4. **No box name, folder path, uploader or modification time** in any reply: MHAI doesn't need them.
5. **MHAI starts at `latest`.** No backfill of the existing archive; a limited catch-up by `start=<time>` only when the hospital asks and reviewers are ready for it.
6. **Keys: Bearer header, a recognisable prefix, hashed at rest, 365-day life, two at once for rotation, revocation within 60 seconds,** optional IP binding (recommended).
7. **Cursors and removal entries last at least 90 days;** an expired cursor stops MHAI rather than restarting it.
8. **No redirects on download,** and the SHA-256 in the listing is checked.
9. **A reviewed file removed from the archive is not deleted from the record by itself:** the hospital's admin decides.

## 12. Acceptance checks

The pilot hospital's team can run these before we connect; we run them again together on a test copy with invented files.

1. `start=latest` returns no items and a cursor; a file added afterwards appears after that cursor.
2. A file whose modification time is a year ago, added today, appears after today's cursor.
3. Paging through with `limit=1` returns every entry once, in order, with `has_more` false at the end.
4. Replacing a file's bytes produces a `changed` entry with a new `revision` and `sha256`; renaming it produces nothing.
5. Deleting a file produces a `removed` entry before the file disappears from the index; its content is then 404.
6. Moving a file out of the key's boxes produces `out_of_scope`; its content is 404, the same reply as a file that never existed.
7. The downloaded bytes' SHA-256 equals the listing's and `X-Content-SHA256`; no redirect is sent.
8. The integration key gets 403 on search, boxes, exports, batch download and admin; the deployment-wide key gets 401 on the integration endpoints (they are separate).
9. A revoked key gets 401 within 60 seconds on every server; a second active key keeps working.
10. The 11th download in a minute gets 429 with `Retry-After`.
11. No error body, log line or audit entry for an integration call contains a file name.
12. An expired or unknown cursor gives 410 `cursor_expired` or 400 `invalid_cursor` respectively.

## 13. Notes for the archive's implementers (not binding)

- A simple way to meet §4.3: give each indexing event (new file, new revision) an increasing number from a counter in the archive's database, store it with the entry, and make the cursor an encoding of that number. Do the same for removals in a separate log.
- The indexer already prunes files that vanished from the file store; writing the removal entry just before the prune meets §5.
- The archive already computes SHA-256 in places; computing it once per new revision at index time meets §4.2 without hashing at download time.
- The existing per-endpoint rate limiter can key on the integration key's id for these endpoints.
