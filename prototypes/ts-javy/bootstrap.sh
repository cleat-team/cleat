#!/usr/bin/env bash
# Build EVERY input the verification uses, from the COMMITTED tree, and record where each came from.
#
# Why this exists: checkpoint-1 review found that phase2-verify.sh did not run from a fresh checkout. The worker, cleatctl, the
# linker, the API key and javy were all untracked and built by no script, and the worker the committed evidence came from was built
# at bd775a64 with vcs.modified=true -- a commit whose engine.go does not even contain the "javascript" label (it was uncommitted
# then). This script is the fix: after it, nothing the verification runs is a stray binary.
#
#   usage: [CLEAT_PROTO_ALLOW_DIRTY=1] [CLEAT_PROTO_* see phase1_lib.py] ./bootstrap.sh
#   exit:  0 built and recorded; 2 UNMEASURED (a tool is missing, a download or checksum failed, the tree is dirty)
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
HERE=$PWD; ROOT=$(cd ../.. && pwd); W=.work
mkdir -p "$W" .tools
unmeasured() { echo "UNMEASURED: $*" >&2; echo "  (this is a failure of the setup, not a finding about the prototype)" >&2; exit 2; }

echo "### tools"
missing=()
for t in go cargo rustc node npm docker python3 curl shasum git; do command -v "$t" >/dev/null 2>&1 || missing+=("$t"); done
if command -v rustup >/dev/null 2>&1; then
  inst=$(rustup target list --installed)
  grep -q '^wasm32-wasip1$' <<<"$inst" || missing+=("rust target wasm32-wasip1 (rustup target add wasm32-wasip1)")
  grep -q '^wasm32-unknown-unknown$' <<<"$inst" || missing+=("rust target wasm32-unknown-unknown, for the Rust twin in the Phase 3 harness (rustup target add wasm32-unknown-unknown)")
fi
[ ${#missing[@]} -eq 0 ] || unmeasured "missing: ${missing[*]}"
echo "  go: $(go version | cut -d' ' -f3)  rustc: $(rustc --version | cut -d' ' -f2)  node: $(node --version)  docker: $(docker --version | cut -d' ' -f3 | tr -d ,)"

echo "### the tree these binaries will be built from"
HEAD_SHA=$(git -C "$ROOT" rev-parse HEAD)
# evidence/ is OUTPUT of the verification (phase2-verify.sh opens its files before this script runs), not an input to any binary
DIRTY=$(git -C "$ROOT" status --porcelain -- . ':!prototypes/ts-javy/evidence' | wc -l | tr -d ' ')
echo "  HEAD $HEAD_SHA, $DIRTY changed or untracked path(s)"
if [ "$DIRTY" != 0 ] && [ -z "${CLEAT_PROTO_ALLOW_DIRTY:-}" ]; then
  git -C "$ROOT" status --porcelain -- . ':!prototypes/ts-javy/evidence' | head -8 | sed 's/^/    /'
  unmeasured "the tree is dirty, so a binary built from it cannot be tied to a commit. Commit, or set CLEAT_PROTO_ALLOW_DIRTY=1 for development (the provenance file then says DIRTY and the evidence is not reproducible)."
fi
grep -q '"javascript"' "$ROOT/engine/engine.go" || unmeasured "engine/engine.go at HEAD has no \"javascript\" language label: the worker would refuse these modules"

echo "### javy (pinned in toolchain.lock)"
case "$(uname -s)-$(uname -m)" in
  Darwin-arm64) asset=javy-arm-macos-v9.1.0.gz
    want=$(awk '/^javy.arm-macos/{print $3}' toolchain.lock) ;;
  *) unmeasured "no pinned javy checksum for $(uname -s)-$(uname -m); only Darwin-arm64 is pinned in toolchain.lock" ;;
esac
if [ ! -x .tools/javy ] || [ "$(.tools/javy --version 2>/dev/null)" != "javy 9.1.0" ]; then
  curl -fsSL -o ".tools/$asset" "https://github.com/bytecodealliance/javy/releases/download/v9.1.0/$asset" || unmeasured "could not download $asset"
  got=$(shasum -a 256 ".tools/$asset" | cut -d' ' -f1)
  [ "$got" = "$want" ] || unmeasured "javy checksum mismatch: got $got, toolchain.lock pins $want"
  gunzip -c ".tools/$asset" > .tools/javy; chmod +x .tools/javy
fi
[ "$(.tools/javy --version)" = "javy 9.1.0" ] || unmeasured "javy --version is $(.tools/javy --version), expected javy 9.1.0"
echo "  $(.tools/javy --version)  sha256(binary) $(shasum -a 256 .tools/javy | cut -c1-16)"

echo "### esbuild (package-lock.json, exact)"
npm ci --no-audit --no-fund >/dev/null 2>&1 || unmeasured "npm ci failed"
[ "$(npx esbuild --version)" = "0.28.2" ] || unmeasured "esbuild is $(npx esbuild --version), package.json pins 0.28.2"
echo "  esbuild $(npx esbuild --version)"

echo "### the worker and cleatctl, from this tree's own engine (CGO on: wasmtime is the only backend)"
( cd "$ROOT" && env -u GOWORK -u GOFLAGS -u GOPROXY go build -o "$HERE/$W/cleat-worker" ./cmd/cleat-worker \
  && env -u GOWORK -u GOFLAGS -u GOPROXY go build -o "$HERE/$W/cleatctl" ./cmd/cleatctl ) || unmeasured "building the worker or cleatctl failed"

echo "### the linker"
cargo build --release --manifest-path link/Cargo.toml >/dev/null 2>&1 || unmeasured "building link/ (cleat-js-link) failed"
[ -x link/target/release/cleat-js-link ] || unmeasured "link/target/release/cleat-js-link was not produced"

echo "### PostgreSQL, the cleat_app role, a database, migrations and the API key"
read -r CONT PGPORT DBNAME < <(python3 -c 'import phase1_lib as L; print(L.PG, L.PG_PORT, L.PG_DB)')
if ! docker inspect "$CONT" >/dev/null 2>&1; then
  echo "  creating container $CONT (postgres:16) on port $PGPORT"
  docker run -d --name "$CONT" -p "$PGPORT:5432" -e POSTGRES_PASSWORD=postgres postgres:16 >/dev/null || unmeasured "could not start $CONT"
fi
docker start "$CONT" >/dev/null 2>&1 || true
for _ in $(seq 1 60); do docker exec "$CONT" pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done
docker exec "$CONT" pg_isready -U postgres >/dev/null 2>&1 || unmeasured "$CONT never became ready"
docker exec "$CONT" psql -U postgres -At -c "select 1 from pg_roles where rolname='cleat_app'" | grep -q 1 \
  || docker exec "$CONT" psql -U postgres -At -c "CREATE ROLE cleat_app LOGIN PASSWORD 'cleat-ts-proto' NOSUPERUSER NOBYPASSRLS" >/dev/null
./provision-db.sh

echo "### provenance"
python3 - "$HEAD_SHA" "$DIRTY" <<'PY'
import hashlib, re, subprocess, sys, datetime
head, dirty = sys.argv[1], int(sys.argv[2])
sha = lambda p: hashlib.sha256(open(p, "rb").read()).hexdigest()
def vcs(p):
    out = subprocess.run(["go", "version", "-m", p], capture_output=True, text=True).stdout
    g = lambda k: (re.search(rf"vcs\.{k}=(\S+)", out) or [None, "?"])[1]
    return g("revision"), g("modified"), g("time")
lines = [f"cleat-ts-proto provenance, written {datetime.datetime.now(datetime.timezone.utc).isoformat(timespec='seconds')}",
         f"source tree: HEAD {head}; {'DIRTY (' + str(dirty) + ' paths): NOT reproducible from a commit' if dirty else 'clean'}"]
ok = dirty == 0
for p in (".work/cleat-worker", ".work/cleatctl"):
    rev, mod, t = vcs(p)
    same = rev == head and mod == "false"
    ok &= same
    lines.append(f"{p}: sha256 {sha(p)[:16]}  built from {rev[:12]} modified={mod} at {t}  -> {'matches HEAD, clean' if same else 'DOES NOT MATCH a clean HEAD'}")
lines.append(f"link/target/release/cleat-js-link: sha256 {sha('link/target/release/cleat-js-link')[:16]} (cargo, link/Cargo.lock)")
lines.append(f".tools/javy: sha256 {sha('.tools/javy')[:16]}  {subprocess.check_output(['.tools/javy','--version'],text=True).strip()}")
lines.append("toolchain: " + "; ".join(subprocess.check_output(c, text=True).strip().split("\n")[0] for c in (["go", "version"], ["rustc", "--version"], ["node", "--version"], ["npx", "esbuild", "--version"])))
lines.append("RESULT: " + ("every binary was built from the committed HEAD of a clean tree" if ok else "NOT FULLY REPRODUCIBLE FROM A COMMIT (see above)"))
open(".work/provenance.txt", "w").write("\n".join(lines) + "\n")
print("\n".join("  " + l for l in lines))
PY
echo "bootstrap complete"
