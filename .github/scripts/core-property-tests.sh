#!/usr/bin/env bash
# The core service's property tests in CI: which tests the property jobs
# run, which ones the -race job leaves to them, and the check that nothing
# falls between the two. Run from services/core.
#
#   core-property-tests.sh run disclosure|history   the property job (PROPERTY_SHARD=i/n)
#   core-property-tests.sh race-skip                the -skip pattern for go test -race
#   core-property-tests.sh check                    every skipped test runs in a property job
#
# The property jobs run these at their non-race size, which draws the same
# cases as the -race size and more (internal/analysis/disclose seeds(),
# internal/smallgroup historyCount()): the race job skips them rather than
# run a subset of the same cases again. The code they test runs on one
# goroutine per case.
set -euo pipefail

# The suites, as the property jobs have always selected them.
disclosure_pkgs=./internal/analysis/disclose/...
disclosure_run='TestP1|TestP2|TestP4|TestP5|TestOneStatePerGroupInFamily'
# A test whose one case costs a shard most of its time runs in a go test of
# its own, beside the rest, so the two overlap on the runner's CPUs.
disclosure_solo=TestP2Realistic
history_pkgs=./internal/smallgroup/...
history_solo=
history_run='TestHistoryAnchoredAtOnboarding|TestUnionsOfHiddenWeeksMeasured|TestFullyHiddenAlwaysPasses|TestHistoryFromRouteJSON|TestHistorySetsFromRouteJSON|TestModularAgreesWithRational|TestFallbackAllHiddenAddsNothing'

# The costly tests the race job leaves to the property jobs, by full name.
# (TestP2Realistic, TestP2CoversSources, TestP2LedgerIncremental and
# TestP5BoundsReported skip themselves under -race.)
race_skip=(
  TestP1NoHiddenCellSolvable TestP2FamilyExhaustive TestP4HistoryNoExposure TestOneStatePerGroupInFamily
  TestHistoryAnchoredAtOnboarding TestUnionsOfHiddenWeeksMeasured TestFullyHiddenAlwaysPasses
  TestHistoryFromRouteJSON TestHistorySetsFromRouteJSON TestModularAgreesWithRational TestFallbackAllHiddenAddsNothing
)

skip_pattern() { local IFS='|'; echo "^(${race_skip[*]})\$"; }

case "${1:-}" in
  run)
    case "${2:-}" in
      disclosure) pkgs=$disclosure_pkgs run=$disclosure_run solo=$disclosure_solo ;;
      history) pkgs=$history_pkgs run=$history_run solo=$history_solo ;;
      *) echo "core-property-tests.sh: run disclosure|history" >&2; exit 2 ;;
    esac
    echo "PROPERTY_SHARD=${PROPERTY_SHARD:-} (unset: every case); $(nproc) CPUs"
    # Build once, so the go tests below only link.
    go test -count=1 -run '^$' $pkgs >/dev/null
    logs=$(mktemp -d)
    # -v: each case's time and the properties' counts are in the log.
    gotest() { go test -count=1 -timeout 30m -v "$@" $pkgs 2>&1 | grep -Ev '^=== (RUN|PAUSE|CONT)'; }
    pids=()
    if [ -n "$solo" ]; then
      gotest -run "^($solo)\$" > "$logs/solo" &
      pids+=($!)
      gotest -run "$run" -skip "^($solo)\$" > "$logs/rest" &
      pids+=($!)
    else
      gotest -run "$run" > "$logs/rest" &
      pids+=($!)
    fi
    status=0
    for p in "${pids[@]}"; do wait "$p" || status=1; done
    for f in "$logs"/*; do echo "::group::$(basename "$f")"; cat "$f"; echo "::endgroup::"; done
    grep -h -E '^(--- FAIL|FAIL|ok|panic)' "$logs"/* || true
    rm -rf "$logs"
    exit "$status"
    ;;
  race-skip)
    skip_pattern
    ;;
  check)
    bad=0
    for name in "${race_skip[@]}"; do
      # Defined once, in a property package...
      files=$(grep -rlE "^func $name\(t \*testing\.T\)" --include='*_test.go' . || true)
      case "$(echo "$files" | wc -w)" in
        1) ;;
        *) echo "::error::$name: want one definition, found: ${files:-none}"; bad=1; continue ;;
      esac
      case "$files" in
        ./internal/analysis/disclose/*) run=$disclosure_run ;;
        ./internal/smallgroup/*) run=$history_run ;;
        *) echo "::error::$name is skipped under -race but $files isn't a property package"; bad=1; continue ;;
      esac
      # ...and selected by that package's property job.
      if ! grep -qE "^($run)" <<<"$name"; then
        echo "::error::$name is skipped under -race but no property job runs it"; bad=1
      fi
    done
    [ "$bad" = 0 ] && echo "every test the race job skips runs in a property job"
    exit "$bad"
    ;;
  *)
    echo "usage: core-property-tests.sh run disclosure|history | race-skip | check" >&2
    exit 2
    ;;
esac
