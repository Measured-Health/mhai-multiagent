# EMR on live: what lets data flow (H3 and H6)

- **Status:** written 27 Nov 2026. EMR is on in live (production mode, `deploy/emr/enable-emr.sh`), but nothing is polled: connections can be created and tested only. Two items unblock the plumbing; the clinical, privacy and on-call gates (H4, H5, H7, H9 in TODO.md) still gate real patient data.
- **Owner of each step:** the product owner unless marked "us".

## H6: licences for the code tables

The emr service maps each result and medicine to a standard code before rules and alerts use it. Today the tables are synthetic (`services/emr/codes/testdata/`); the loader reads `services/emr/codes/{loinc,atc}.csv` (plan Task 21).

### LOINC (lab tests and vital signs)

Free, but a person must accept the licence for Measured Health.

1. Create a free account at loinc.org (an organisation email).
2. Read and accept the LOINC licence (its terms of use) when downloading.
3. Download the current LOINC release (the "LOINC Table" in CSV).
4. Keep the licence acceptance (a screenshot or the confirmation email) with our legal records.
5. Tell us the release version. **Us:** load it into `services/emr/codes/loinc.csv` by the loader's format, add the attribution notice the licence requires, run the map checks, deploy.

### WHO ATC/DDD (medicines)

The ATC/DDD index belongs to the WHO Collaborating Centre for Drug Statistics Methodology (Oslo). Using its electronic file inside a commercial product needs their permission, usually for a fee.

1. Go to the Centre's website (whocc.no) and find "ATC/DDD Index" and the order or request page for the electronic file.
2. Request the file for use in software, naming the product (MHAI clinical decision support) and the use (mapping hospital medication records to ATC codes; no redistribution of the index).
3. Sign their agreement and pay any fee; keep the agreement with our legal records.
4. Send us the file. **Us:** load it into `services/emr/codes/atc.csv`, run the map checks, deploy.

Until both are in, alerts can't rely on real codes, and any real data stays blocked.

## H3: the secret store

Hospitals' connection credentials (FHIR keys, SFTP keys) must live in a store separate from the server's disk, readable only by the emr service. The service already talks to a store interface (`services/emr/internal/secrets/store.go`: put, get, delete by path `mhai/<env>/emr/<hospital>/<connection>/<field>`); today only the file store exists, which production refuses.

The owner ruled out AWS's store (27 Nov). Options:

| Option | What it is | For | Against |
|---|---|---|---|
| **OpenBao (recommended)** | Open-source secrets server (the community fork of HashiCorp Vault, MPL-2.0), run by us | No cloud tie-in; can run in Nigeria later (helps H5's data location); per-path access rules; full audit log | We run it: someone holds the unseal keys and unseals it after a restart |
| HashiCorp Vault | The original | Mature | Its licence (BSL) limits some commercial use; OpenBao avoids that |
| GCP Secret Manager or Azure Key Vault | Another cloud's managed store | Fully managed | Cross-cloud: the server would hold a key file to reach it, weaker than an attached identity; data outside Nigeria |
| Encrypted files on the server (sops and age) | Encrypted files | Simplest | Not a separate store: whoever takes the server takes the secrets; the design rules it out for production |

### If OpenBao is chosen

1. **Where:** best on its own small machine (in Nigeria if hosting moves there); acceptable to start in its own locked-down container on the live host, on its own network, reachable only by the emr service.
2. **Unseal keys:** OpenBao starts sealed. The owner keeps the unseal key shares offline (with the production CA in the password manager); after any restart someone unseals it. A later step can automate this with a hardware key or a cloud KMS.
3. **Access:** one policy, the emr service's identity may read, write and delete under `mhai/production/emr/*` only; core has none. TLS from our private CA. Audit log on.
4. **Backups:** its storage is encrypted at rest; back it up with the database backups.
5. **Us:** build the `openbao` backend behind the store interface (plan Task 12), with tests against a local OpenBao container; a deploy script like `enable-emr.sh` to start and configure it; switch the emr service from `staging` to `production`.

**Decision needed:** confirm OpenBao (or pick another option), and where it runs to start.
