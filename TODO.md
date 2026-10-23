# TODO

Open work and decisions for mhai-multiagent. Remove an entry once it's done or decided.

## Under consideration

- **Go alongside Python.** We are considering using Go together with Python in this service. Nothing is decided yet, including which parts would move to Go. Any change needs an ADR in `docs/adr/` first.

## Backend contracts the frontend uses on sample data

mhai-web has built these screens against contracts that are marked **Assumed** or proposed. Each needs building here, or confirming as built:

- §10 Analysis (Plan 6), and the hospital side of clearance letters (Plan 11a).
- §14.10 platform clearances and the named-access switch (Plan 11b), including the `today` fields and revoke refusals (`same_person`, `same_side`).
- Named access in Analysis (Plan 11c): `POST /analysis/patients/named` and `clearance/me` with `reason`.
- §14.11 record connections (platform admin) and §12 EMR alert settings, routing and evidence (hospital side) (Plan 13).
- §14.8 platform analysis across hospitals (Plan 12, designed only).
