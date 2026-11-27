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

**Chosen: OpenBao on the live host (owner, 27 Nov).** It is built (plan Task 12, this change) and rehearsed on the build server, and not yet run on live.

- **The service:** OpenBao runs in its own locked-down container on the live host, on an internal network shared with the emr service only, with no published port. It uses TLS from our production CA, and emr's client certificate is required.
- **Access:** one policy. The emr service may read, write and delete under `mhai/production/emr/*` only. Core has no access. The audit log is on.
- **With it,** the emr service runs as production.

**Steps on live, in order (deploy/README.md, "OpenBao (the EMR secret store)"):**

1. **Us:**
   - `bash openbao/enable-openbao.sh --check`;
   - bring the production CA back briefly, then `--prepare --ca-dir ~/mhai-ca`;
   - move the CA off again;
   - `bash openbao/enable-openbao.sh`. That is about 15 s, and the EMR service is away about 6 s; nothing else restarts.
2. **Owner: the unseal key shares.** The setup writes 5 shares (any 3 unseal) to `~/mhai-openbao-init/` on the host, and nowhere else.
   - Move them off at once.
   - Spread them so that no one person and no one place holds 3, never in chat or email.
   - Then delete the folder there (`shred`).
   - With fewer than 3 shares left, every stored credential is lost and must be entered again.
3. **Owner: who unseals.** OpenBao starts sealed after any restart. Until it is unsealed (`bash openbao/enable-openbao.sh --unseal`, 3 shares), EMR connections fail closed; the rest of the site is unaffected.
4. **Backups:** the nightly backup adds an encrypted OpenBao snapshot; restoring it needs 3 shares.

**Later:**

- OpenBao on its own small machine (in Nigeria if hosting moves there, which helps H5);
- auto-unseal from a second OpenBao (transit) or an HSM, to remove the manual unseal.

The options considered on 27 Nov:

| Option | What it is | For | Against |
|---|---|---|---|
| **OpenBao (chosen)** | Open-source secrets server (the community fork of HashiCorp Vault, MPL-2.0), run by us | No cloud tie-in; can run in Nigeria later; per-path access rules; full audit log | We run it: someone holds the unseal keys and unseals it after a restart |
| HashiCorp Vault | The original | Mature | Its licence (BSL) limits some commercial use |
| GCP Secret Manager or Azure Key Vault | Another cloud's managed store | Fully managed | Cross-cloud key file on the server; data outside Nigeria |
| Encrypted files on the server (sops and age) | Encrypted files | Simplest | Not a separate store; the design rules it out for production |
