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
#      failing everything;
#   5. a REAL transient fetch failure (the 429 log) that passes on the second attempt: exit 0, the failure's
#      cause is printed on the RETRY line, the example is OK and is listed as having needed a retry, and the
#      build ran exactly twice (cleat#4254);
#   6. a REAL "Could not find" fetch failure that never clears: exit 1, FAIL, and exactly four attempts (three
#      retries), so the retry is bounded and a persistent failure still turns the job red;
#   7. a failure that is NOT a fetch (the compile error of case 1): exactly ONE attempt and no RETRY, so a real
#      failure is never retried into a pass;
#   8. BDE_FETCH_RETRIES=0 disables the retry: one attempt on the 429 log;
#   9. THE WAITS, recorded through a stand-in `sleep` (cleat#4353): three of them, each inside its [delay, 1.5 x delay]
#      band with the delay DOUBLING (20, 40, 80), the RETRY line names the wait that was slept, and the first wait
#      VARIES across runs. The last is its own check on purpose: with the jitter removed the waits are exactly 20, 40
#      and 80, which is inside every bound, so a bounds check alone certifies a loop that has lost its jitter (the same
#      lesson as scripts/fetch-wasm-tools.sh's case 5c).
#
# The two fetch fixtures are the java-workflow build's own output from real failed runs of this job on 2026-11-20
# (run 44674959287: HTTP 429 from repo.maven.apache.org; run 44773417605: `Could not find org.teavm:teavm-core:0.10.2`
# at plugins.gradle.org), not invented text.
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

cat >"$WORK/fetch-429.log" <<'EOF'
Vetting Java project in /home/runner/work/cleat/cleat/examples/java-workflow...

  Summary: 1 files, 0 errors, 0 warnings
  Compiling Java to WASM via TeaVM...
Downloading https://services.gradle.org/distributions/gradle-7.6.4-bin.zip
...........10%............20%............30%...........40%............50%............60%...........70%............80%............90%............100%

FAILURE: Build failed with an exception.

* What went wrong:
A problem occurred configuring root project 'java-workflow'.
> Could not resolve all files for configuration ':classpath'.
   > Could not resolve org.teavm:teavm-core:0.10.2.
     Required by:
         project : > org.teavm:org.teavm.gradle.plugin:0.10.2 > org.teavm:teavm-gradle-plugin:0.10.2
      > Could not resolve org.teavm:teavm-core:0.10.2.
         > Could not get resource 'https://plugins.gradle.org/m2/org/teavm/teavm-core/0.10.2/teavm-core-0.10.2.pom'.
            > Could not GET 'https://repo.maven.apache.org/maven2/org/teavm/teavm-core/0.10.2/teavm-core-0.10.2.pom'. Received status code 429 from server: Too Many Requests
   > Could not resolve org.teavm:teavm-tooling:0.10.2.
     Required by:
         project : > org.teavm:org.teavm.gradle.plugin:0.10.2 > org.teavm:teavm-gradle-plugin:0.10.2
      > Could not resolve org.teavm:teavm-tooling:0.10.2.
         > Could not get resource 'https://plugins.gradle.org/m2/org/teavm/teavm-tooling/0.10.2/teavm-tooling-0.10.2.pom'.
            > Could not GET 'https://repo.maven.apache.org/maven2/org/teavm/teavm-tooling/0.10.2/teavm-tooling-0.10.2.pom'. Received status code 429 from server: Too Many Requests

* Try:
> Run with --stacktrace option to get the stack trace.
> Run with --info or --debug option to get more log output.
> Run with --scan to get full insights.

* Get more help at https://help.gradle.org

BUILD FAILED in 27s
Error: gradle build failed: exit status 1
Make sure the TeaVM plugin is configured in the Gradle build file.
EOF
cat >"$WORK/fetch-notfound.log" <<'EOF'
Vetting Java project in /home/runner/work/cleat/cleat/examples/java-workflow...

  Summary: 1 files, 0 errors, 0 warnings
  Compiling Java to WASM via TeaVM...
Downloading https://services.gradle.org/distributions/gradle-7.6.4-bin.zip
...........10%............20%............30%...........40%............50%............60%...........70%............80%............90%............100%

FAILURE: Build failed with an exception.

* What went wrong:
A problem occurred configuring root project 'java-workflow'.
> Could not resolve all files for configuration ':classpath'.
   > Could not find org.teavm:teavm-core:0.10.2.
     Searched in the following locations:
       - https://plugins.gradle.org/m2/org/teavm/teavm-core/0.10.2/teavm-core-0.10.2.pom
     If the artifact you are trying to retrieve can be found in the repository but without metadata in 'Maven POM' format, you need to adjust the 'metadataSources { ... }' of the repository declaration.
     Required by:
         project : > org.teavm:org.teavm.gradle.plugin:0.10.2 > org.teavm:teavm-gradle-plugin:0.10.2
   > Could not find org.teavm:teavm-tooling:0.10.2.
     Searched in the following locations:
       - https://plugins.gradle.org/m2/org/teavm/teavm-tooling/0.10.2/teavm-tooling-0.10.2.pom
     If the artifact you are trying to retrieve can be found in the repository but without metadata in 'Maven POM' format, you need to adjust the 'metadataSources { ... }' of the repository declaration.
     Required by:
         project : > org.teavm:org.teavm.gradle.plugin:0.10.2 > org.teavm:teavm-gradle-plugin:0.10.2

* Try:
> Run with --stacktrace option to get the stack trace.
> Run with --info or --debug option to get more log output.
> Run with --scan to get full insights.

* Get more help at https://help.gradle.org

BUILD FAILED in 21s
Error: gradle build failed: exit status 1
Make sure the TeaVM plugin is configured in the Gradle build file.
EOF

# A `cleat` that fails java-workflow with $FIXTURE's text and succeeds for every
# other example, or succeeds for all when FIXTURE is unset. When FAILS_FIRST is
# set it fails only that many times (counted in $COUNTER) and then succeeds; the
# count of java-workflow builds is left in $COUNTER either way.
mkdir -p "$WORK/bin"
cat >"$WORK/bin/cleat" <<'EOF'
#!/usr/bin/env bash
if [[ -n "${FIXTURE:-}" && "$PWD $*" == *java-workflow* ]]; then
  n=$(( $(cat "$COUNTER" 2>/dev/null || echo 0) + 1 ))
  echo "$n" >"$COUNTER"
  if [[ -z "${FAILS_FIRST:-}" || "$n" -le "$FAILS_FIRST" ]]; then
    cat "$FIXTURE"
    exit 1
  fi
fi
exit 0
EOF
chmod +x "$WORK/bin/cleat"

failures=0
run_script() { # fixture-or-empty [extra env...] -> output in $WORK/out, status in $RC
  local fixture=$1
  shift
  rm -f "$WORK/count"
  env FIXTURE="$fixture" COUNTER="$WORK/count" BDE_RETRY_SLEEP=0 CLEAT_BIN="$WORK/bin/cleat" "$@" bash "$SCRIPT" >"$WORK/out" 2>&1
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

attempts() { cat "$WORK/count" 2>/dev/null || echo 0; }

echo "5. a real 429 fetch failure that clears on the second attempt"
run_script "$WORK/fetch-429.log" FAILS_FIRST=1
expect "exits 0" "$((RC == 0 ? 0 : 1))" "rc=$RC"
expect "built the example exactly twice" "$((  $(attempts) == 2 ? 0 : 1))" "attempts=$(attempts)"
expect "prints a RETRY line" "$(has 'RETRY examples/java-workflow'; echo $?)"
expect "prints the first failure's cause on it" "$(has 'Received status code 429 from server: Too Many Requests'; echo $?)"
expect "the example is OK and says it was retried" "$(has 'OK    examples/java-workflow (after 1 retry(ies)'; echo $?)"
expect "the summary lists it as needing a retry" "$(has 'RETRIED and then built'; echo $?)"
expect "the totals count it" "$(has 'needed a retry: 1'; echo $?)"

echo "6. a real 'Could not find' fetch failure that never clears"
run_script "$WORK/fetch-notfound.log"
expect "exits 1" "$((RC == 1 ? 0 : 1))" "rc=$RC"
expect "tried exactly four times (three retries), then stopped" "$(( $(attempts) == 4 ? 0 : 1))" "attempts=$(attempts)"
expect "names the failing example" "$(has 'FAIL  examples/java-workflow'; echo $?)"
expect "says it failed after 4 attempts" "$(has 'failed after 4 attempts'; echo $?)"
expect "prints the cause" "$(has 'Could not find org.teavm:teavm-core:0.10.2'; echo $?)"

echo "7. a failure that is not a fetch is never retried"
run_script "$WORK/gradle-failure.log"
expect "exits 1" "$((RC == 1 ? 0 : 1))" "rc=$RC"
expect "built exactly once" "$(( $(attempts) == 1 ? 0 : 1))" "attempts=$(attempts)"
expect "prints no RETRY" "$(has 'RETRY ' && echo 1 || echo 0)"

echo "8. BDE_FETCH_RETRIES=0 disables the retry"
run_script "$WORK/fetch-429.log" BDE_FETCH_RETRIES=0
expect "exits 1" "$((RC == 1 ? 0 : 1))" "rc=$RC"
expect "built exactly once" "$(( $(attempts) == 1 ? 0 : 1))" "attempts=$(attempts)"

echo "9. the waits double, stay inside their jitter band, and vary"
mkdir -p "$WORK/fakebin"
cat >"$WORK/fakebin/sleep" <<'STUB'
#!/bin/sh
# Records what the script asked to sleep, and does not sleep.
echo "$1" >>"$SLEEPLOG"
STUB
chmod +x "$WORK/fakebin/sleep"
nine() { # -> $WORK/sleeps holds the recorded waits, one per line
  rm -f "$WORK/sleeps"
  run_script "$WORK/fetch-notfound.log" BDE_RETRY_SLEEP=20 SLEEPLOG="$WORK/sleeps" PATH="$WORK/fakebin:$PATH"
}
nine
w1=$(sed -n 1p "$WORK/sleeps" 2>/dev/null)
w2=$(sed -n 2p "$WORK/sleeps" 2>/dev/null)
w3=$(sed -n 3p "$WORK/sleeps" 2>/dev/null)
nsleeps=$(wc -l <"$WORK/sleeps" 2>/dev/null | tr -d ' ')
expect "slept exactly three times" "$(( ${nsleeps:-0} == 3 ? 0 : 1))" "waits: $(tr '\n' ' ' <"$WORK/sleeps" 2>/dev/null)"
expect "wait 1 is in [20,30]" "$(( ${w1:--1} >= 20 && ${w1:--1} <= 30 ? 0 : 1))" "w1=$w1"
expect "wait 2 is in [40,60] (doubled)" "$(( ${w2:--1} >= 40 && ${w2:--1} <= 60 ? 0 : 1))" "w2=$w2"
expect "wait 3 is in [80,120] (doubled again)" "$(( ${w3:--1} >= 80 && ${w3:--1} <= 120 ? 0 : 1))" "w3=$w3"
expect "the RETRY lines name the waits that were slept" "$(has "after ${w1}s"; echo $?)" "$(grep -m3 RETRY "$WORK/out" | cut -c1-90)"
firsts=()
outside=0
for _ in 1 2 3 4 5 6 7 8; do
  nine
  firsts+=("$(sed -n 1p "$WORK/sleeps")")
  # EVERY wait of EVERY run is held to its band: the jitter is random, so one run's draw can sit inside a band that a broken jitter
  # usually leaves (a mutant with up to 100% jitter passed a single run's bounds 2 times in 12).
  for want in "1 20 30" "2 40 60" "3 80 120"; do
    read -r idx lo hi <<<"$want"
    got=$(sed -n "${idx}p" "$WORK/sleeps")
    if [[ -z "$got" ]] || (( got < lo || got > hi )); then outside=$((outside + 1)); fi
  done
done
distinct=$(printf '%s\n' "${firsts[@]}" | sort -u | grep -c .)
expect "the first wait varies across 8 runs (jitter is present)" "$(( distinct >= 2 ? 0 : 1))" "first waits: ${firsts[*]}"
expect "all 24 waits across 8 runs are inside their bands" "$(( outside == 0 ? 0 : 1))" "$outside wait(s) outside"

if (( failures > 0 )); then
  echo "build-documented-examples self-test: $failures check(s) failed" >&2
  exit 1
fi
echo "build-documented-examples self-test: all checks held"
