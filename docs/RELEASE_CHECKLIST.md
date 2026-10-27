# HotLoop Flow: Release Checklist

Every step, in order. Each rule is here because Flow, or HotLoop's own releases
which this is modeled on, got bitten without it, and the section says how.

Commands are bash, run from the root of a checkout. `X.Y.Z` is the version
you're releasing, `PREV` the last one published.

## How a release works

Nothing publishes on a merge. `ci.yml` runs on every push to `main` and every
pull request, and publishes nothing. Two other workflows run on a `v*` tag and
only on a tag, side by side, neither waiting for the other:

| Workflow | What it does | Where it lands |
|---|---|---|
| `image-publish.yml` | Builds the one `Containerfile` with Podman, one platform at a time into one manifest list for linux/amd64 and linux/arm64, checks the list and each binary, and pushes `X.Y.Z`, `X.Y` and `X` | `ghcr.io/hotloop-io/hotloop-flow` |
| `helm-publish.yml` | Lints the chart, packages it at the tag's version, rebuilds `index.yaml` from that version alone | gh-pages, served at `https://hotloop.io/hotloop-flow/` |

They share nothing to race on: one writes ghcr, the other writes gh-pages.
Neither runs the test suite, and neither makes a GitHub release. So the gates
in section 4 are the only gates, and the GitHub release in section 7 is a step
you do by hand.

## 1. Pick the number, and make sure it's free

Minor (`2.1.0`) for anything new that doesn't break a flow file or the API.
Patch (`2.0.6`) when it only fixes things. Major only for a change that makes a
flow file, a config file or the data directory from `PREV` stop working, and
that is Patrick's call before it is anybody else's. The byte-exact round trip of
a flow file is a hard rule: a change that touches it is a major, or it doesn't
ship.

Read the whole log before deciding. A title tells you nothing.

```bash
git fetch origin
git log --format='%h %s' vPREV..origin/main
git log --format='### %s%n%b' vPREV..origin/main
```

The number has to be free: no tag, no release.

```bash
git ls-remote --tags origin "vX.Y.Z"   # must print nothing
gh release list --limit 5              # X.Y.Z must not be there
```

Ask the remote, not your clone. And once a tag has published anything, the
number is gone for good. See section 6.

## 2. Where the version lives

### The chart does not carry it

`charts/hotloop-flow/Chart.yaml` says `version: 0.1.0` and `appVersion: "0.1.0"`
on purpose. `helm-publish.yml` packages with `--version` and `--app-version`
taken from the tag, and the deployment's image tag defaults to
`.Chart.AppVersion` (`templates/deployment.yaml`), so the published chart and
the image it points at cannot disagree. Bumping the placeholders changes
nothing that ships and only creates a second answer to the same question. Leave
them, and prove they are still the placeholders:

```bash
grep -E '^(version|appVersion):' charts/hotloop-flow/Chart.yaml   # 0.1.0 both
```

### The image carries it from the tag

`image-publish.yml` passes `VERSION=<tag>` as a build argument and the
`Containerfile` stamps it with `-X main.version=`. `cmd/hotloop-flow/main.go` has
`var version = "dev"`, which is what a `go build` from a checkout reports. Leave
that too.

### What you do bump

These name the version an operator is told to install. They are text, and no
workflow checks them, so nothing refuses a release that forgets them:

```bash
git grep -n -F 'PREV' -- README.md docs ':!docs/bench' ':!docs/ROADMAP.md'
```

Read each hit and decide. Most are the install commands and the "Current
release" line, and move to `X.Y.Z`. Some are history ("up to and including 2.0.4
...") and stay. Don't `sed` the lot.

`docs/bench/README.md` and the numbers table in the README say which version a
measurement was taken on. Don't change the version on a measurement you didn't
retake. If the numbers moved, retake them, with the command next to them.

Then prove nothing was missed:

```bash
git grep -n -F 'X.Y.Z' -- README.md docs
git grep -n -E '(hotloop-flow:|Current release is `)[0-9]+\.[0-9]+\.[0-9]+' -- README.md docs
```

The second one must show only `X.Y.Z`.

## 3. Notes

Build the list from what actually merged, and read every commit body, not just
its title (section 1 has the command). Do it again right before the release PR
merges: anything that landed after the branch was cut is in the release whether
or not the notes say so.

Two files in the release PR:

1. **`docs/releases/X.Y.Z.md`.** The text of the GitHub release (section 7). What
   it is, what's new, what changed, upgrade notes, new settings, and a link to
   the README at the tag. Written for the person running it on a plant floor,
   not the person who wrote it.
2. **The README's "Current release" line** and the install commands, per
   section 2.

What the notes have to tell an operator, spelled out, because nobody reads the
diff:

- **Upgrade steps from PREV.** Is it a plain `helm upgrade`? A new image tag? What
  happens on the first start? What can't be undone?
- **New settings,** by their YAML key and their environment variable, with the
  default.
- **Anything that behaves differently,** including a change that is deliberate.
  Read each commit for it.
- **What isn't proven.** If it only ran in a lab, say so.
- **A change to NOTICE or the licenses,** because it ships inside the image.

Check the body before anybody else sees it:

```bash
cat docs/releases/X.Y.Z.md
grep -nP '\xE2\x80[\x93\x94]' docs/releases/X.Y.Z.md   # must print nothing
```

The notes say what Flow does, in its own words. Same voice as the README, and
no em or en dashes.

The same notes go on the website in a second PR, after the release is public
(section 9).

## 4. Gates

All of these on the release branch, before the PR goes up. They are the same
steps `ci.yml` runs. A release is never less tested than a PR.

### Go: tidy, vet, the full suite with a real broker

CI starts two Mosquitto brokers, one open on 1883 and one that refuses
anonymous clients on 1884, builds the WASM test guest, and clones the published
JSONata suite at its pinned commit. Do the same, or half the tests skip and a
skip reads as a pass.

```bash
go mod tidy && git diff --exit-code go.mod go.sum
go vet ./...

GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o testdata/wasmguest/guest.wasm ./testdata/wasmguest
test -s testdata/wasmguest/guest.wasm

podman run -d --name flow-release-mosq -p 127.0.0.1:1883:1883 \
  docker.io/library/eclipse-mosquitto:2.1.2-alpine mosquitto -c /mosquitto-no-auth.conf

M=$(mktemp -d) && chmod 0755 "$M"
printf 'listener 1883\nallow_anonymous false\npassword_file /mosquitto/config/passwd\n' > "$M/mosquitto.conf"
touch "$M/passwd"
podman run --rm -v "$M:/mosquitto/config" docker.io/library/eclipse-mosquitto:2 \
  mosquitto_passwd -b /mosquitto/config/passwd line3 ci-broker-pass
chmod 0700 "$M/passwd"
podman run -d --name flow-release-mosq-auth -p 127.0.0.1:1884:1883 \
  -v "$M:/mosquitto/config" docker.io/library/eclipse-mosquitto:2

J=$(mktemp -d)
git -c advice.detachedHead=false clone -q --depth 1 --branch v2.2.2 \
  https://github.com/jsonata-js/jsonata.git "$J/jsonata-js"
test "$(git -C "$J/jsonata-js" rev-parse HEAD)" = 6c7e95fdbf4405a1e741852a7cd8cd985b4305bb

CGO_ENABLED=1 \
HOTLOOP_FLOW_TEST_MQTT=localhost:1883 \
HOTLOOP_FLOW_TEST_MQTT_AUTH=127.0.0.1:1884 \
HOTLOOP_FLOW_TEST_MQTT_USER=line3 \
HOTLOOP_FLOW_TEST_MQTT_PASSWORD=ci-broker-pass \
HOTLOOP_FLOW_JSONATA_SUITE="$J/jsonata-js/test/test-suite" \
  go test -race -count=1 ./...
```

Every package `ok`. The race detector needs cgo, so this is a build for the test
only; the shipped binary is `CGO_ENABLED=0`. Take the brokers down when you're
done, by name, and only those two:

```bash
podman rm -f flow-release-mosq flow-release-mosq-auth
```

Never `podman system prune` or a bulk remove: other containers on the box are
not yours.

Tests that skip are tests that didn't run. CI asserts the ones that matter
actually executed (the broker tests, the sandbox, the OpenSSL interop, the
JSONata count of 1673 of 1686). If you changed one of those, read its step in
`ci.yml`.

If the suite fails on a test that failed on `main` too, that is a flaky test.
Fix it in its own PR before you tag. Don't rerun until it's green and don't
widen a tolerance.

### The editor: typecheck, build, and the browser tests

```bash
(cd web && npm ci && npm run typecheck && npm run build)
test -s web/dist/index.html
CGO_ENABLED=0 go build -trimpath -buildvcs=false -o hotloop-flow ./cmd/hotloop-flow
grep -qa "FOUC guard" hotloop-flow && echo "editor is in the binary"
(cd web/e2e && npm ci && npx playwright install --with-deps chromium && npx playwright test)
```

The Playwright tests drive the binary that ships, editor embedded. All pass.

### The chart

```bash
helm lint charts/hotloop-flow
```

then the render checks in `ci.yml`'s `chart` job and `helm-publish.yml`'s `lint`
job, which are the ones a tag runs against the chart: the generated admin
password matches its hash, an explicit hash is used as given, tenant labels land
in seven places and only when asked for, discovery adds no capability, a mounted
Secret is read-only and in the secret scope, the store labels count eight and
nothing else under `embernet.ai/` appears, every network mode renders, and the
bad configurations are refused. If you changed the chart, run each of them by
hand, or start `ci.yml` on the PR and read it.

### The image

```bash
podman build -t hotloop-flow:check .
id=$(podman create hotloop-flow:check) && podman cp "$id:/usr/share/doc/hotloop-flow" check-doc && podman rm "$id"
for f in LICENSE LICENSES/Apache-2.0.txt NOTICE THIRD_PARTY_LICENSES; do test -s "check-doc/$f" && echo "$f ok"; done
```

The build fails on a GPL, AGPL or LGPL module or one with no identifiable
license, which is how a PR finds out. A release is not the first build.

### If you touched a workflow

```bash
actionlint .github/workflows/*.yml
```

A typo in `image-publish.yml` doesn't fail until a tag runs it.

## 5. The release PR

One PR, titled `X.Y.Z`, off fresh `origin/main`: the version text from section
2, the notes from section 3, and nothing else. Commits as
`patrickryan01 <patrick@fireballz.ai>`, no trailers.

```bash
git -c user.name=patrickryan01 -c user.email=patrick@fireballz.ai commit
gh pr create --base main --title "X.Y.Z" --body-file pr.md
```

The body says what's in the release, how the gates came out, and that the tag
goes on the squash commit after it merges.

This repository is public, so it lands by PR and merges only when every check is
green: Test, Chart, Static build, Image and its licenses, Fuzz smoke, Editor.

```bash
gh pr checks N
gh pr merge N --squash --delete-branch   # only when every line above says pass
```

When it's in:

```bash
git fetch origin
gh pr view N --json state,mergeCommit -q '.state, .mergeCommit.oid'
git log -1 --format='%H %an <%ae> %s' origin/main
```

`MERGED`, and the commit is `X.Y.Z (#N)` on `main`. If something merges to
`main` while the PR waits, rebase, read the log again (section 3) and put it in
the notes or leave it out on purpose.

## 6. Tag

An annotated tag, as patrickryan01, on the squash commit and nothing else, and
only after nothing else is running:

```bash
gh run list --limit 5                          # nothing in_progress or queued
SHA=$(gh pr view N --json mergeCommit -q .mergeCommit.oid)
git merge-base --is-ancestor "$SHA" origin/main && echo "on main"
git -c user.name=patrickryan01 -c user.email=patrick@fireballz.ai tag -a vX.Y.Z "$SHA" -m "HotLoop Flow X.Y.Z: one line of what it is"
git cat-file -p vX.Y.Z | head -5               # object is $SHA, tagger is patrickryan01
git push origin vX.Y.Z
```

The merge to `main` starts `ci.yml`. Let it finish before you push the tag:
the tag starts two more workflows, and one Actions run at a time is the rule.

### Never re-point a published tag

Once any part of a release is public, its number is spent. The image goes out as
`X.Y.Z`, `X.Y` and `X`, somebody pulls it, and a moved tag means two different
binaries answer to the same version. Fix it in a PR and release the next number.

If nothing was published, the number is still clean. Here's how to tell:

```bash
gh run view RUN --json jobs -q '.jobs[] | [.name, .conclusion] | @tsv'
gh release view vX.Y.Z                                          # must fail: release not found
curl -s https://hotloop.io/hotloop-flow/index.yaml | grep -c 'version: X.Y.Z'   # must print 0
T=$(curl -s "https://ghcr.io/token?scope=repository:hotloop-io/hotloop-flow:pull" | python -c 'import sys,json; print(json.load(sys.stdin)["token"])')
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $T" \
  -H 'Accept: application/vnd.oci.image.index.v1+json' \
  https://ghcr.io/v2/hotloop-io/hotloop-flow/manifests/X.Y.Z        # 404
```

Nothing was published only if `Build and push` (the image) and `Publish` (the
chart) both failed or never started, and all four checks agree. A failure after
either pushed means something may be out: treat it as published, check what is,
and take the next number. If nothing published:

```bash
git tag -d vX.Y.Z
git push origin :refs/tags/vX.Y.Z
# fix it with a PR, then tag its squash commit as above
```

## 7. Watch both workflows to the end

```bash
gh run list --branch vX.Y.Z --limit 5 --json databaseId,workflowName,status,conclusion,url
gh run watch RUN --exit-status         # once per run, one after the other
gh run view RUN --json conclusion,jobs -q '.conclusion, (.jobs[] | [.name, .conclusion, .startedAt, .completedAt] | @tsv)'
```

`Publish Image` has one job, `Build and push`. `Publish Helm Chart` has two,
`Lint` and then `Publish`. All green. Write both run URLs down; they go in the
release log below.

A green run proves the jobs ran. It doesn't prove anybody else can use what they
made. That's next.

## 8. Verify it publicly, then make the GitHub release

Everything here runs with no login, because that's what a plant gets.

### The image pulls anonymously

```bash
T=$(curl -s "https://ghcr.io/token?scope=repository:hotloop-io/hotloop-flow:pull" | python -c 'import sys,json; print(json.load(sys.stdin)["token"])')
curl -s -D - -o manifest.json -H "Authorization: Bearer $T" \
  -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json' \
  https://ghcr.io/v2/hotloop-io/hotloop-flow/manifests/X.Y.Z | grep -iE '^HTTP|docker-content-digest'
python -c 'import json; print(sorted(p["platform"]["architecture"] for p in json.load(open("manifest.json"))["manifests"] if p["platform"]["architecture"] != "unknown"))'
```

`200`, a digest, and `['amd64', 'arm64']`. A `401` means the package isn't public
(section 10). Then pull it for real with an empty auth file, so no stored login
can help, and ask it what it is:

```bash
echo '{}' > anon.json
podman pull --authfile anon.json ghcr.io/hotloop-io/hotloop-flow:X.Y.Z
podman run --rm ghcr.io/hotloop-io/hotloop-flow:X.Y.Z version
podman image inspect ghcr.io/hotloop-io/hotloop-flow:X.Y.Z --format '{{.Digest}}'
```

The image ships its licenses, because Apache, MIT, BSD and ISC all say the
notice goes wherever the binary goes:

```bash
id=$(podman create ghcr.io/hotloop-io/hotloop-flow:X.Y.Z)
podman cp "$id:/usr/share/doc/hotloop-flow" pulled-doc && podman rm "$id"
ls -l pulled-doc                       # LICENSE, LICENSES/, NOTICE, THIRD_PARTY_LICENSES, all non-empty
```

### The chart is in the index, and nothing else is

```bash
curl -s https://hotloop.io/hotloop-flow/index.yaml | grep -E '^    version:|\.tgz$'
curl -s -o /dev/null -w '%{http_code}\n' https://hotloop.io/hotloop-flow/hotloop-flow-X.Y.Z.tgz
```

One entry, at `X.Y.Z` and only `X.Y.Z`, its URL under
`https://hotloop.io/hotloop-flow/`, and a `200`. The index is rebuilt from this
version alone on purpose: earlier versions predate the App Store contract and
must not be resolvable. The Pages CDN caches `index.yaml` for up to ten minutes
(`Cache-Control: max-age=600`), so an old index right after the chart job is the
cache, not the release.

### Helm finds it

With a throwaway repository config, so your own `helm repo` list is neither used
nor touched:

```bash
export HELM_REPOSITORY_CONFIG=$(mktemp) HELM_REPOSITORY_CACHE=$(mktemp -d)
helm repo add hotloop-flow https://hotloop.io/hotloop-flow/
helm search repo hotloop-flow --versions
```

`hotloop-flow` at `X.Y.Z`, chart and app version both.

### The GitHub release

The workflows don't make one, so you do, from the notes in the release PR:

```bash
gh release create vX.Y.Z --title "HotLoop Flow X.Y.Z" --notes-file docs/releases/X.Y.Z.md --verify-tag
gh release view vX.Y.Z --json name,isDraft,isPrerelease,url -q '.name, .isDraft, .isPrerelease, .url'
gh release list --limit 1
```

`HotLoop Flow X.Y.Z`, not a draft, not a prerelease, marked Latest, with the
body you reviewed.

## 9. The site and the docs

Two more PRs, in two other repositories, merged on green the same way, because
they're public too. They go up after section 8 passes, never before: a page
that says a version is out while its image still 401s is a lie with a link in it.

- `HotLoop-io/HotLoop-io.github.io`: `src/content/releases/flow/X.Y.Z.md` (front
  matter `product`, `version`, `date`, `published`, `summary`, `highlights`,
  `breaking`, then the body), and the version shown on `src/pages/flow.astro`
  (`softwareVersion` and the "is out" notice).
- `HotLoop-io/docs`: **sync docs.hotloop.io.** Its `scripts/sync-docs.mjs` reads
  this repo's `origin/main` and the release you just made from a local clone,
  rewrites every Flow page from the README and `docs/`, marks whatever is newer
  than the release as next release, and fills the new version into the install
  pages. It stops on a hand-written page last read against an older version, so
  read that page against the release and bump its `reviewed:` line. The docs
  repo's README has the command. `npm run build`, PR, merge on green, then curl
  a Flow page on https://docs.hotloop.io.

```bash
git grep -n -F 'PREV' -- src README.md        # in each repo; every hit is read
curl -s -o /dev/null -w '%{http_code}\n' https://hotloop.io/flow/
curl -s https://hotloop.io/flow/ | grep -c 'X.Y.Z'
curl -s -o /dev/null -w '%{http_code}\n' https://hotloop.io/releases/flow/
curl -s https://hotloop.io/releases/flow/ | grep -c 'id="flow-X.Y.Z"'
```

Live pages answer `200` and say `X.Y.Z`. The release notes are one page with an
anchor per version, so there is no `/releases/flow/X.Y.Z/` (it is a 404). A URL
fragment never reaches the server, so the check is the `200` on the page plus
the `id="flow-X.Y.Z"` grep, which must print `1`. Pages deploys take a few minutes after
the merge.

## 10. The package stays public

```bash
gh api orgs/HotLoop-io/packages/container/hotloop-flow --jq '.name + " " + .visibility'
```

`public`. A private package lets the release go green and answers `401` to every
plant that pulls it. The anonymous pull in section 8 is the proof; this is the
explanation.

## 11. What not to do

- **Don't tag anything but the squash commit on `main`,** and don't tag before the
  PR merged.
- **Don't move a tag that published anything.** Section 6.
- **Don't bump the chart's placeholders.** Section 2.
- **Don't build or push an image by hand** under a release tag. The workflow
  builds both architectures from the tagged tree; a hand push overwrites that
  with something no run can account for.
- **Don't hand-edit gh-pages or `index.yaml`.** The chart job rebuilds the index
  from the one version it just packaged.
- **Don't tick ROADMAP boxes in a release.** They're Patrick's.
- **Don't ship an unproven feature as done.** If it only ran in the lab, the
  notes say so, and say what's left.
- **Don't prune, or bulk remove, containers or images on a shared box.** Remove
  the ones you made, by name.

