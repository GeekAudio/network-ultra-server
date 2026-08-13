#!/usr/bin/env bash
# Destructive-path fixture for update-server.sh. All paths stay under /tmp.
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
UPDATE_SCRIPT="$SCRIPT_DIR/update-server.sh"
FIXTURE=$(mktemp -d /tmp/network-ultra-update-fixture.XXXXXX)
trap 'rm -rf -- "$FIXTURE"' EXIT

REMOTE="$FIXTURE/remote.git"
WORK="$FIXTURE/work"
LIVE="$FIXTURE/opt/network-ultra-src"
BIN_DIR="$FIXTURE/usr/local/bin"
SVC_DIR="$FIXTURE/etc/systemd/system"
CFG_DIR="$FIXTURE/etc/network-ultra"
FAKE_BIN="$FIXTURE/fake-bin"
LOG="$FIXTURE/systemctl.log"
mkdir -p "$WORK/cmd/server" "$WORK/systemd" "$BIN_DIR" "$SVC_DIR" "$CFG_DIR" "$FAKE_BIN"

git init --bare "$REMOTE" >/dev/null
git -C "$WORK" init >/dev/null
git -C "$WORK" config user.email fixture@example.invalid
git -C "$WORK" config user.name fixture
cat >"$WORK/go.mod" <<'EOF'
module fixture.invalid/network-ultra

go 1.22
EOF
cat >"$WORK/cmd/server/main.go" <<'EOF'
package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

var buildVersion = "dev"

func main() {
	configPath := flag.String("config", "", "")
	showVersion := flag.Bool("version", false, "")
	printHealthURL := flag.Bool("print-health-url", false, "")
	flag.Parse()
	if *showVersion {
		fmt.Printf("network-ultra-server %s\n", buildVersion)
		return
	}
	if *printHealthURL {
		if override := os.Getenv("NU_FIXTURE_HEALTH_URL_OVERRIDE"); override != "" {
			fmt.Println(override)
			return
		}
		url, err := fixtureHealthURL(*configPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println(url)
	}
}

func fixtureHealthURL(path string) (string, error) {
	listen := "127.0.0.1:18901"
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "health_listen") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			return "", errors.New("bad health_listen")
		}
		listen = strings.Trim(strings.TrimSpace(parts[1]), "\"'")
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", err
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", errors.New("health host is not an explicit loopback IP")
	}
	host = ip.String()
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", errors.New("bad health port")
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(n)) + "/healthz", nil
}
EOF
printf '%s\n' '[Service]' 'ExecStart=/usr/local/bin/network-ultra-server' >"$WORK/systemd/network-ultra-server.service"
git -C "$WORK" add .
printf '%s\n' old >"$WORK/source-state.txt"
git -C "$WORK" add source-state.txt
git -C "$WORK" commit -m old-fixture >/dev/null
OLD_COMMIT=$(git -C "$WORK" rev-parse HEAD)
git -C "$WORK" remote add origin "$REMOTE"
git -C "$WORK" push origin HEAD:main >/dev/null
git clone "$REMOTE" "$LIVE" >/dev/null 2>&1
git -C "$LIVE" checkout --detach "$OLD_COMMIT" >/dev/null
printf '%s\n' new >"$WORK/source-state.txt"
printf '%s\n' '[Service]' 'ExecStart=/usr/local/bin/network-ultra-server' 'Environment=FIXTURE_VERSION=new' >"$WORK/systemd/network-ultra-server.service"
git -C "$WORK" add source-state.txt systemd/network-ultra-server.service
git -C "$WORK" commit -m new-fixture >/dev/null
COMMIT=$(git -C "$WORK" rev-parse HEAD)
git -C "$WORK" push origin HEAD:main >/dev/null
printf '%s\n' '#!/usr/bin/env bash' 'printf "%s\n" installed-binary' >"$BIN_DIR/network-ultra-server"
chmod 0755 "$BIN_DIR/network-ultra-server"
printf '%s\n' '[Service]' 'ExecStart=/usr/local/bin/network-ultra-server' 'Environment=FIXTURE_VERSION=old' >"$SVC_DIR/network-ultra-server.service"
printf '%s\n' '[server]' 'health_listen = "127.0.0.1:29001"' >"$CFG_DIR/config.toml"

cat >"$FAKE_BIN/systemctl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${NU_FIXTURE_SYSTEMCTL_LOG:?}"
case "${1:-}" in
  is-active)
    [[ -f "${NU_FIXTURE_STATE:?}/active" ]]
    exit $?
    ;;
  is-enabled)
    [[ -f "${NU_FIXTURE_STATE:?}/enabled" ]]
    exit $?
    ;;
  start)
    if [[ -f "${NU_FIXTURE_STATE:?}/fail-next-start" ]]; then
      rm -f -- "${NU_FIXTURE_STATE:?}/fail-next-start"
      exit 1
    fi
    : >"${NU_FIXTURE_STATE:?}/active"
    ;;
  stop) rm -f -- "${NU_FIXTURE_STATE:?}/active" ;;
  enable) : >"${NU_FIXTURE_STATE:?}/enabled" ;;
  disable) rm -f -- "${NU_FIXTURE_STATE:?}/enabled" ;;
esac
exit 0
EOF
cat >"$FAKE_BIN/curl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${NU_FIXTURE_CURL_LOG:?}"
if [[ "${NU_FIXTURE_FAIL_HEALTH:-0}" == 1 ]]; then exit 22; fi
printf '{"status":"ok","version":"%s"}\n' "${NU_RELEASE_VERSION:?}"
EOF
cat >"$FAKE_BIN/go" <<'EOF'
#!/usr/bin/env bash
if [[ "${1:-}" == test && "${NU_FIXTURE_FAIL_BUILD:-0}" == 1 ]]; then exit 42; fi
if [[ "${1:-}" == test && "${NU_FIXTURE_TERM_BUILD:-0}" == 1 ]]; then
  kill -TERM "$PPID"
  exit 143
fi
exec "${NU_FIXTURE_REAL_GO:?}" "$@"
EOF
cat >"$FAKE_BIN/sleep" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod 0755 "$FAKE_BIN/systemctl" "$FAKE_BIN/curl" "$FAKE_BIN/go" "$FAKE_BIN/sleep"

tree_hash() {
  git -C "$LIVE" status --porcelain=v1 --untracked-files=all
  git -C "$LIVE" rev-parse HEAD
  git -C "$LIVE" remote get-url origin
  git -C "$LIVE" ls-files -s
  sha256sum "$BIN_DIR/network-ultra-server" "$SVC_DIR/network-ultra-server.service"
}

if [[ -z "${NU_FIXTURE_REAL_GO:-}" ]]; then
  NU_FIXTURE_REAL_GO=$(command -v go || true)
elif [[ "$NU_FIXTURE_REAL_GO" =~ ^[A-Za-z]:\\ ]]; then
  NU_FIXTURE_REAL_GO=$(cygpath -u "$NU_FIXTURE_REAL_GO")
fi
[[ -n "$NU_FIXTURE_REAL_GO" && -x "$NU_FIXTURE_REAL_GO" ]] || {
  printf 'fixture error: set NU_FIXTURE_REAL_GO to an executable Go toolchain\n' >&2
  exit 1
}
export NU_FIXTURE_REAL_GO
export PATH="$FAKE_BIN:$PATH"
export NU_UPDATE_TEST_ROOT="$FIXTURE"
export NU_UPDATE_TEST_REPO="$REMOTE"
export NU_SOURCE_COMMIT="$COMMIT"
export NU_RELEASE_VERSION="1.2.3"
export NU_FIXTURE_SYSTEMCTL_LOG="$LOG"
export NU_FIXTURE_CURL_LOG="$FIXTURE/curl.log"
export NU_FIXTURE_STATE="$FIXTURE/systemctl-state"
mkdir -p "$NU_FIXTURE_STATE"
: >"$NU_FIXTURE_STATE/active"
: >"$NU_FIXTURE_STATE/enabled"
BEFORE=$(tree_hash)

assert_rolled_back() {
  local label=$1
  local actual
  actual=$(tree_hash)
  [[ "$BEFORE" == "$actual" ]] || {
    printf 'fixture error: %s changed installed source/binary/service\n' "$label" >&2
    exit 1
  }
  [[ -f "$NU_FIXTURE_STATE/active" && -f "$NU_FIXTURE_STATE/enabled" ]] || {
    printf 'fixture error: %s did not restore active/enabled state\n' "$label" >&2
    exit 1
  }
}

LOCK_HOOK="$FIXTURE/lock-hook"
NU_UPDATE_TEST_HOLD_AFTER_LOCK="$LOCK_HOOK" "$UPDATE_SCRIPT" >"$FIXTURE/holder.out" 2>&1 &
HOLDER_PID=$!
for _ in {1..100}; do
  [[ -e "$LOCK_HOOK.ready" ]] && break
  /usr/bin/sleep 0.05
done
[[ -e "$LOCK_HOOK.ready" ]] || {
  printf 'fixture error: first updater never acquired the lock\n' >&2
  kill -TERM "$HOLDER_PID" 2>/dev/null || true
  exit 1
}
SECOND_OUTPUT=""
if SECOND_OUTPUT=$("$UPDATE_SCRIPT" 2>&1); then
  printf 'fixture error: concurrent updater unexpectedly acquired the lock\n' >&2
  kill -TERM "$HOLDER_PID" 2>/dev/null || true
  exit 1
fi
[[ "$SECOND_OUTPUT" == *"another server operation is already running"* ]] || {
  printf 'fixture error: concurrent updater failed for the wrong reason: %s\n' "$SECOND_OUTPUT" >&2
  kill -TERM "$HOLDER_PID" 2>/dev/null || true
  exit 1
}
[[ "$BEFORE" == "$(tree_hash)" ]] || {
  printf 'fixture error: rejected concurrent updater touched installed state\n' >&2
  kill -TERM "$HOLDER_PID" 2>/dev/null || true
  exit 1
}
kill -TERM "$HOLDER_PID"
set +e
wait "$HOLDER_PID"
HOLDER_STATUS=$?
set -e
[[ "$HOLDER_STATUS" -ne 0 ]] || {
  printf 'fixture error: signalled lock holder exited successfully\n' >&2
  exit 1
}

BUILD_FAIL_OUTPUT=""
if BUILD_FAIL_OUTPUT=$(NU_FIXTURE_FAIL_BUILD=1 "$UPDATE_SCRIPT" 2>&1); then
	printf 'fixture error: build failure unexpectedly succeeded\n' >&2
	exit 1
fi
[[ "$BUILD_FAIL_OUTPUT" != *"another server operation is already running"* ]] || {
  printf 'fixture error: signal did not release update lock\n' >&2
  exit 1
}
assert_rolled_back 'pre-commit build failure'

if NU_FIXTURE_TERM_BUILD=1 "$UPDATE_SCRIPT" >/dev/null 2>&1; then
  printf 'fixture error: terminated build unexpectedly succeeded\n' >&2
  exit 1
fi
assert_rolled_back 'pre-commit termination'

if NU_FIXTURE_FAIL_HEALTH=1 "$UPDATE_SCRIPT" >/dev/null 2>&1; then
  printf 'fixture error: failed health check unexpectedly succeeded\n' >&2
  exit 1
fi
assert_rolled_back 'post-swap health failure'

: >"$NU_FIXTURE_STATE/fail-next-start"
if "$UPDATE_SCRIPT" >/dev/null 2>&1; then
  printf 'fixture error: failed service start unexpectedly succeeded\n' >&2
  exit 1
fi
assert_rolled_back 'post-swap start failure'

"$UPDATE_SCRIPT" >/dev/null
[[ "$(git -C "$LIVE" rev-parse HEAD)" == "$COMMIT" ]]
[[ "$(cat "$LIVE/source-state.txt")" == new ]]
[[ "$($BIN_DIR/network-ultra-server -version)" == 'network-ultra-server 1.2.3' ]]
grep -F -- 'http://127.0.0.1:29001/healthz' "$NU_FIXTURE_CURL_LOG" >/dev/null || {
  printf 'fixture error: updater did not probe configured IPv4 health port\n' >&2
  exit 1
}
if compgen -G "${LIVE}.rollback.*" >/dev/null; then
  printf 'fixture error: successful update left a source rollback directory\n' >&2
  exit 1
fi

printf '%s\n' '[server]' 'health_listen = "[::1]:29002"' >"$CFG_DIR/config.toml"
: >"$NU_FIXTURE_CURL_LOG"
"$UPDATE_SCRIPT" >/dev/null
grep -F -- 'http://[::1]:29002/healthz' "$NU_FIXTURE_CURL_LOG" >/dev/null || {
  printf 'fixture error: updater did not probe configured IPv6 health port\n' >&2
  exit 1
}

PUBLIC_BEFORE=$(tree_hash)
printf '%s\n' '[server]' 'health_listen = "203.0.113.10:29003"' >"$CFG_DIR/config.toml"
PUBLIC_OUTPUT=""
if PUBLIC_OUTPUT=$("$UPDATE_SCRIPT" 2>&1); then
  printf 'fixture error: public health endpoint was accepted\n' >&2
  exit 1
fi
[[ "$PUBLIC_OUTPUT" == *"valid loopback health URL"* ]] || {
  printf 'fixture error: public health endpoint failed for the wrong reason: %s\n' "$PUBLIC_OUTPUT" >&2
  exit 1
}
[[ "$PUBLIC_BEFORE" == "$(tree_hash)" ]] || {
  printf 'fixture error: invalid public health config touched installed state\n' >&2
  exit 1
}
[[ -f "$NU_FIXTURE_STATE/active" && -f "$NU_FIXTURE_STATE/enabled" ]] || {
  printf 'fixture error: invalid public health config changed service state\n' >&2
  exit 1
}

printf '%s\n' '[server]' 'health_listen = "127.0.0.1:29001"' >"$CFG_DIR/config.toml"
STRICT_BEFORE=$(tree_hash)
if NU_FIXTURE_HEALTH_URL_OVERRIDE='http://203.0.113.10:29003/healthz' "$UPDATE_SCRIPT" >/dev/null 2>&1; then
  printf 'fixture error: staged binary public health URL bypassed shell validation\n' >&2
  exit 1
fi
INJECTION_MARKER="$FIXTURE/health-url-injection"
if NU_FIXTURE_HEALTH_URL_OVERRIDE="http://127.0.0.1:29001/healthz;touch $INJECTION_MARKER" "$UPDATE_SCRIPT" >/dev/null 2>&1; then
  printf 'fixture error: injected health URL bypassed shell validation\n' >&2
  exit 1
fi
[[ ! -e "$INJECTION_MARKER" && "$STRICT_BEFORE" == "$(tree_hash)" ]] || {
  printf 'fixture error: rejected staged health URL changed state or executed input\n' >&2
  exit 1
}

# An administrator-stopped service must remain stopped throughout an update.
# Re-running the exact pinned commit is sufficient to exercise the same source
# and binary swap without constructing a second remote commit.
rm -f -- "$NU_FIXTURE_STATE/active"
: >"$LOG"
"$UPDATE_SCRIPT" >/dev/null
if grep -Eq '^start([[:space:]]|$)' "$LOG"; then
  printf 'fixture error: inactive update briefly started the service; log follows:\n' >&2
  sed 's/^/  /' "$LOG" >&2
  exit 1
fi
[[ ! -f "$NU_FIXTURE_STATE/active" ]] || {
  printf 'fixture error: inactive service was left active\n' >&2
  exit 1
}
[[ -f "$NU_FIXTURE_STATE/enabled" ]] || {
  printf 'fixture error: inactive update changed enabled state\n' >&2
  exit 1
}
printf 'UPDATE_SERVER_FIXTURE_OK\n'
