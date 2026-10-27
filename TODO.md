# TODO

Open work and decisions for mhai-multiagent. Remove an entry once it's done or decided.

## Decided

- **Go alongside Python.** A Go gateway owns the API, auth, tenancy, audit and EMR plumbing. The Python services keep the models and are reached only over NATS. See [ADR 0010](docs/adr/0010-api-in-go-models-in-python-over-nats.md).

## Open items for the Go gateway

From §11 of the [design](docs/specs/2026-10-26-go-gateway-nats-and-auth-design.md):

- **Mail provider** for invites and password resets. Needed in step 2 (authentication). AWS SES is the suggestion.
- **Scale assumption.** Sizing assumes up to about 50 hospitals and 5,000 staff accounts in the first year. Correct it if talks suggest more.
- **Data Protection Act check** before go-live: data processing agreements with each hospital, and where data is stored. Needs someone qualified.

## Under consideration

- **Fast intelligence service (parked 27 Oct, product owner).** Serve the models on a rented cloud GPU, aiming for millisecond steps and a checked answer in about 3 s instead of 8–30 s. The approach: vLLM, compiled small models, streaming, FP8 and speculative decoding. The draft design, with the questions to answer first, is in `docs/specs/2026-10-27-intelligence-performance-design.md`. It needs the Python-over-NATS boundary in place, plus a GPU budget (about $1,400 a month for one L40S) and clinical sign-off for FP8 and streaming.

## Backend contracts the frontend uses on sample data

mhai-web has built these screens against contracts that are marked **Assumed** or proposed. Each needs building here, or confirming as built:

- §10 Analysis (Plan 6), and the hospital side of clearance letters (Plan 11a).
- §14.10 platform clearances and the named-access switch (Plan 11b), including the `today` fields and revoke refusals (`same_person`, `same_side`).
- Named access in Analysis (Plan 11c): `POST /analysis/patients/named` and `clearance/me` with `reason`.
- §14.11 record connections (platform admin) and §12 EMR alert settings, routing and evidence (hospital side) (Plan 13).
- §14.8 platform analysis across hospitals (Plan 12, designed only).
