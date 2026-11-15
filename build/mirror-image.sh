#!/usr/bin/env bash
# Make an image available to this runner from the HotLoop mirror on ghcr.io, and
# fill the mirror from Docker Hub the first time it is asked for something it
# does not hold.
#
# CI must not depend on anonymous pulls from Docker Hub: they are rate limited per
# address, GitHub's runners share addresses, and a test job died on
# "toomanyrequests" pulling the TinyGo image. So CI names every image by its place
# in the mirror, ghcr.io/hotloop-io/mirror/<path>, which is the Docker Hub name
# with docker.io/ replaced by that prefix:
#
#   ghcr.io/hotloop-io/mirror/library/postgres:17    is docker.io/library/postgres:17
#   ghcr.io/hotloop-io/mirror/tinygo/tinygo@sha256:… is docker.io/tinygo/tinygo@sha256:…
#
# Usage: build/mirror-image.sh <mirror reference>...
#
# For each one: pull it from the mirror; if it is not there, copy it from Docker
# Hub with every platform and the same digests (skopeo copy --all
# --preserve-digests), so the mirror holds exactly what Docker Hub does and a
# reference by digest means the same thing in both, then pull it. Hub is asked
# only when the mirror cannot answer, and with backoff when it says to slow down.
# Where the token cannot write to ghcr.io (a pull request from a fork), the image
# is pulled from Docker Hub and tagged with the mirror's name, so the run goes on.

# Root's podman, unless the caller names one. sudo keeps the runner user's
# XDG_RUNTIME_DIR, and root's podman then writes its registry login into
# /run/user/<uid>/containers/auth.json owned by root, so the user's own podman
# login after it fails with "permission denied" (v2.10.1's image run). Without
# XDG_RUNTIME_DIR root keeps its login in /run/containers/0/auth.json.
PODMAN=${PODMAN:-sudo env -u XDG_RUNTIME_DIR podman}

set -euo pipefail

# build/mirror-pins.txt says which digest a tag has to be: "<mirror path> sha256:...".
# A tag that moved on Docker Hub is a different image than the one CI was written
# against, so an image with a pin is checked after it is pulled.
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
check_pin() {
  local ref="$1" path want got
  path="${ref#"$PREFIX"}"
  want=$(awk -v p="$path" '$1 == p { print $2 }' "$here/mirror-pins.txt" 2>/dev/null || true)
  [ -z "$want" ] && return 0
  got=$($PODMAN image inspect --format '{{.Digest}}' "$ref")
  if [ "$got" != "$want" ]; then
    echo "mirror-image: $ref is $got, and build/mirror-pins.txt says it must be $want" >&2
    return 1
  fi
}

PREFIX="ghcr.io/hotloop-io/mirror/"

# The mirror's packages are private until someone with the org's settings makes
# them public, so read it signed in. A token that cannot is not an error here:
# the pull below then fails and Docker Hub is the fallback.
if [ -n "${GITHUB_TOKEN:-}" ]; then
  printf '%s' "$GITHUB_TOKEN" | $PODMAN login ghcr.io -u "${GITHUB_ACTOR:-x}" --password-stdin >/dev/null 2>&1 || true
fi

for dst in "$@"; do
  case "$dst" in
    "$PREFIX"*) ;;
    *) echo "mirror-image: $dst is not under $PREFIX" >&2; exit 2 ;;
  esac
  if [[ "$dst" == *@* ]]; then
    echo "mirror-image: $dst is a digest reference; use a tag and a line in build/mirror-pins.txt" >&2
    exit 2
  fi
  src="docker.io/${dst#"$PREFIX"}"

  if $PODMAN pull -q "$dst" >/dev/null 2>&1; then
    check_pin "$dst"
    echo "mirror-image: $dst from the mirror"
    continue
  fi

  copied=""
  if [ -n "${GITHUB_TOKEN:-}" ]; then
    for attempt in 1 2 3 4 5 6; do
      if skopeo copy --all --preserve-digests --src-no-creds \
          --dest-creds "${GITHUB_ACTOR:-x}:${GITHUB_TOKEN}" "docker://$src" "docker://$dst" >/dev/null 2>"${TMPDIR:-/tmp}/mirror-image.err"; then
        copied=1
        break
      fi
      echo "mirror-image: copying $src failed (attempt $attempt of 6): $(tail -n 2 "${TMPDIR:-/tmp}/mirror-image.err" | tr '\n' ' ' | cut -c1-300)" >&2
      sleep $((attempt * 20))
    done
  fi
  if [ -n "$copied" ] && $PODMAN pull -q "$dst" >/dev/null; then
    check_pin "$dst"
    echo "mirror-image: $dst copied into the mirror from Docker Hub"
    continue
  fi

  echo "mirror-image: the mirror could not be filled, pulling $src from Docker Hub" >&2
  for attempt in 1 2 3 4 5 6; do
    if $PODMAN pull -q "$src" >/dev/null; then
      $PODMAN tag "$src" "$dst"
      check_pin "$dst"
      break
    fi
    [ "$attempt" = 6 ] && { echo "mirror-image: could not get $src at all" >&2; exit 1; }
    sleep $((attempt * 20))
  done
done
