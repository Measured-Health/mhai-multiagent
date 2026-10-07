# Security policy

MHAI processes patient records and produces clinical decision support.
Please report vulnerabilities privately; don't open a public issue.

## Reporting

Use **GitHub private vulnerability reporting**: the repository's
*Security* tab, then *Report a vulnerability*. Include:

- what's affected (file, endpoint, configuration) and the impact;
- steps to reproduce, using synthetic data only;
- any suggested fix.

**Never include real patient data, credentials or tokens in a report.** If
you found exposed PHI or secrets, say where they are; don't copy them.

We aim to acknowledge reports within 3 working days and to agree a fix
timeline after triage.

## Scope

In scope:

- cross-patient data exposure;
- authentication and authorisation bypass;
- guardrail bypasses that let unverified clinical content through as
  verified;
- PHI in logs or storage;
- secret exposure;
- supply-chain issues in this repository.

The rules the code must follow are in [SKILLS.md](SKILLS.md).

## Supported versions

Only the latest release and `main` receive security fixes.
