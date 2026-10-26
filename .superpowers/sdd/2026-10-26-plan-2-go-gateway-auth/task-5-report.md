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
