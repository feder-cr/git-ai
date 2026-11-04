#!/usr/bin/env bash
# conformance-replay.sh proves that the conformance harness catches past bugs: it runs the harness of
# this tree against their pre-fix commits. Each commit is unpacked with `git archive` into a directory
# of its own, so that the worktree and its build stay untouched; the harness and the tests it reuses
# are copied in.
#
#   _support/conformance-replay.sh WORKDIR [CASE...]
#
# Make arguments go in CONFORMANCE_REPLAY_MAKE_ARGS (for example a shared DEPENDENCY_DIR), and a command
# that serializes `make prepare-tests` on a shared machine in CONFORMANCE_REPLAY_LOCK.
#
# A case is CAUGHT when the harness fails on the pre-fix commit, in a test that names the bug; the
# script prints what failed, and a human checks that it failed for that reason. The gate is at least
# 2 of the 4 known bugs caught: these three, and the admission that a failed push could keep, which
# TestConformanceFailedPushReleasesTheAdmission checks on the current tree. The commits also predate
# later fixes, so rows of those fixes fail with them (first_parent_ref, start_point, the writes through
# a symbolic branch into refs/graft/): the case is caught only by the rows that name its own bug.
set -uo pipefail

[ $# -ge 1 ] || { echo "usage: $0 WORKDIR [CASE...]"; exit 2; }
workdir=$1; shift
root=$(git rev-parse --show-toplevel)
mkdir -p "$workdir"

# name, pre-fix commit, packages, the tests that are expected to catch it.
declare -A commits packages tests
commits[exact-branch]=914e481
packages[exact-branch]="./internal/gitaly/service/operations ./internal/graft/conformance"
tests[exact-branch]='^(Test.*ResolvesBranchesExactly|TestConformanceDecoys)$'

commits[graft-reservation]=3feafcd
packages[graft-reservation]="./internal/graft/conformance"
tests[graft-reservation]='^(TestConformanceAgentWritesThroughSymbolicBranches|TestConformanceAgentNamespaces)$'

commits[fanout-race]=3f1ee11^1
packages[fanout-race]="./internal/graft/conformance"
tests[fanout-race]='^TestConformanceClientTable$'

cases=("$@")
[ ${#cases[@]} -gt 0 ] || cases=(exact-branch graft-reservation fanout-race)

caught=0
for name in "${cases[@]}"; do
  copy="$workdir/$name"
  mkdir -p "$copy"
  if ! git -C "$root" archive "${commits[$name]}" | tar -x -C "$copy"; then
    echo "$name: cannot unpack ${commits[$name]}"; exit 2
  fi

  # The harness, and the decoy tests of the fix. Tests that reach into what the fix added stay out.
  mkdir -p "$copy/internal/graft/conformance"
  cp "$root"/internal/graft/conformance/*.go "$copy/internal/graft/conformance/"
  if [ "$name" = exact-branch ]; then
    python3 "$root/_support/conformance-replay-strip.py" \
      "$root/internal/gitaly/service/operations/branch_exact_test.go" \
      "$copy/internal/gitaly/service/operations/branch_exact_test.go"
  fi

  log="$workdir/$name.log"
  echo "== $name at ${commits[$name]}: building"
  # shellcheck disable=SC2086
  if ! ${CONFORMANCE_REPLAY_LOCK:-} make -C "$copy" -s prepare-tests ${CONFORMANCE_REPLAY_MAKE_ARGS:-} > "$log" 2>&1; then
    echo "$name: BUILD FAILED, see $log"; continue
  fi
  echo "== $name: running ${tests[$name]}"
  # shellcheck disable=SC2086
  make -C "$copy" -s test-go ${CONFORMANCE_REPLAY_MAKE_ARGS:-} \
    TEST_PACKAGES="${packages[$name]}" \
    TEST_OPTIONS="-count=1 -timeout 20m -run '${tests[$name]//\$/\$\$}'" \
    TEST_FORMAT=standard-verbose TEST_JUNIT_REPORT="$workdir/$name.junit.xml" TEST_JSON_REPORT="$workdir/$name.json" \
    >> "$log" 2>&1
  code=$?
  if grep -q '\[build failed\]' "$log"; then
    # A package that does not compile proves nothing about the bug.
    echo "$name: BUILD FAILED, see $log"; grep -A6 '^=== Errors' "$log" | head -12; continue
  fi
  if [ $code -ne 0 ] && grep -qE '^\s*--- FAIL' "$log"; then
    caught=$((caught + 1))
    echo "$name: CAUGHT (exit $code); failed tests:"
    grep -E '^\s*--- FAIL' "$log" | sed 's/^ *//' | sort -u | head -40
  else
    echo "$name: NOT CAUGHT"
  fi
  echo "   log: $log"
done

echo "caught $caught of ${#cases[@]} replayed"
