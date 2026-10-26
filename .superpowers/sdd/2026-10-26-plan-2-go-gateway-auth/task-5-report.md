# Task 5 report

## Red (before implementation; build failures)
```
vet: internal/account/account_test.go:9:9: undefined: NormaliseEmail
vet: internal/password/password_test.go:12:12: undefined: Hash
vet: cmd/gateway/seed_test.go:12:9: undefined: seed
internal/account/account_test.go:9:9: undefined: NormaliseEmail
internal/account/account_test.go:16:12: undefined: ValidateRoles
internal/account/account_test.go:16:78: undefined: ErrRoleHospital
internal/account/account_test.go:19:12: undefined: ValidateRoles
internal/account/account_test.go:19:70: undefined: ErrRoleHospital
internal/account/account_test.go:22:12: undefined: ValidateRoles
internal/account/account_test.go:25:12: undefined: ValidateRoles
internal/account/account_test.go:28:12: undefined: ValidateRoles
internal/account/account_test.go:35:13: undefined: ValidateRoles
internal/account/account_test.go:39:12: undefined: ValidateRoles
FAIL	github.com/Measured-Health/mhai-multiagent/gateway/internal/password [build failed]
FAIL	github.com/Measured-Health/mhai-multiagent/gateway/internal/account [build failed]
internal/password/password_test.go:12:12: undefined: Hash
internal/password/password_test.go:19:13: undefined: Verify
internal/password/password_test.go:23:11: undefined: Hash
internal/password/password_test.go:30:12: undefined: Hash
internal/password/password_test.go:34:13: undefined: Verify
```

## Green (gofmt, vet, -race, -race -tags integration)
```
ok  	github.com/Measured-Health/mhai-multiagent/gateway/cmd/gateway	1.010s
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/account	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/audit	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/config	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/httpx	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/logx	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/password	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/cmd/gateway	5.139s
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/account	8.139s
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/audit	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/bus	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/config	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/db	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/httpx	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/logx	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/password	(cached)
```

Notes: argon2id m=64MiB t=2 p=1 (237 ms locally); list = SecLists xato top 100k (README beside it); CLI uses DATABASE_URL as the owner connection and needs superuser/BYPASSRLS because owner has no insert policies under forced RLS.

## Fix round 1

I1: migration 00004 adds mhai_provision (insert-only, RLS insert policies, SELECT on hospitals(id, short_code) only); ids generated in Go; account.inTx runs SET LOCAL ROLE mhai_provision; CheckLoginRole refuses it; tests run as a non-superuser prov login; migrate tests expect 4.
M3: breached list rebuilt from full xato 10M list, entries of 12+ chars: 515,286 entries (README updated). Check(pw, email) adds repeated-char, sequence/keyboard-row and email-local-part rejections.
Also: M1 floors + NeedsRehash, M2 strict parsing and caps, M4 fail closed + Preload (serve and CLI), M5 long/invalid UTF-8 stdin, M6 ValidateNewUser before prompt, M7 ErrBadName, M9 interleaved timing test, M10 NFC, M11 payload via=cli. Skipped M8 (existing tests insert lowercase short codes).

### Red (new tests, before implementation)
```
internal/password/password_test.go:53:34: too many arguments in call to Check
internal/password/password_test.go:56:34: too many arguments in call to Check
internal/password/password_test.go:59:46: too many arguments in call to Check
internal/password/password_test.go:65:33: too many arguments in call to Check
internal/password/password_test.go:68:34: too many arguments in call to Check
internal/password/password_test.go:71:44: too many arguments in call to Check
internal/password/password_test.go:100:12: undefined: Preload
internal/password/password_test.go:108:23: too many arguments in call to Check
internal/password/password_test.go:108:44: undefined: ErrPattern
internal/password/password_test.go:113:39: too many arguments in call to Check
--- FAIL: TestMigrateUpFromEmpty (1.12s)
--- FAIL: TestCheckLoginRole (0.00s)
--- FAIL: TestCheckLoginRoleRefusesInheritedRoles (0.04s)
--- FAIL: TestCheckLoginRoleRefusesDatabaseAndSchemaOwners (0.04s)
--- FAIL: TestCheckLoginRoleRefusesProvisionLogin (0.00s)
--- FAIL: TestProvisionRoleCannotBypassAnything (0.00s)
--- FAIL: TestProvisionReadsOnlyHospitalIDAndCode (0.00s)
--- FAIL: TestProvisionCannotUpdateOrDelete (0.00s)
--- FAIL: TestProvisionInsertPoliciesHold (0.00s)
```
### Green
```
ok  	github.com/Measured-Health/mhai-multiagent/gateway/cmd/gateway	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/account	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/audit	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/config	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/httpx	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/logx	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/password	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/cmd/gateway	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/account	8.957s
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/audit	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/bus	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/config	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/db	4.195s
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/httpx	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/logx	(cached)
ok  	github.com/Measured-Health/mhai-multiagent/gateway/internal/password	(cached)
```
