#!/usr/bin/env bash
# Proves build/publish-tags.sh against a registry of its own, with no GitHub in
# it: a run that builds amd64 only must leave arm64 on X.Y and X, must put amd64
# only on the new X.Y.Z, and a run that builds both must replace both.
#
#   build/test-publish-tags.sh            (needs podman, jq, and network for the images)
#
# Every name it makes starts with flow-pub-test, and it removes them all.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
PORT=${PUBLISH_TEST_PORT:-18871}
REG=localhost:$PORT
IMAGE=$REG/hotloop-flow
C=flow-pub-test-registry
W=$(mktemp -d)
cleanup() { podman rm -f -t 1 "$C" >/dev/null 2>&1 || true; podman manifest rm localhost/flow-pub-test-old localhost/hotloop-flow-release >/dev/null 2>&1 || true; podman rmi -f localhost/hotloop-flow:amd64 localhost/hotloop-flow:arm64 >/dev/null 2>&1 || true; rm -rf "$W"; }
trap cleanup EXIT
# this registry is plain http on the loopback: say so in a registries file of our own
cat > "$W/registries.conf" <<EOC
[[registry]]
location = "$REG"
insecure = true
EOC
export CONTAINERS_REGISTRIES_CONF="$W/registries.conf"

podman run -d --name "$C" -p 127.0.0.1:$PORT:5000 docker.io/library/registry:2 >/dev/null
for i in $(seq 1 30); do curl -sf "http://$REG/v2/" >/dev/null && break; sleep 0.5; done

digests() { # tag -> "arch:shortdigest ..." sorted
  podman manifest inspect "$IMAGE:$1" | jq -r '[.manifests[] | .platform.architecture + ":" + .digest[7:19]] | sort | join(" ")'
}
fail() { echo "FAIL: $*" >&2; exit 1; }

# The old release: a list with an amd64 and an arm64 image on 2.6.0, 2.6 and 2.
podman pull -q --platform linux/amd64 docker.io/library/alpine:3.22 >/dev/null
podman tag docker.io/library/alpine:3.22 localhost/flow-pub-test-old-amd64
podman pull -q --platform linux/arm64 docker.io/library/alpine:3.22 >/dev/null
podman tag docker.io/library/alpine:3.22 localhost/flow-pub-test-old-arm64
podman manifest create localhost/flow-pub-test-old >/dev/null
podman manifest add localhost/flow-pub-test-old containers-storage:localhost/flow-pub-test-old-amd64 >/dev/null
podman manifest add localhost/flow-pub-test-old containers-storage:localhost/flow-pub-test-old-arm64 >/dev/null
for T in 2.6.0 2.6 2; do podman manifest push -q --all localhost/flow-pub-test-old "docker://$IMAGE:$T" >/dev/null; done
OLD=$(digests 2.6.0)
echo "before: 2.6.0, 2.6 and 2 are: $OLD"
[ "$(digests 2)" = "$OLD" ] && [ "$(digests 2.6)" = "$OLD" ] || fail "setup"
OLD_ARM=$(echo "$OLD" | tr ' ' '\n' | grep '^arm64:')

# The new release, amd64 only: a different image from the old amd64.
podman pull -q --platform linux/amd64 docker.io/library/busybox:1.37 >/dev/null
podman tag docker.io/library/busybox:1.37 localhost/hotloop-flow:amd64
podman manifest create localhost/hotloop-flow-release localhost/hotloop-flow:amd64 >/dev/null
"$HERE/publish-tags.sh" "$IMAGE" 2.7.0 localhost/hotloop-flow-release amd64
NEW_AMD=$(digests 2.7.0)
echo "after an amd64 run:"
for T in 2.7.0 2.7 2 2.6 2.6.0; do echo "  $T: $(digests $T)"; done
echo "$NEW_AMD" | grep -q '^amd64:' && [ "$(echo "$NEW_AMD" | wc -w)" = 1 ] || fail "2.7.0 should hold exactly one amd64 image: $NEW_AMD"
[ "$(digests 2.7)" = "$NEW_AMD" ] || fail "2.7 should be the new amd64 image alone"
[ "$(digests 2)" = "$NEW_AMD $OLD_ARM" ] || fail "2 should be the new amd64 and the old arm64: got $(digests 2)"
[ "$(digests 2.6)" = "$OLD" ] && [ "$(digests 2.6.0)" = "$OLD" ] || fail "2.6 and 2.6.0 must not move"
case "$(digests 2)" in *"$(echo "$OLD" | tr ' ' '\n' | grep '^amd64:')"*) fail "2 still holds the old amd64";; esac

# The follow-up, both platforms: everything is replaced and nothing is kept.
podman pull -q --platform linux/arm64 docker.io/library/busybox:1.37 >/dev/null
podman tag docker.io/library/busybox:1.37 localhost/hotloop-flow:arm64
podman manifest add localhost/hotloop-flow-release containers-storage:localhost/hotloop-flow:arm64 >/dev/null
"$HERE/publish-tags.sh" "$IMAGE" 2.7.1 localhost/hotloop-flow-release amd64 arm64
BOTH=$(digests 2.7.1)
echo "after a both platform run: 2.7.1: $BOTH"
[ "$(echo "$BOTH" | wc -w)" = 2 ] || fail "2.7.1 should hold two images"
[ "$(digests 2.7)" = "$BOTH" ] && [ "$(digests 2)" = "$BOTH" ] || fail "2.7 and 2 should be the new pair"
echo "PUBLISH TAGS OK"
