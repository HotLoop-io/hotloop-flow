#!/usr/bin/env bash
# Pushes the release's tags for an image that was built for some platforms and
# may not have been built for all of them yet.
#
#   build/publish-tags.sh IMAGE VERSION FRESH_LIST_LOCAL "amd64" [arm64 ...]
#
# VERSION is X.Y.Z without the v. FRESH_LIST_LOCAL is the local manifest list the
# release just built, and the platforms after it are the ones it holds.
#
# X.Y.Z goes out as built. X.Y and X are tags other boxes follow, and a box on
# another architecture pulls whatever they hold: a run that builds only amd64
# would otherwise replace a list that has an arm64 image with one that has none,
# and take the image away from every arm64 box that follows the tag. So for X.Y
# and X, the list that goes out holds the new image for each platform this run
# built, and for every other platform, the image the tag already points at. A tag
# that does not exist yet has nothing to keep. When a run builds every platform,
# nothing is kept and the lists are the new ones, which is the normal case.
#
# Needs podman, jq, and a login that can push.
set -euo pipefail

IMAGE=$1 VERSION=$2 FRESH=$3
shift 3
BUILT=("$@")

TAGS="$VERSION"
case "$VERSION" in
  *-*) ;;
  *) MINOR="${VERSION%.*}"; MAJOR="${VERSION%%.*}"
     TAGS="$TAGS $MINOR"
     [ "$MAJOR" = 0 ] || TAGS="$TAGS $MAJOR" ;;
esac

built() { local a; for a in "${BUILT[@]}"; do [ "$a" = "$1" ] && return 0; done; return 1; }

for T in $TAGS; do
  if [ "$T" = "$VERSION" ]; then
    podman manifest push -q --all "$FRESH" "docker://$IMAGE:$T"
    echo "pushed $T: ${BUILT[*]}"
    continue
  fi
  # What the tag points at now. A tag that is not there yet is a 404, which is fine.
  if OLD=$(podman manifest inspect "$IMAGE:$T" 2>/dev/null) && [ -n "$OLD" ]; then
    KEEP=$(printf '%s' "$OLD" | jq -r '.manifests[]? | select(.platform.os == "linux") | [.platform.architecture, .digest] | @tsv')
  else
    KEEP=""
  fi
  LIST="localhost/hotloop-flow-tag-$T"
  podman manifest rm "$LIST" >/dev/null 2>&1 || true
  podman manifest create "$LIST" >/dev/null
  for A in "${BUILT[@]}"; do
    podman manifest add "$LIST" "containers-storage:localhost/hotloop-flow:$A" >/dev/null
  done
  kept=""
  while IFS=$'\t' read -r ARCH DIGEST; do
    [ -z "${ARCH:-}" ] && continue
    built "$ARCH" && continue
    podman manifest add "$LIST" "docker://$IMAGE@$DIGEST" >/dev/null
    kept="$kept $ARCH"
  done <<< "$KEEP"
  podman manifest push -q --all "$LIST" "docker://$IMAGE:$T"
  echo "pushed $T: ${BUILT[*]} new${kept:+, kept$kept from the tag as it was}"
done
