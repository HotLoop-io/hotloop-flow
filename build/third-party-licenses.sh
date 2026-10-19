#!/usr/bin/env bash
# Writes THIRD_PARTY_LICENSES for one shipped binary, and fails the build if
# anything compiled into that binary carries a license we haven't agreed to
# ship, or if the list comes up short of what the binary actually contains.
#
#   build/third-party-licenses.sh -n "HotLoop Gateway" -b /out/hotloop \
#     -o /out/THIRD_PARTY_LICENSES ./cmd/hotloop
#
#   -n NAME             what the file says it's for
#   -b BINARY           the binary go build just wrote; the list is checked
#                       against the modules it says are linked into it
#   -o FILE             where the file goes
#   -v "NAME|SPDX|FILE" a component vendored into this repo, and its license
#                       file. Repeatable. Same allowlist as everything else.
#   -a FILE             a finished section another build stage wrote, appended
#                       as is. Repeatable.
#   the rest            the main package(s), exactly what go build was given
#
# Why: MIT, BSD, ISC and Apache all say the notice goes with the binary. The
# images shipped the binary and none of the notices. Now the list comes out of
# the binary's own import graph at build time, so it can't drift from what got
# compiled in, and a GPL, AGPL, LGPL or unidentifiable license stops the build
# instead of riding along into a release.
#
# Run it in the image's Go build stage with the same GOOS, GOARCH and
# CGO_ENABLED as the go build. The graph is per platform: a package can import
# something on arm64 it never touches on amd64, and the list has to match the
# binary that ships, not the machine that built it.
#
# Needs go-licenses on PATH: go install github.com/google/go-licenses/v2@v2.0.1
set -euo pipefail

# The licenses we ship. Anything else fails the build, and that's the point:
# adding one here is a decision somebody makes on purpose with a reason in the
# commit, never a flag somebody flips to get a release out the door.
#
# The permissive ones only want their text included, and it is.
# MPL-2.0 and EPL-2.0 are file-level copyleft: fine for a module compiled in
# unmodified, but a change to one of their files gets published under the same
# license. Eclipse Paho (MQTT) is EPL-2.0 or EDL-1.0 (BSD), our pick, and both
# texts go in the file.
# LicenseRef-MIT-Lucent is the 1998 Lucent Technologies notice on goja's ftoa
# package (number formatting in the JavaScript engine Cascade and Flow embed):
# use it for anything, keep the notice, don't use Lucent's name. It stopped
# the first Cascade build until somebody read it and added it here.
# Never: GPL, AGPL, LGPL (the binary is static, so LGPL's relink clause can't be
# met), SSPL, BUSL, Commons Clause, or anything nobody can identify. That is
# the rule for what we depend on. Our own packages are skipped below with
# --ignore, and they are BUSL-1.1.
ALLOWED=(
  MIT MIT-0 ISC 0BSD BSD-2-Clause BSD-3-Clause Apache-2.0 Zlib BSL-1.0
  Unlicense CC0-1.0 LicenseRef-MIT-Lucent
  MPL-2.0 EPL-2.0
)

name="" bin="" out=""
vendored=() appends=()
while getopts "n:b:o:v:a:" opt; do
  case $opt in
    n) name=$OPTARG ;;
    b) bin=$OPTARG ;;
    o) out=$OPTARG ;;
    v) vendored+=("$OPTARG") ;;
    a) appends+=("$OPTARG") ;;
    *) exit 2 ;;
  esac
done
shift $((OPTIND - 1))
if [ -z "$name" ] || [ -z "$bin" ] || [ -z "$out" ] || [ $# -eq 0 ]; then
  echo "usage: $0 -n NAME -b BINARY -o FILE [-v 'NAME|SPDX|FILE']... [-a FILE]... PACKAGE..." >&2
  exit 2
fi

allowed() {
  local l
  for l in "${ALLOWED[@]}"; do [ "$l" = "$1" ] && return 0; done
  return 1
}

fail() {
  echo >&2
  echo "THIRD-PARTY LICENSE CHECK FAILED for $name" >&2
  echo "$@" >&2
  echo "Allowed: ${ALLOWED[*]}" >&2
  echo "Don't widen the allowlist to get past this. Replace the dependency, or" >&2
  echo "get a yes from whoever owns licensing first." >&2
  exit 1
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Our own packages aren't third party. --ignore still walks their imports.
self=$(go list -m)
goos=$(go env GOOS) goarch=$(go env GOARCH)

# go-licenses' own gate: fails on a module with no license file at all, or one
# whose license isn't on the list.
if ! go-licenses check "$@" --ignore "$self" \
    --allowed_licenses="$(IFS=,; echo "${ALLOWED[*]}")" 2>"$work/check.log"; then
  grep -v '^W[0-9]' "$work/check.log" >&2 || true
  fail "go-licenses check refused the graph for $goos/$goarch (above)."
fi

# One row per module and license, tab separated. LicensePath is the file
# go-licenses identified; the other license files beside it (a NOTICE, the
# second half of a dual license) get picked up further down.
cat >"$work/rows.tpl" <<'EOF'
{{ range . }}{{ .Name }}	{{ .Version }}	{{ .LicenseName }}	{{ .LicensePath }}
{{ end }}
EOF
if ! go-licenses report "$@" --ignore "$self" --template "$work/rows.tpl" \
    >"$work/rows" 2>"$work/report.log"; then
  grep -v '^W[0-9]' "$work/report.log" >&2 || true
  fail "go-licenses report failed for $goos/$goarch (above)."
fi
sed -i '/^[[:space:]]*$/d' "$work/rows"
[ -s "$work/rows" ] || fail "go-licenses found no third-party modules at all, which is wrong for anything we ship."

# check lets through a module whose license file it found but couldn't
# classify, because it only compares the names it did find. The report calls
# that one Unknown, so it's caught here instead.
bad=""
while IFS=$'\t' read -r mod ver lic path; do
  allowed "$lic" || bad="$bad    $mod $ver: $lic ($path)"$'\n'
done <"$work/rows"
for v in "${vendored[@]}"; do
  IFS='|' read -r vname vlic vfile <<<"$v"
  [ -s "$vfile" ] || fail "vendored component $vname: license file $vfile is missing or empty."
  allowed "$vlic" || bad="$bad    $vname (vendored): $vlic"$'\n'
done
[ -z "$bad" ] || fail "Not on the allowlist, or not identifiable:"$'\n'"$bad"
for a in "${appends[@]}"; do
  [ -s "$a" ] || fail "section $a is missing or empty."
done

# The binary records every module linked into it. Each one has to be in the
# list at the version that was linked, or the list is short and the build
# stops. go-licenses names a module by its packages' common prefix, which is
# why golang.org/x/time shows up as golang.org/x/time/rate. It also drops
# the +incompatible off a pre-modules v2 like go-sourcemap's v2.1.3, so that
# comes off here too, or the check calls a listed module missing.
go version -m "$bin" | awk '$1 == "dep" { sub(/\+incompatible$/, "", $3); print $2 "\t" $3 }' >"$work/linked"
[ -s "$work/linked" ] || fail "$bin carries no module build info. Was it built from this module?"
missing=""
while IFS=$'\t' read -r mod ver; do
  awk -F'\t' -v m="$mod" -v v="$ver" '
    ($1 == m || index($1, m "/") == 1) && $2 == v { found = 1 }
    END { exit !found }' "$work/rows" || missing="$missing    $mod $ver"$'\n'
done <"$work/linked"
if [ -n "$missing" ]; then
  echo >&2
  echo "THIRD-PARTY LICENSE LIST IS SHORT for $name" >&2
  echo "Linked into $bin but missing from the list:" >&2
  printf '%s' "$missing" >&2
  echo "Pass the same package(s) go build was given, with the same GOOS, GOARCH" >&2
  echo "and CGO_ENABLED." >&2
  exit 1
fi

# Every license file beside the one go-licenses picked: NOTICE (Apache-2.0
# section 4(d) says it travels with the binary too), and the other text of a
# dual license, like Paho's edl-v10 next to its EPL LICENSE.
license_files() {
  local f base lower
  for f in "$1"/*; do
    [ -f "$f" ] || continue
    base=${f##*/}
    lower=$(printf '%s' "$base" | tr '[:upper:]' '[:lower:]')
    case $lower in
      *.go | *.s | *.c | *.h | *.json | *.yml | *.yaml | *.proto | *.sh | *.py) continue ;;
    esac
    case $lower in
      licen[cs]e* | unlicen[cs]e* | copying* | notice* | patents* | copyright* | \
        *-licen[cs]e* | *_licen[cs]e* | edl-v* | epl-v*) echo "$f" ;;
    esac
  done | sort
}

rule='================================================================================'
thin='--------------------------------------------------------------------------------'
goroot=$(go env GOROOT)
gover=$(go env GOVERSION)

# One section per module: a module under two licenses (the MCP SDK is MIT and
# Apache-2.0) gets both names on one section.
awk -F'\t' '{
  key = $1 "\t" $2 "\t" $4
  if (!(key in lic)) { order[++n] = key; lic[key] = $3 } else { lic[key] = lic[key] ", " $3 }
} END { for (i = 1; i <= n; i++) print order[i] "\t" lic[order[i]] }' "$work/rows" >"$work/mods"

{
  echo "Third-party software in $name"
  echo "$rule"
  echo
  echo "$name is built from HotLoop's source and the third-party software"
  echo "listed here. Each piece is used under its own license, and every license"
  echo "text is reproduced in full below."
  echo
  echo "This file is generated when the image is built, from the import graph of"
  echo "the binary itself ($goos/$goarch, $gover, go-licenses v2.0.1), and"
  echo "checked against the modules the binary records as linked in. It lists"
  echo "what is in this binary and nothing else."
  echo
  printf '%-58s %-20s %s\n' "Component" "Version" "License"
  printf '%-58s %-20s %s\n' "Go standard library and runtime" "$gover" "BSD-3-Clause"
  while IFS=$'\t' read -r mod ver path lic; do
    printf '%-58s %-20s %s\n' "$mod" "$ver" "$lic"
  done <"$work/mods"
  for v in "${vendored[@]}"; do
    IFS='|' read -r vname vlic vfile <<<"$v"
    printf '%-58s %-20s %s\n' "$vname" "(vendored)" "$vlic"
  done
  echo

  # The runtime and standard library are in every Go binary, and Go's BSD
  # license wants its notice in a binary redistribution like any other.
  echo "$rule"
  echo "Go standard library and runtime $gover"
  echo "License: BSD-3-Clause"
  echo "$thin"
  for f in "$goroot/LICENSE" "$goroot/PATENTS"; do
    [ -f "$f" ] || continue
    echo "[${f##*/}]"
    cat "$f"
    echo
  done

  while IFS=$'\t' read -r mod ver path lic; do
    echo "$rule"
    echo "$mod $ver"
    echo "License: $lic"
    echo "$thin"
    # The file go-licenses identified always goes in, whatever it's called.
    printf '%s\n%s\n' "$path" "$(license_files "$(dirname "$path")")" |
      awk 'NF && !seen[$0]++' | while read -r f; do
        echo "[${f##*/}]"
        cat "$f"
        echo
      done
  done <"$work/mods"

  for v in "${vendored[@]}"; do
    IFS='|' read -r vname vlic vfile <<<"$v"
    echo "$rule"
    echo "$vname (vendored in this repository)"
    echo "License: $vlic"
    echo "$thin"
    cat "$vfile"
    echo
  done

  for a in "${appends[@]}"; do
    cat "$a"
  done
} >"$out"

# A count in the build log, so a release shows at a glance what it ships.
echo "$name ($goos/$goarch): $(wc -l <"$work/mods") license sections covering all $(wc -l <"$work/linked") linked modules; written to $out" >&2
cut -f4 "$work/mods" | tr ',' '\n' | sed 's/^ *//' | sort | uniq -c | sed 's/^/  /' >&2
