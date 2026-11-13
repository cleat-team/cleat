#!/bin/bash
# Phase 0 verification: everything PHASE0.md claims, with its raw output. Needs: PG (OWNER_DSN), a stock cleat-worker
# running against it (WORKER_API + KEY for the default tenant), and .work/{hello,hello2}.wasm built by build.sh.
# usage: OWNER_DSN=... WORKER_API=http://127.0.0.1:18771 KEY=... ./phase0-verify.sh
set -uo pipefail
cd "$(dirname "$0")"
W=.work
: "${OWNER_DSN:?}"; : "${WORKER_API:?}"; : "${KEY:?}"
poll() { id=$1; for i in $(seq 1 40); do st=$(curl -fsS -m 5 "$WORKER_API/api/workflows/$id" -H "Authorization: Bearer $KEY"); s=$(echo "$st" | python3 -c "import json,sys;print(json.load(sys.stdin)['status'])"); [ "$s" = done -o "$s" = failed ] && break; sleep 1; done; echo "$st" | python3 -c "import json,sys;d=json.load(sys.stdin);print(json.dumps({k:d.get(k) for k in ('def_name','status','result','error','reclaim_count')}))"; }
start() { curl -fsS -m 10 -X POST "$WORKER_API/api/workflows/$1/start" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"input":{}}' | python3 -c "import json,sys;print(json.load(sys.stdin)['id'])"; }
deploy() { CLEAT_DB_URL="$OWNER_DSN" $W/cleatctl deploy workflow "$1" "$2" 2>&1 | tail -1; }

echo "### versions"; .tools/javy --version; npx esbuild --version; rustc --version; wasm-tools --version; (cd ../.. && git log -1 --format='cleat base %h' origin/develop)
echo; echo "### 1. WASI import census of the deployed module against engine/wasi_policy.go"
python3 - <<'PY'
import re,subprocess
pol=open('../../engine/wasi_policy.go').read()
bucket={m.group(1):m.group(2) for m in re.finditer(r'^\s*"([a-z_0-9]+)":\s*\{(wasi[A-Za-z]+)',pol,re.M)}
out=subprocess.run(['wasm-tools','print','.work/hello.wasm'],capture_output=True,text=True).stdout
imps=re.findall(r'\(import "([^"]+)" "([^"]+)"',out)
for mod,name in imps:
    print(f"  {mod}::{name:<20} {bucket.get(name,'NOT IN POLICY (fatal)') if mod=='wasi_snapshot_preview1' else 'NON-WASI IMPORT'}")
print("  imports from modules other than wasi_snapshot_preview1:", sum(1 for m,_ in imps if m!='wasi_snapshot_preview1'))
PY
echo; echo "### 2. exports of the deployed module (the ABI's exports are entry points)"; wasm-tools print $W/hello.wasm | grep -E '^\s*\(export ' | sed 's/^ *//'
echo; echo "### 3. deploy hello with cleatctl, run it on the stock worker"
deploy hello $W/hello.wasm; id=$(start hello); echo "run $id"; poll "$id"
echo; echo "### 4. green control: a different result in the source comes back different"
deploy hello2 $W/hello2.wasm; id=$(start hello2); poll "$id"
echo; echo "### 5. red control: the same Javy module WITHOUT the trampoline (cleat-js-link skipped)"
cp $W/hello.javy.wasm $W/hello_raw.wasm
(cd ../.. && go run ./prototypes/ts-javy/tools/tsmeta -in prototypes/ts-javy/$W/hello_raw.wasm -out prototypes/ts-javy/$W/hello_raw.stamped.wasm -name hello_raw -entries hello) && deploy hello_raw $W/hello_raw.stamped.wasm; id=$(start hello_raw); poll "$id"
echo; echo "### 6. dynamic mode (-C dynamic) on the stock engine"
(cd ../.. && go run ./prototypes/ts-javy/tools/tsmeta -in prototypes/ts-javy/$W/hello-dynamic.wasm -out prototypes/ts-javy/$W/hello_dyn.wasm -name hello_dyn -entries run) && deploy hello_dyn $W/hello_dyn.wasm; id=$(start hello_dyn); poll "$id"
echo; echo "### 7. nothing outside prototypes/ differs from origin/develop (empty = none)"
(cd ../.. && git diff origin/develop --stat -- . ':!prototypes'; git status --short | grep -v '^?? prototypes/')
echo "(end)"
