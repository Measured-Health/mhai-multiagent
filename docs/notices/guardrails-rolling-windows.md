# Notice: rolling guardrail windows are retired

`GET /api/v1/platform/guardrails?period=7d|30d|90d` is deprecated (Plan 7c,
ruling R41a-3; ADR 0012).

## What changed

- **From the 7c-1 release**, the rolling values `7d`, `30d` and `90d` answer in
  the same shape as before, but **every count and share is hidden**, `all`
  included. Each response carries `Deprecation`, `Sunset` and a `Link` to this
  notice.
- **After the sunset** (90 days after the release, the date in the `Sunset`
  header), the three values are removed and answer `422 validation_failed`.

## Why

Rolling windows can be subtracted from each other, and from the next day's
windows, to work out a hidden count of 1 to 4. Platform statistics now show
only finished, nested calendar periods, whose figures never change.

## What to use instead

`GET /api/v1/platform/guardrails/periods` with `period=week`, `period=month`
or `period=quarter` (UTC). It is a new route with its own schema (ruling
R41a-4): `/platform/guardrails` keeps its schema and values until the sunset.

- a **week** is month-aligned: days 1–7, 8–14, 15–21 and 22 to the month's
  end (7 to 10 days);
- a **month** is a calendar month, and a **quarter** Jan–Mar, Apr–Jun, Jul–Sep
  or Oct–Dec.

The response is the latest finished period of that kind; `from` and `to`
bound it. Only periods that start after the release are published, so until
the first one of a kind is, the route answers `503 not_ready`.
