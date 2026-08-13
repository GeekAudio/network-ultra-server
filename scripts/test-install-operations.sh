#!/usr/bin/env bash
# Static operation-policy checks plus the real source-tree transaction fixture.
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
SOURCE_INSTALL="$SCRIPT_DIR/install-from-source.sh"
FIXTURE=$(mktemp -d /tmp/network-ultra-source-swap-fixture.XXXXXX)
trap 'rm -rf -- "$FIXTURE"' EXIT

scripts=(
  "$SCRIPT_DIR/install.sh"
  "$SOURCE_INSTALL"
  "$SCRIPT_DIR/update-server.sh"
  "$SCRIPT_DIR/set-password.sh"
)

for script in "${scripts[@]}"; do
  grep -F 'LOCK_DIR="/run/network-ultra-server-update"' "$script" >/dev/null || {
    printf 'fixture error: %s does not use the shared fixed lock directory\n' "$script" >&2
    exit 1
  }
  grep -F 'LOCK_FILE="$LOCK_DIR/update.lock"' "$script" >/dev/null || {
    printf 'fixture error: %s does not use the shared fixed lock file\n' "$script" >&2
    exit 1
  }
  grep -F 'flock -n 9' "$script" >/dev/null || {
    printf 'fixture error: %s does not hold the shared lock descriptor\n' "$script" >&2
    exit 1
  }
  grep -F "stat -c '%u:%g:%a' \"\$LOCK_DIR\"" "$script" >/dev/null || {
    printf 'fixture error: %s does not validate lock-directory ownership/mode\n' "$script" >&2
    exit 1
  }
  grep -F "stat -c '%u:%g:%a' \"\$LOCK_FILE\"" "$script" >/dev/null || {
    printf 'fixture error: %s does not validate lock-file ownership/mode\n' "$script" >&2
    exit 1
  }
done

if grep -En 'curl.*127\.0\.0\.1:18901/healthz' "${scripts[@]}" >/dev/null; then
  printf 'fixture error: a mutating script still hard-codes the health endpoint\n' >&2
  exit 1
fi
grep -F 'STAGE=$(mktemp -d "$SRC_PARENT/.network-ultra-install.XXXXXX")' "$SOURCE_INSTALL" >/dev/null || {
  printf 'fixture error: source installer stage is not adjacent to the live /opt tree\n' >&2
  exit 1
}

assert_lock_precedes() {
  local script=$1 marker=$2 lock_line mutation_line
  lock_line=$(grep -n -m1 'flock -n 9' "$script" | cut -d: -f1)
  mutation_line=$(grep -n -m1 -F "$marker" "$script" | cut -d: -f1)
  [[ -n "$lock_line" && -n "$mutation_line" && "$lock_line" -lt "$mutation_line" ]] || {
    printf 'fixture error: %s does not lock before %s\n' "$script" "$marker" >&2
    exit 1
  }
}
assert_lock_precedes "$SCRIPT_DIR/install.sh" 'TMP=$(mktemp'
assert_lock_precedes "$SOURCE_INSTALL" 'STAGE=$(mktemp'
assert_lock_precedes "$SCRIPT_DIR/update-server.sh" 'STAGE=$(mktemp'
assert_lock_precedes "$SCRIPT_DIR/set-password.sh" 'TMP_CFG="$(mktemp'

prepare_swap() {
  rm -rf -- "$FIXTURE/opt"
  mkdir -p "$FIXTURE/opt/network-ultra-src" "$FIXTURE/opt/.network-ultra-install.test/src"
  printf 'old-tree\n' >"$FIXTURE/opt/network-ultra-src/state.txt"
  printf 'new-tree\n' >"$FIXTURE/opt/.network-ultra-install.test/src/state.txt"
}

# Interrupt precisely after the old tree rename. The production helper must
# finish the rename pair, then its EXIT rollback must restore the original tree.
prepare_swap
HOOK="$FIXTURE/swap-hook"
NU_SOURCE_SWAP_TEST_ROOT="$FIXTURE" NU_SOURCE_SWAP_TEST_HOOK="$HOOK" \
  "$SOURCE_INSTALL" >"$FIXTURE/term.out" 2>&1 &
SWAP_PID=$!
for _ in {1..100}; do
  [[ -e "$HOOK.ready" ]] && break
  /usr/bin/sleep 0.05
done
[[ -e "$HOOK.ready" ]] || {
  printf 'fixture error: source transaction did not reach the blocking hook\n' >&2
  kill -TERM "$SWAP_PID" 2>/dev/null || true
  exit 1
}
kill -TERM "$SWAP_PID"
set +e
wait "$SWAP_PID"
SWAP_STATUS=$?
set -e
[[ "$SWAP_STATUS" -ne 0 ]] || {
  printf 'fixture error: terminated source transaction exited successfully\n' >&2
  exit 1
}
[[ "$(cat "$FIXTURE/opt/network-ultra-src/state.txt")" == old-tree ]] || {
  printf 'fixture error: terminated source transaction did not restore old tree\n' >&2
  exit 1
}
[[ ! -e "$FIXTURE/opt/network-ultra-src.rollback.test" \
  && ! -e "$FIXTURE/opt/.network-ultra-install.test" ]] || {
  printf 'fixture error: terminated source transaction left a half tree\n' >&2
  exit 1
}

# A non-interrupted run must publish only the complete staged tree.
prepare_swap
HOOK="$FIXTURE/success-hook"
: >"$HOOK.release"
OUTPUT=$(NU_SOURCE_SWAP_TEST_ROOT="$FIXTURE" NU_SOURCE_SWAP_TEST_HOOK="$HOOK" "$SOURCE_INSTALL")
[[ "$OUTPUT" == *SOURCE_SWAP_FIXTURE_OK* \
  && "$(cat "$FIXTURE/opt/network-ultra-src/state.txt")" == new-tree \
  && ! -e "$FIXTURE/opt/network-ultra-src.rollback.test" \
  && ! -e "$FIXTURE/opt/.network-ultra-install.test" ]] || {
  printf 'fixture error: successful source transaction did not publish one complete tree\n' >&2
  exit 1
}

printf 'INSTALL_OPERATIONS_FIXTURE_OK\n'
