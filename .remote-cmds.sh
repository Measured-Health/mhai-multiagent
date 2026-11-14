set -o pipefail
cd services/core || exit 1
echo "== gofmt"; test -z "$(gofmt -l .)" || { gofmt -l .; echo GOFMT_FAIL; }
echo "== vet"; go vet ./... && go vet -tags smoke ./internal/smoke/... || echo VET_FAIL
echo "== staticcheck"; go run honnef.co/go/tools/cmd/staticcheck@2025.1 ./... || echo STATICCHECK_FAIL
echo "== permgen"; go run ./cmd/permgen && (cd ../.. && git status --short 2>/dev/null; git diff --stat 2>/dev/null) ; echo permgen-done
echo "== unit race"; go test -race -timeout 25m ./... 2>&1 | grep -vE "^ok|no test files" ; echo unit-done
echo "== integration"; go test -race -timeout 25m -tags integration ./... 2>&1 | grep -vE "^ok|no test files"; echo integ-done
echo "== integration test"; go test -race -timeout 25m -tags "integration test" ./... 2>&1 | grep -vE "^ok|no test files"; echo integ-test-done
echo "== analytics props"; go test -count=1 -timeout 30m -run 'TestHistoryAnchoredAtOnboarding|TestUnionsOfHiddenWeeksMeasured|TestFullyHiddenAlwaysPasses|TestHistoryFromRouteJSON|TestHistorySetsFromRouteJSON' ./internal/smallgroup/... 2>&1 | tail -3
echo "== analysis props"; go test -count=1 -timeout 60m -run 'TestP1|TestP2|TestP4|TestP5|TestOneStatePerGroupInFamily' ./internal/analysis/disclose/... 2>&1 | tail -3
echo "== emr"; cd ../emr && go test -race -timeout 25m -tags integration ./... 2>&1 | grep -vE "^ok|no test files"; echo emr-done
echo ALL-DONE
