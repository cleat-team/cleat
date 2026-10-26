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

## When a develop or rc tag is missing

`develop-image.yml` runs once per push to `develop` and to a `release/**`
branch. If a run failed, or a commit landed before the workflow existed, that
commit has no image. A maintainer can publish the current head by dispatching
the workflow on that branch (`gh workflow run develop-image.yml --ref develop`,
or `--ref release/v0.5.0`). A dispatch on any other ref is refused before
anything is pushed, and the tag prefix is chosen from the ref alone.
