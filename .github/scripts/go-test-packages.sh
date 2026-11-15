#!/usr/bin/env bash
# Lists the packages of the Go module in the current directory whose test
# binary changes between two sets of build tags, or the ones it doesn't.
#
#   go-test-packages.sh differ "<tags a>" "<tags b>"   test build differs with b
#   go-test-packages.sh same   "<tags a>" "<tags b>"   test build is identical
#   go-test-packages.sh test same|differ "<tags a>" "<tags b>" [go test flags]
#       go test those packages: "same" with tags a, "differ" with tags b
#
# A package's test binary is the same under both tag sets when neither it,
# its test files nor any package it links pick different files. Running such
# a package twice runs the same code twice, so CI runs it once (the core
# jobs in .github/workflows/ci.yml): "same" and "differ" split ./... with
# nothing left out, and a package that newly gains a tagged file moves to
# "differ" by itself.
set -euo pipefail
if [ "${1:-}" = test ]; then
  which=${2:?usage: go-test-packages.sh test same|differ "<tags a>" "<tags b>" [go test flags]}
  ta=${3?tags a}
  tb=${4?tags b}
  shift 4
  out=$(bash "$0" "$which" "$ta" "$tb")
  tags=$ta
  [ "$which" = differ ] && tags=$tb
  if [ -z "$out" ]; then
    echo "no package's test build is $which between tags \"$ta\" and \"$tb\": nothing to run"
    exit 0
  fi
  mapfile -t pkgs <<<"$out"
  echo "go test -tags \"$tags\" $* on ${#pkgs[@]} packages"
  exec go test -tags "$tags" "$@" "${pkgs[@]}"
fi
mode=${1:?usage: go-test-packages.sh differ|same "<tags a>" "<tags b>"}
a=${2?tags a}
b=${3?tags b}
case "$mode" in differ|same) ;; *) echo "go-test-packages.sh: unknown mode $mode" >&2; exit 2 ;; esac

# The files a package compiles from, as linked into a test binary: a test
# variant ("p [p.test]", "p_test [p.test]") lists its test files here too.
files='{{.ImportPath}}|{{.GoFiles}}{{.CgoFiles}}{{.SFiles}}{{.EmbedFiles}}'
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
# Every package any test binary links, with its files, under each tag set.
go list -e -deps -test -tags "$a" -f "$files" ./... | sort -u > "$tmp/a"
go list -e -deps -test -tags "$b" -f "$files" ./... | sort -u > "$tmp/b"
# The ones whose files differ, or that exist under one tag set only.
comm -3 "$tmp/a" "$tmp/b" | sed 's/^\t//; s/|.*//' | sort -u > "$tmp/changed"
# Each test binary's packages (itself and everything it links), under b.
go list -e -test -tags "$b" -f '{{.ImportPath}}|{{.ForTest}}|{{join .Deps ","}}' ./... > "$tmp/bins"
# The same under a: a dependency b dropped is in a's list only.
go list -e -test -tags "$a" -f '{{.ImportPath}}|{{.ForTest}}|{{join .Deps ","}}' ./... >> "$tmp/bins"
go list -e -f '{{.ImportPath}}' ./... | sort -u > "$tmp/all"

awk -F'|' -v changed="$tmp/changed" '
  BEGIN { while ((getline p < changed) > 0) bad[p] = 1 }
  {
    pkg = $2 != "" ? $2 : $1
    sub(/ \[.*\]$/, "", pkg); sub(/\.test$/, "", pkg); sub(/_test$/, "", pkg)
    if ($1 in bad) { hit[pkg] = 1; next }
    n = split($3, d, ",")
    for (i = 1; i <= n; i++) if (d[i] in bad) { hit[pkg] = 1; break }
  }
  END { for (p in hit) print p }
' "$tmp/bins" | sort -u > "$tmp/differ"

if [ "$mode" = differ ]; then
  comm -12 "$tmp/all" "$tmp/differ"
else
  comm -23 "$tmp/all" "$tmp/differ"
fi
