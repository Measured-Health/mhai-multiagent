# 0019. Read HL7 v2 as files over SFTP, and take MLLP push through an on-site bridge

- **Status:** Proposed
- **Date:** 2026-11-27
- **Deciders:** product owner (the coordinator recorded the first version's call under the owner's "decide and proceed" rule; the bridge is the owner's scope of 27 Nov)
- **Extends:** [ADR 0014](0014-separate-emr-service-and-document-route.md) (the emr service holds outside credentials and the only route out to hospital systems)
- **Design:** [`docs/designs/2026-11-27-hl7v2-file-connector-and-bridge.md`](../designs/2026-11-27-hl7v2-file-connector-and-bridge.md)

## Context

Most hospital lab and pharmacy systems speak HL7 v2. Interface engines can send it over MLLP (TCP) or write it to files. Every EMR connector so far is outbound only: the emr service pulls through egress-emr, and nothing listens for hospital traffic on our hosts. An MLLP listener on the live host would be the first inbound port for hospital traffic, with its own authentication, DoS and network questions.

## Decision

1. **`hl7v2_file`:** HL7 v2 messages (ORU^R01, RDE^O11) are read as batch files pulled over SFTP, with the `file_export` connector's transport unchanged (pinned host key, key-only sign-in through egress-emr, `processed/` and `rejected/`, nothing deleted, staleness by declared interval). They map to the same raw records as `fhir_r4`, so normalisation, rules and alerts are unchanged.
2. **Push through an on-site bridge:** a hospital whose engine can only push MLLP runs our `hl7-bridge` next to it. It acknowledges each message only after it is on disk, spools the messages into `hl7v2_file` files, and serves them over its own SFTP server. Our side stays outbound only.
3. **The file format is defined once,** in `services/emr/internal/hl7v2`, used by the connector and the bridge.
4. **MLLP listener on our side: deferred, needs owner decision.**

## Consequences

- HL7 v2 hospitals connect without custom CSV mapping, and without an inbound port on our hosts.
- The bridge puts PHI on a host at the hospital: full-disk encryption there is required and is the hospital's to set up; retention and type filtering keep what is stored small.
- The bridge's SFTP port must be reachable from our egress address (port 22, a DNS name) or over the VPN. That is an inbound port at the hospital, under its firewall.
- One more deliverable to version and support (an image the hospital runs).

## Alternatives considered

- **MLLP listener on our host:** the most direct; deferred (a new inbound attack surface and a security decision of its own).
- **Ask every hospital for `mhai_csv_v1`:** custom mapping work at each hospital.
- **The bridge pushing to us over HTTPS:** an inbound endpoint again.
