# cleat-worker container images

The worker image is published to `ghcr.io/cleat-team/cleat-worker` from two
places. They differ in what they promise.

| tag | published by | when | promise |
|---|---|---|---|
| `vX.Y.Z` | `release.yml` | a release tag | a release: migrations, upgrade notes and compatibility as documented for that version |
| `X.Y` | `release.yml` | a release tag | moves to the newest `vX.Y.Z` patch |
| `latest` | `release.yml` | a release tag | moves to the newest release |
| `develop-<9-hex sha>` | `develop-image.yml` | every push to `develop` | **none** — see below |
| `develop-<40-hex sha>` | `develop-image.yml` | every push to `develop` | the same image as the 9-hex tag |
| `develop` | `develop-image.yml` | every push to `develop` | **none**, and it moves |

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

## When a develop tag is missing

`develop-image.yml` runs once per push to `develop`. If a run failed, or a
commit landed before the workflow existed, that commit has no image. A
maintainer can publish the current `develop` head by dispatching the workflow
on `develop` (`gh workflow run develop-image.yml --ref develop`). A dispatch
on any other ref is refused before anything is pushed.
