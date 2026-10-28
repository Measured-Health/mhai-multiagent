# 0012. Version the API by major number in the path and subject, and change a version only by adding

- **Status:** Accepted 2026-10-28 by the product owner
- **Date:** 2026-10-28
- **Deciders:** product owner

## Context

The API is already `v1` everywhere: the HTTP path `/api/v1`, the NATS subjects `api.v1.*`, `inference.v1.*` and `jobs.v1.*`, and the protobuf package `mhai.intelligence.v1`. What was missing is the rule for when something changes, and for how a version ends. Several clients depend on core (the clinical and platform BFFs, and the Python workers), and they deploy on their own schedules.

## Decision

1. **The major version is in the address.** The HTTP path (`/api/v1`) and the NATS subject (`api.v1.<audience>`, `inference.v1.>`, `jobs.v1.>`) carry it, and so does the protobuf package. A new major version is served alongside the old one, never in place of it.
2. **Inside a major version, changes only add.** Allowed: a new route, a new optional request field, a new response field, a new error code, a new optional header. Clients must ignore response fields they do not know, and must treat an error code they do not know as the generic error for its HTTP status. Adding a value to `Problem.code` is non-breaking; the breaking-change check allows exactly that.
3. **Breaking means** any of: removing or renaming a route or field; changing a field's type, format or limits so a valid value becomes invalid; making an optional request field required; changing a route's authentication, role or step-up requirements so a call that worked is refused; changing what an error code or a status means. A breaking change is made in `v2`, never in `v1`.
4. **Retiring a version.** A route or version that will go away answers with `Deprecation: @<unix seconds>` (RFC 9745) and `Sunset: <HTTP-date>` (RFC 8594) on every response, plus `Link: <notice>; rel="deprecation"`. The notice period between the first `Deprecation` header and the `Sunset` date is **at least 90 days** while we are at the demo stage. This number is the product owner's to change; change it here and nowhere else. After the sunset date the route may be removed. `httpx.Deprecated` sets the headers.
5. **Protobuf follows the same rule.** The wire contract in `proto/` changes only by adding (new fields with new numbers, new messages); `buf breaking` against `main` enforces it in CI.
6. **`info.version` in `services/core/openapi.yaml` is semver,** kept in step with these rules: a **minor** bump for an additive change, a **patch** bump for a documentation-only change. The **major** number equals the path's major; a major bump comes with a new path and a new contract file for it, not an edit of this one.
7. **CI enforces the HTTP rule.** `oasdiff breaking` (pinned version) compares `openapi.yaml` with the one on the target branch and fails the pull request on a breaking change. The same pull request that adds `v2` must keep `v1`'s file unchanged.

## Consequences

- A BFF can upgrade on its own schedule: old calls keep working until a published sunset date.
- Some mistakes in `v1` (a badly named field) stay until `v2`. That is the cost.
- Two versions served at once means two sets of routes and tests while a sunset runs.
- oasdiff finds structural breaks only. A change in behaviour with the same shape (rule 3, "meaning") still needs a human reviewer to call it.
