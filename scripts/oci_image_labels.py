#!/usr/bin/env python3
"""The OCI labels every published cleat-worker image carries, and the check that
a built image really carries them.

One definition, used by every workflow that builds the image:

  * release.yml (`Publish the cleat-worker image`) -- a v* tag
  * develop-image.yml                                -- develop and release/**
  * ci.yml (`Release Dry Run`)                       -- every PR, asserts them

cleat#3487: v0.4.0 and v0.5.0 were published with NO labels (`null` on both
platforms), while the rc-* images carried revision, source and ref.name. A
published release image therefore did not say what commit it was built from,
and clew's slice check (revision label == pinned commit) could not run against
it. The two paths each wrote their own `labels:` block; the release one simply
never got one. Sharing the definition is the repair; `check` is what keeps it
from being quietly dropped again, because a step that sets no labels does not
fail -- it produces `null`.

Subcommands:

  emit   --repo OWNER/NAME --ref REF --sha SHA [--version V]
         Prints a `labels<<EOF ... EOF` block for $GITHUB_OUTPUT, to be passed
         to docker/build-push-action's `labels:`.
  check  --repo OWNER/NAME --ref REF --sha SHA [--version V]
         (--image IMAGE | --labels-json JSON)
         Fails unless the image carries exactly the labels `emit` would set.
  --self-test

Exit status: 0 passed, 1 a finding (labels missing or wrong), 2 could not
establish what was being measured (bad input, docker absent, image not
inspectable). 2 is a failure of the check and not a finding about the image.
"""
import argparse
import io
import json
import os
import re
import subprocess
import sys

SHA_RE = re.compile(r"^[0-9a-f]{40}$")
REPO_RE = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$")
PREFIX = "org.opencontainers.image."

# The keys that must be present whatever the caller passes. version is added
# when a version is given (a release), so a develop/rc image is not required to
# carry one it has never had.
ALWAYS_REQUIRED = ("source", "revision", "ref.name")


class Unmeasured(Exception):
    """The check could not establish what it was asked about."""


def labels(repo, ref, sha, version=None):
    if not REPO_RE.match(repo or ""):
        raise Unmeasured("--repo %r is not OWNER/NAME" % (repo,))
    if not ref or "\n" in ref:
        raise Unmeasured("--ref %r is empty or multi-line" % (ref,))
    if not SHA_RE.match(sha or ""):
        # A truncated or empty sha would be recorded as the revision and look
        # exactly like a real one to everything downstream.
        raise Unmeasured("--sha %r is not a full 40-character lowercase sha" % (sha,))
    if version is not None and (not version or "\n" in version):
        raise Unmeasured("--version %r is empty or multi-line" % (version,))
    out = [
        (PREFIX + "source", "https://github.com/" + repo),
        (PREFIX + "revision", sha),
        (PREFIX + "ref.name", ref),
    ]
    if version is not None:
        out.append((PREFIX + "version", version))
    return out


def emit_block(pairs):
    return "labels<<CLEAT_LABELS_EOF\n" + "".join("%s=%s\n" % kv for kv in pairs) + "CLEAT_LABELS_EOF\n"


def inspect_labels(image, docker):
    try:
        res = subprocess.run(
            [docker, "inspect", "--format", "{{json .Config.Labels}}", image],
            capture_output=True, text=True, timeout=120)
    except (OSError, subprocess.SubprocessError) as e:
        raise Unmeasured("could not run `%s inspect %s`: %s" % (docker, image, e))
    if res.returncode != 0:
        raise Unmeasured("`%s inspect %s` failed: %s" % (docker, image, res.stderr.strip()))
    return res.stdout


def findings(want, got_json):
    try:
        got = json.loads(got_json)
    except ValueError as e:
        raise Unmeasured("the image's labels are not JSON (%s): %r" % (e, got_json[:80]))
    if got is None:
        got = {}  # `null`: the image carries no labels at all
    if not isinstance(got, dict):
        raise Unmeasured("the image's labels decoded to %s, not an object" % type(got).__name__)
    required = {PREFIX + k for k in ALWAYS_REQUIRED}
    required |= {k for k, _ in want if k.endswith(".version")}
    if not required <= {k for k, _ in want}:
        # `emit` and `check` would agree on an empty label set. Refuse that.
        raise Unmeasured("the expected label set is missing %s" % sorted(required - {k for k, _ in want}))
    bad = []
    for k, v in want:
        if k not in got:
            bad.append("%s is missing" % k)
        elif got[k] != v:
            bad.append("%s is %r, want %r" % (k, got[k], v))
    return bad


def run(argv, out, err):
    ap = argparse.ArgumentParser(prog="oci_image_labels.py")
    sub = ap.add_subparsers(dest="cmd")
    for name in ("emit", "check"):
        p = sub.add_parser(name)
        p.add_argument("--repo", required=True)
        p.add_argument("--ref", required=True)
        p.add_argument("--sha", required=True)
        p.add_argument("--version")
        if name == "check":
            g = p.add_mutually_exclusive_group(required=True)
            g.add_argument("--image")
            g.add_argument("--labels-json")
            p.add_argument("--docker", default=os.environ.get("DOCKER", "docker"))
    try:
        a = ap.parse_args(argv)
    except SystemExit as e:
        return 2 if e.code else 0
    if a.cmd is None:
        err.write("usage: oci_image_labels.py emit|check ... | --self-test\n")
        return 2
    try:
        want = labels(a.repo, a.ref, a.sha, a.version)
        if a.cmd == "emit":
            out.write(emit_block(want))
            return 0
        got = a.labels_json if a.labels_json is not None else inspect_labels(a.image, a.docker)
        bad = findings(want, got)
    except Unmeasured as e:
        err.write("UNMEASURED: %s\n  this is a failure of the check, not a finding about the image\n" % e)
        return 2
    if bad:
        err.write("image labels do not match what the release path sets (cleat#3487):\n")
        for b in bad:
            err.write("  - %s\n" % b)
        return 1
    out.write("ok: %d labels present and correct\n" % len(want))
    return 0


def self_test():
    sha = "a" * 40
    repo = "cleat-team/cleat"
    ok_json = json.dumps(dict(labels(repo, "v1.2.3", sha, "1.2.3")))
    failures = []

    def case(label, argv, want_rc, want_text=None):
        out, err = io.StringIO(), io.StringIO()
        rc = run(argv, out, err)
        text = out.getvalue() + err.getvalue()
        why = None
        if rc != want_rc:
            why = "exit %d, want %d" % (rc, want_rc)
        elif want_text is not None and want_text not in text:
            # Assert on the TEXT as well as the status: a second, correct
            # mechanism can supply the expected status (CLAUDE.md, "A condition
            # that never decides anything").
            why = "exit was right but the output lacks %r: %r" % (want_text, text)
        print("  %-62s %s" % (label, "ok" if why is None else "FAIL: " + why))
        if why:
            failures.append(label)

    base = ["check", "--repo", repo, "--ref", "v1.2.3", "--sha", sha]
    case("all labels present and right", base + ["--version", "1.2.3", "--labels-json", ok_json], 0, "4 labels")
    # KNOWN-POSITIVES: the shape v0.5.0 actually had, and each way to be wrong.
    case("KNOWN-POSITIVE: `null` (what v0.5.0 had)", base + ["--version", "1.2.3", "--labels-json", "null"], 1,
         "org.opencontainers.image.revision is missing")
    case("KNOWN-POSITIVE: empty object", base + ["--labels-json", "{}"], 1, "org.opencontainers.image.source is missing")
    wrong = dict(json.loads(ok_json))
    wrong[PREFIX + "revision"] = "b" * 40
    case("wrong revision", base + ["--version", "1.2.3", "--labels-json", json.dumps(wrong)], 1, "revision is")
    nover = dict(json.loads(ok_json))
    del nover[PREFIX + "version"]
    case("version expected but absent", base + ["--version", "1.2.3", "--labels-json", json.dumps(nover)], 1,
         "version is missing")
    case("no version expected, none required", base + ["--labels-json", json.dumps(nover)], 0)
    # UNMEASURED is its own status, never 0 and never 1.
    case("short sha is UNMEASURED, not a pass", ["check", "--repo", repo, "--ref", "r", "--sha", "abc123",
                                                 "--labels-json", ok_json], 2, "not a full 40-character")
    case("labels that are not JSON are UNMEASURED", base + ["--labels-json", "not json"], 2, "not JSON")
    case("docker absent is UNMEASURED", base + ["--image", "x", "--docker", "/nonexistent/docker"], 2,
         "could not run")
    case("bad repo is UNMEASURED", ["check", "--repo", "nope", "--ref", "r", "--sha", sha, "--labels-json", "{}"],
         2, "OWNER/NAME")
    case("emit with a bad sha refuses", ["emit", "--repo", repo, "--ref", "r", "--sha", "HEAD"], 2, "40-character")
    case("emit prints the block", ["emit", "--repo", repo, "--ref", "v1.2.3", "--sha", sha, "--version", "1.2.3"], 0,
         "org.opencontainers.image.revision=" + sha)
    out, err = io.StringIO(), io.StringIO()
    run(["emit", "--repo", repo, "--ref", "develop", "--sha", sha], out, err)
    if PREFIX + "version" in out.getvalue():
        print("  emit without --version must not invent a version label: FAIL")
        failures.append("emit invented a version")
    if failures:
        print("self-test: %d failure(s): %s" % (len(failures), "; ".join(failures)))
        return 1
    print("self-test: ok")
    return 0


def main():
    if sys.argv[1:] == ["--self-test"]:
        return self_test()
    return run(sys.argv[1:], sys.stdout, sys.stderr)


if __name__ == "__main__":
    sys.exit(main())
