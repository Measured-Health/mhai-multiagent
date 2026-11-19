# Hand-off to mhai-web: documents PR 5 (uploads and letters live)

- **From:** mhai-multiagent, documents service PR 5 ([design](../designs/2026-11-14-documents-service.md) §10, §10.1; §17 departures 35-46)
- **Contract:** `services/core/openapi.yaml` 1.33.0 (additive). Regenerate the types from it; never hand-write them.
- **Switches:** everything here is off by default. `CORE_DOCUMENTS_ENABLED` turns the document routes on; `CORE_DOCUMENTS_LIVE_LETTERS` turns the live letter on and stays off until the legal lead signs the letter template (H3). Off, every route below answers 404 `not_found`, and the multipart letter upload stays the demo's.

Nothing below changes a route or a field mhai-web already uses. Build it behind your own flag, so it ships dark until the switches are on.

## 1. Clinical document upload (clinicians and hospital admins)

The file never goes through the BFF or core. Three calls and one direct post:

1. `POST /documents` with `{purpose: "clinical", content_type, size_bytes, patient_id?}`. `content_type` is one of `application/pdf`, `image/png`, `image/jpeg`, `image/tiff`; `size_bytes` is 1 to 25 MB. Send `patient_id` when the upload starts from a patient's page. Check the type and size in the browser first, so the user isn't sent a 422.
   - **201** `{document, upload: {url, fields: [{name, value}], expires_at}}`.
   - 422 `validation_failed`: the type or size isn't allowed. 404 `not_found`: the patient isn't one of the hospital's. 429 `rate_limited`: 30 uploads an hour per person. 503 `busy` (new code): the hospital's reading queue is full, so try later. 503 `unavailable`: the documents service didn't answer (the document is already cancelled).
2. **Post the file from the browser to `upload.url`** as `multipart/form-data`: every `upload.fields` entry, in the order given, then the file as the last field, named `file`. The policy expires at `expires_at` (10 minutes) and accepts only that type and size range. The object store answers 204; anything else means the post failed. The policy is a credential: never log it, store it or send it to analytics.
3. `POST /documents/{id}/uploaded` with no body. **202** with the document (`uploaded`). Calling it twice is harmless. 409 `conflict`: more than 30 minutes passed since step 1, so the document is `cancelled` and the user starts again.
4. Poll `GET /documents/{id}` (every few seconds, with back-off) until the status leaves `uploaded` / `scanning`:
   - `ready`: checked and stored. Review comes with PR 6.
   - `quarantined` with `reason`. Suggested words: `unsupported_type` "This isn't a PDF or image we can read." `too_large` "The file is over 25 MB." `too_many_pages` "The file has more than 50 pages." `too_many_pixels` "The pages are too large to read." `password_protected` "The file is password-protected; upload a copy without a password." `malware` "This file didn't pass our safety check." (show it as a security warning). `unreadable` "We couldn't read this file."
   - `failed` with `scan_unavailable`: "We couldn't check this file. Try again later."
   - `cancelled`.

Cancel: `POST /documents/{id}/cancel {version}`, with the `version` from the last read. It works until the document reaches review; afterwards it's 409 `conflict`.

Only the uploader can see their documents for now: another person's document is 404. Reviewers get their own routes in PR 6. Platform accounts are refused on all of these routes.

`DocumentOut`: `{id, purpose, status, reason, pages, received_at, scanned_at, version}`. There is no file name and no content. The status and reason enums in the spec include the later review statuses: render unknown values as a neutral "In progress", don't fail.

## 2. Live clearance letter (hospital admins, step-up), behind `CORE_DOCUMENTS_LIVE_LETTERS`

1. `POST /hospital/clearances/live` (JSON, `Idempotency-Key` required) takes the same fields as the multipart form (`signed_by`, `signer_title`, `letter_date`, `end_date`, `covered`, `covered_platform`, `renews`, `mfa_code` or `backup_code`), plus `content_type` (PDF, PNG or JPEG) and `size_bytes` (at most 10 MB). The refusals are the multipart route's (`end_date_too_late`, `not_eligible` with `ids`, `not_eligible_platform`, `rate_limited`, `mfa_invalid`), plus 503 `unavailable`, in which case no letter was made.
   - **201** `{clearance, upload}`: the letter is `checking` and `upload` is a POST policy, as in section 1.
2. Post the file as in section 1, then call `POST /hospital/clearances/{id}/uploaded` (no body, no code). **202** with the letter. 409 `conflict`: the file wasn't posted within 30 minutes, and the letter is now `refused` (`unreadable`).
3. The letter stays `checking` (already in your enum) until its check finishes, then becomes `waiting` with `pages`, or `refused` with `refused_reason` (`file_type`, `file_too_large`, `too_many_pages`, `password_protected`, `unreadable`, `failed_safety_check`; all already in your enum). Poll `GET /hospital/clearances/{id}`.

How to pick the route: when the hospital isn't the demo and the live switch is on, use `/live`. Keep the multipart route for the demo. A 404 from `/live` means the switch is off; keep today's "not available yet" message.

## 3. Letter pages

`GET /hospital/clearances/{id}/pages/{n}` is unchanged for demo letters (`image/png`). A live letter's page is now `image/jpeg`: at most 1,600 px wide and 900 KB, stamped across with the viewer's name and the time. Render it from the response's `Content-Type`, not by assuming PNG. 503 `unavailable` means the documents service didn't answer, and no view was recorded, so offer a retry. Keep the `no-store` handling: don't cache the image or put it in a service worker.

## 4. New error code

`busy` (503): add it to the error-code union and show "Too many documents are waiting to be read. Try again later." The `Problem.code` enum in the spec includes it.

## 5. Audit kinds (sent bare until you have them)

Core writes six new actions. The audit catalogue withholds them (they show as "can't be displayed") until mhai-web has kinds for them; then core maps them (TODO.md). Proposed mapping, all `type: "security"` or a new `documents` type, as you prefer:

| Action | Facts (ids and codes only) | Proposed kind |
|---|---|---|
| `document.upload_started` | `document_id`, `purpose`, `via` | `document_upload_started` |
| `document.uploaded` | `document_id`, `purpose`, `via` | `document_uploaded` |
| `document.cancelled` | `document_id`, `purpose`, `reason` (`user`, `unavailable`, `expired`) | `document_cancelled` |
| `document.ready` | `document_id`, `pages` | `document_ready` |
| `document.quarantined` | `document_id`, `reason` | `document_quarantined` (flag `malware` as a safety event) |
| `document.scan_failed` | `document_id`, `reason` (`scan_unavailable`) | `document_scan_failed` |

The letter's own entries (`clearance.uploaded`, `clearance.ready`, `clearance.refused`, `clearance.viewed`) are unchanged.

## 6. Not in this PR

Review screens and routes (PR 6), extraction (PR 7), the archive (PR 10). Review pages will use the same stamped JPEG as letters.
