# cleat-worker container images

The worker image is published to `ghcr.io/cleat-team/cleat-worker` from three
places. They differ in what they promise. **`develop-*` and `rc-*` tags carry no
compatibility promise; `vX.Y.Z` tags are releases.**

| tag | published by | when | promise |
|---|---|---|---|
| `vX.Y.Z` | `release.yml` | a release tag | a release: migrations, upgrade notes and compatibility as documented for that version |
| `X.Y` | `release.yml` | a release tag | moves to the newest `vX.Y.Z` patch |
| `latest` | `release.yml` | a release tag | moves to the newest release |
| `develop-<9-hex sha>` | `develop-image.yml` | every push to `develop` | **none** — see below |
| `develop-<40-hex sha>` | `develop-image.yml` | every push to `develop` | the same image as the 9-hex tag |
| `develop` | `develop-image.yml` | every push to `develop` | **none**, and it moves |
| `rc-<9-hex sha>` | `develop-image.yml` | every push to a `release/**` branch | **none** — a release candidate, see below |
| `rc-<40-hex sha>` | `develop-image.yml` | every push to a `release/**` branch | the same image as the 9-hex tag |

All of them are `linux/amd64` and `linux/arm64`, built from the same
`Dockerfile`. That Dockerfile runs `cleat-worker --verify-backend` for each
architecture during the build, so no tag ships without the wasmtime backend.
Pulling needs no authentication.

## Develop tags carry no compatibility promise

A `develop-*` image is whatever was on `develop` at that commit. Specifically:

- **No migration path between develop images.** `develop` re-baselines its
  migrations from time to time. A database created by one develop image may
  not migrate under the next one. Plan for a fresh database whenever you move
  your pin.
- **No upgrade path to the next release.** Upgrades are tested from the
  previous *release*, not from an arbitrary develop commit.
- **No API or ABI stability.** Breaking changes land on `develop` before they
  are released.

## Release candidates: `rc-*`

A push to a `release/**` branch publishes the head of that branch as
`rc-<9-hex sha>` and `rc-<40-hex sha>`, so the things that validate a release
before its tag (the upgrade test, a consumer's re-run) can pull the candidate
instead of building the worker with CGO from the branch.

- **No moving `rc` tag.** There is no `rc` or `rc-latest`. A release branch is
  re-pushed, and a tag that means "the newest candidate" is the one that gets
  pinned by accident. Pin the sha.
- **Only a protected `release/**` branch publishes.** The job asks the API
  whether the branch is protected or covered by a ruleset and refuses if it is
  neither, or if it cannot tell. "Protected" is the floor, not a review
  guarantee: a rule that only forbids force-push satisfies it. Requiring a pull
  request on `release/**` is a repository ruleset, and it is the owner's to add.
  The check also lives in the workflow file of the pushed ref, so it stops a
  mistake, not a writer who edits it away; that control is the ruleset plus the
  organisation's Actions and package settings.
- **Only a commit that landed the sanctioned way publishes.** The `release/**`
  ruleset has no `creation` rule, so a writer can create `release/x` at any
  commit and the protection check above would pass. A second job therefore asks
  about the commit the tag names. It must be an ancestor of `develop` (a release
  branch is cut at a `develop` commit), or the merge commit of a merged pull
  request whose base is that release branch **and whose first parent has landed
  too**. The walk matters: a fix merged onto a branch that was created at an
  unlanded commit carries that commit with it, and without following first
  parents it would be accepted. Anything else is refused, and the message names
  the commit where the chain broke. Exit status `2` means the check could not
  read something, or the walk ran out of its bound; it says nothing about the
  commit and stops the job all the same. `scripts/check-rc-sha-landed.py` is the
  judge. **"Landed" is a claim about the path, not about a person reading the
  change:** the ruleset requires zero approving reviews, so a merged pull request
  proves it went through the required status checks, not that anyone approved it.
  **This prevents an accidental rc image from an unlanded commit. It is not an
  authorization control:** repository write access already implies the ability
  to publish packages, since any branch a writer pushes can carry a workflow that
  grants itself `packages: write` and pushes to `ghcr.io/cleat-team/cleat-worker`
  under any tag. Trust release tags (`vX.Y.Z`), not `rc-*`, for anything that
  matters.
- **An `rc-<40-hex sha>` tag is written once.** A re-run for a sha that already
  has the tag leaves the existing image alone, so the tag keeps pointing at the
  digest it was first given. To rebuild one on purpose, delete the tag first.
  (`develop-*` tags are re-published by a dispatch, as described below.)
- **An `rc-*` image is not a release.** It has the same lack of promise as a
  `develop-*` image. The release is the `vX.Y.Z` tag that `release.yml`
  publishes after the branch is merged to `main`.
- A release branch is cut at `develop`'s head, so the first `rc-<sha>` is the
  same commit as a `develop-<sha>` image that already exists. They are separate
  builds of one commit.

## Pin the sha, not `develop`

To run a specific develop commit, pin its sha tag:

```bash
docker pull ghcr.io/cleat-team/cleat-worker:develop-34fec6ef7
```

Use the commit you pinned in `go.mod` as well. A Go pseudo-version ends in the
first 12 hex characters of the commit, and the first 9 of those are the image
tag. That keeps the SDK and the worker on the same commit.

The moving `develop` tag is a convenience for trying things out. It is moved
only when its build finishes while that commit is still `develop`'s head, but
two merges seconds apart can still race. Never put it in a deployment or a CI
pin.

The image's `org.opencontainers.image.revision` label carries the full commit
sha:

```bash
docker inspect --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}' \
  ghcr.io/cleat-team/cleat-worker:develop-34fec6ef7
```

A release image (`cleat-worker:vX.Y.Z`) carries the same labels, plus
`org.opencontainers.image.version`, from the first release published after
cleat#3487. **v0.4.0 and v0.5.0 carry none** (`null` on both platforms): the
release job set no `labels:` while the rc path did. Their tags are not
re-published, since a released tag's digest should not change. For those two, the
provenance is the digest the tag's own Release run exported (the `Publish the
cleat-worker image` job's log), not a label. All three workflows that push the
image take their labels from `scripts/oci_image_labels.py`, and `Release Dry Run`
fails if a built image does not carry them.

## When a develop or rc tag is missing

`develop-image.yml` runs once per push to `develop` and to a `release/**`
branch. If a run failed, or a commit landed before the workflow existed, that
commit has no image. A maintainer can publish the current head by dispatching
the workflow on that branch (`gh workflow run develop-image.yml --ref develop`,
or `--ref release/v0.5.0`). A dispatch on any other ref is refused before
anything is pushed, and the tag prefix is chosen from the ref alone.

A failed or timed-out push run opens, or comments on, a tracking issue labelled
`area/ci` and titled `[develop-image] image publish failed on <branch>`
(`develop-image-failure-notifier.yml`, cleat#3374). It names the commit, the
sha tags (short and full) that may be missing, and the jobs and steps that failed;
a failure in the step that moves `:develop` is reported as that, because the sha tags
are already pushed by then. A failed
`workflow_dispatch` run is not reported: whoever dispatched it is reading it.

To exercise the landed-sha gate without publishing, dispatch on `develop` with
`dry_run` set and name the branch and commit to judge:

```
gh workflow run develop-image.yml --ref develop -f dry_run=true \
  -f check_branch=release/v0.5.0 -f check_sha=<40-hex sha>
```

Only the `landed` job runs; `publish` is skipped, so nothing is built or
pushed.
