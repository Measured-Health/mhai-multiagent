# TODO

Open work and decisions for mhai-multiagent. Remove an entry once it's done or decided.

## Under consideration

- **Fast intelligence service (parked 27 Oct, product owner).** Serve the models on a rented cloud GPU, aiming for millisecond steps and a checked answer in about 3 s instead of 8–30 s. The approach: vLLM, compiled small models, streaming, FP8 and speculative decoding. The draft design, with the questions to answer first, is in `docs/specs/2026-10-27-intelligence-performance-design.md`. It needs the Python-over-NATS boundary in place, plus a GPU budget (about $1,400 a month for one L40S) and clinical sign-off for FP8 and streaming.

- **Go alongside Python.** We are considering using Go together with Python in this service. Nothing is decided yet, including which parts would move to Go. Any change needs an ADR in `docs/adr/` first.

## Backend contracts the frontend uses on sample data

mhai-web has built these screens against contracts that are marked **Assumed** or proposed. Each needs building here, or confirming as built:

- §10 Analysis (Plan 6), and the hospital side of clearance letters (Plan 11a).
- §14.10 platform clearances and the named-access switch (Plan 11b), including the `today` fields and revoke refusals (`same_person`, `same_side`).
- Named access in Analysis (Plan 11c): `POST /analysis/patients/named` and `clearance/me` with `reason`.
- §14.11 record connections (platform admin) and §12 EMR alert settings, routing and evidence (hospital side) (Plan 13).
- §14.8 platform analysis across hospitals (Plan 12, designed only).
