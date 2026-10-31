#!/usr/bin/env bash
#
# Runs the REAL scripts/build-documented-examples.sh, with a stub in place of the
# `cleat` binary, and asserts what it prints when an example's build fails.
# cleat#3592.
#
# The script used to print `tail -8` of a failed build. For Gradle that keeps the
# epilogue (`> Run with --info`, `BUILD FAILED`) and drops the cause, which sits
# above it: the compiler error and `* What went wrong:`. On a CI run the cause was
# not recoverable at all.
#
# THE KNOWN-POSITIVE is a real log, not an invented one: captured 2026-10-31 from
# `cleat build --target java ./examples/java-workflow/` with one line of
# PlaceOrder.java broken (`int broken = "not an int";`), JDK 17, Gradle 7.6.4. Its
# last eight lines are the ones the issue quotes. A precondition below asserts that
# a tail of eight lines really does hide its cause, so this test cannot pass
# vacuously against a fixture that hid nothing.
#
# Cases:
#   1. that real log: the compiler error and `What went wrong` are printed;
#   2. a long log with the cause well above its last eight lines but inside the
#      cap: printed;
#   3. a log past the cap: the output SAYS lines were omitted and how many, and the
#      cause at line 1 is not printed -- the limit is stated, not hidden;
#   4. every build succeeds: exit 0 and no FAIL, so the stub is not simply
#      failing everything.
#
# BDE_SCRIPT runs a different copy of the script, which is how a falsification is
# done: run this against the version before the change and it must go red.
#
# Exit 0 all cases held, 1 a case did not, 2 UNMEASURED (no repository, or the
# script under test is not there).
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${BDE_SCRIPT:-$ROOT/scripts/build-documented-examples.sh}"
if [[ ! -f "$SCRIPT" ]]; then
  echo "UNMEASURED: $SCRIPT does not exist." >&2
  exit 2
fi
cd "$ROOT" || exit 2
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

cat >"$WORK/gradle-failure.log" <<'EOF'
Vetting Java project in /work/examples/java-workflow...

  Summary: 1 files, 0 errors, 0 warnings
  Compiling Java to WASM via TeaVM...
Note: Some input files use unchecked or unsafe operations.
Note: Recompile with -Xlint:unchecked for details.
warning: File for type 'cleat.generated.CleatDeferRunner' created in the last round will not be subject to annotation processing.
warning: File for type 'cleat.generated.CleatEntryIndex' created in the last round will not be subject to annotation processing.
warning: File for type 'cleat.WorkflowEntry' created in the last round will not be subject to annotation processing.
/work/examples/java-workflow/src/main/java/com/cleat/example/PlaceOrder.java:118: error: incompatible types: String cannot be converted to int
    int broken = "not an int";
                 ^
1 error
3 warnings

FAILURE: Build failed with an exception.

* What went wrong:
Execution failed for task ':compileJava'.
> Compilation failed; see the compiler error output for details.

* Try:
> Run with --stacktrace option to get the stack trace.
> Run with --info or --debug option to get more log output.
> Run with --scan to get full insights.

* Get more help at https://help.gradle.org

BUILD FAILED in 2s
Error: gradle build failed: exit status 1
Make sure the TeaVM plugin is configured in the Gradle build file.
EOF

# Precondition: the issue's complaint is true of this fixture.
if tail -8 "$WORK/gradle-failure.log" | grep -qE 'What went wrong|error: incompatible types'; then
  echo "UNMEASURED: the fixture's last eight lines already contain its cause, so it cannot show the cause being hidden." >&2
  exit 2
fi

# A `cleat` that fails java-workflow with $FIXTURE's text and succeeds for every
# other example, or succeeds for all when FIXTURE is unset.
mkdir -p "$WORK/bin"
cat >"$WORK/bin/cleat" <<'EOF'
#!/usr/bin/env bash
if [[ -n "${FIXTURE:-}" && "$PWD $*" == *java-workflow* ]]; then
  cat "$FIXTURE"
  exit 1
fi
exit 0
EOF
chmod +x "$WORK/bin/cleat"

failures=0
run_script() { # fixture-or-empty [extra env...] -> output in $WORK/out, status in $RC
  local fixture=$1
  shift
  env FIXTURE="$fixture" CLEAT_BIN="$WORK/bin/cleat" "$@" bash "$SCRIPT" >"$WORK/out" 2>&1
  RC=$?
}
expect() { # name condition-result(0=true) detail
  if [[ "$2" == 0 ]]; then
    printf '  ok    %s\n' "$1"
  else
    printf '  FAIL  %s\n' "$1" >&2
    [[ -n "${3:-}" ]] && printf '        %s\n' "$3" >&2
    failures=$((failures + 1))
  fi
}
has() { grep -qF -- "$1" "$WORK/out"; }

echo "1. the real Gradle failure"
run_script "$WORK/gradle-failure.log"
expect "exits 1" "$((RC == 1 ? 0 : 1))" "rc=$RC"
expect "names the failing example" "$(has 'FAIL  examples/java-workflow'; echo $?)"
expect "prints the compiler error" "$(has 'error: incompatible types: String cannot be converted to int'; echo $?)"
expect "prints '* What went wrong:'" "$(has '* What went wrong:'; echo $?)"
expect "prints the failing task" "$(has "Execution failed for task ':compileJava'."; echo $?)"
expect "other examples still report OK" "$(grep -qE '^OK    examples/' "$WORK/out"; echo $?)"

echo "2. a long log, cause inside the cap but far above the last eight lines"
{
  for i in $(seq 1 600); do echo "noise line $i"; done
  echo "THE-CAUSE-AT-LINE-601"
  for i in $(seq 1 100); do echo "epilogue line $i"; done
} >"$WORK/long.log"
run_script "$WORK/long.log"
expect "prints the cause" "$(has 'THE-CAUSE-AT-LINE-601'; echo $?)"
expect "says lines were omitted (701 > 400)" "$(has '301 earlier line(s) of 701 omitted'; echo $?)" "$(grep -m1 omitted "$WORK/out")"

echo "3. past the cap: the limit is stated, not hidden"
{
  echo "THE-CAUSE-AT-LINE-1"
  for i in $(seq 1 999); do echo "noise line $i"; done
} >"$WORK/huge.log"
run_script "$WORK/huge.log"
expect "says how many lines were omitted" "$(has '600 earlier line(s) of 1000 omitted'; echo $?)" "$(grep -m1 omitted "$WORK/out")"
expect "does not claim the dropped cause" "$(has 'THE-CAUSE-AT-LINE-1' && echo 1 || echo 0)"
run_script "$WORK/huge.log" BDE_FAIL_LOG_LINES=1000
expect "BDE_FAIL_LOG_LINES raises the cap" "$(has 'THE-CAUSE-AT-LINE-1'; echo $?)"

echo "4. nothing fails"
run_script ""
expect "exits 0" "$((RC == 0 ? 0 : 1))" "rc=$RC"
expect "prints no FAIL" "$(grep -q '^FAIL ' "$WORK/out" && echo 1 || echo 0)"

if (( failures > 0 )); then
  echo "build-documented-examples self-test: $failures check(s) failed" >&2
  exit 1
fi
echo "build-documented-examples self-test: all checks held"
