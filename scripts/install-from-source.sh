#!/usr/bin/env bash
# Build and install an exact, operator-selected source commit.
set -euo pipefail

REPO_URL="https://github.com/GeekASMR/network-ultra-server.git"
SRC_DIR="/opt/network-ultra-src"
BIN_PATH="/usr/local/bin/network-ultra-server"
CFG_DIR="/etc/network-ultra"
CFG_FILE="$CFG_DIR/config.toml"
SVC_FILE="/etc/systemd/system/network-ultra-server.service"
SERVICE_USER="network-ultra"
STATE_DIR="/var/lib/network-ultra"

die() { printf 'error: %s\n' "$*" >&2; exit 1; }

restore_source_tree() {
  local live=$1 old=$2 staged=$3 original_present=$4 moved=$5
  if [[ -e "$old" ]]; then
    if [[ -e "$live" ]]; then rm -rf -- "$live"; fi
    mv -- "$old" "$live" || true
  elif [[ "$original_present" -eq 0 && -e "$live" \
    && ( "$moved" -eq 1 || ! -e "$staged" ) ]]; then
    rm -rf -- "$live"
  fi
}

switch_source_tree() {
  local live=$1 staged=$2 old=$3
  PENDING_SIGNAL=0
  trap 'PENDING_SIGNAL=130' INT
  trap 'PENDING_SIGNAL=143' TERM
  if [[ -e "$live" ]]; then
    mv -- "$live" "$old"
    HAD_OLD_SRC=1
  fi

  # Test-only blocking point for the transactional fixture. Root execution can
  # never enter fixture mode, and production ignores the hook entirely.
  if [[ -n "${NU_SOURCE_SWAP_TEST_ROOT:-}" && -n "${NU_SOURCE_SWAP_TEST_HOOK:-}" ]]; then
    : >"${NU_SOURCE_SWAP_TEST_HOOK}.ready"
    while [[ ! -e "${NU_SOURCE_SWAP_TEST_HOOK}.release" && "$PENDING_SIGNAL" -eq 0 ]]; do
      /usr/bin/sleep 0.05
    done
  fi

  mv -- "$staged" "$live"
  SOURCE_MOVED=1
  trap 'exit 130' INT
  trap 'exit 143' TERM
  if [[ "$PENDING_SIGNAL" -ne 0 ]]; then exit "$PENDING_SIGNAL"; fi
}

# Exercise the exact source rename/rollback functions without root or any
# system path. This mode is restricted to a freshly-created /tmp fixture and
# is rejected under root, so it cannot bypass production preconditions.
SOURCE_SWAP_TEST_ROOT="${NU_SOURCE_SWAP_TEST_ROOT:-}"
if [[ -n "$SOURCE_SWAP_TEST_ROOT" ]]; then
  [[ "$EUID" -ne 0 ]] || die "source swap fixture mode is forbidden for root"
  [[ "$SOURCE_SWAP_TEST_ROOT" =~ ^/tmp/network-ultra-source-swap-fixture\.[A-Za-z0-9]+$ ]] \
    || die "NU_SOURCE_SWAP_TEST_ROOT is reserved for the isolated fixture"
  [[ -n "${NU_SOURCE_SWAP_TEST_HOOK:-}" && "$NU_SOURCE_SWAP_TEST_HOOK" == "$SOURCE_SWAP_TEST_ROOT/"* ]] \
    || die "NU_SOURCE_SWAP_TEST_HOOK must stay inside the fixture"
  TEST_LIVE="$SOURCE_SWAP_TEST_ROOT/opt/network-ultra-src"
  TEST_STAGE="$SOURCE_SWAP_TEST_ROOT/opt/.network-ultra-install.test"
  TEST_STAGED="$TEST_STAGE/src"
  TEST_OLD="${TEST_LIVE}.rollback.test"
  [[ -d "$TEST_LIVE" && -d "$TEST_STAGED" && ! -e "$TEST_OLD" ]] || die "source swap fixture trees are invalid"
  ORIGINAL_SRC_PRESENT=1
  HAD_OLD_SRC=0
  SOURCE_MOVED=0
  SUCCESS=0
  rollback_source_swap_fixture() {
    local status=$?
    trap - EXIT INT TERM
    if [[ "$SUCCESS" -ne 1 ]]; then
      restore_source_tree "$TEST_LIVE" "$TEST_OLD" "$TEST_STAGED" "$ORIGINAL_SRC_PRESENT" "$SOURCE_MOVED"
    fi
    rm -rf -- "$TEST_STAGE"
    exit "$status"
  }
  trap rollback_source_swap_fixture EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  switch_source_tree "$TEST_LIVE" "$TEST_STAGED" "$TEST_OLD"
  SUCCESS=1
  rm -rf -- "$TEST_OLD"
  printf 'SOURCE_SWAP_FIXTURE_OK\n'
  exit 0
fi

[[ "$(uname -s)" == Linux ]] || die "Linux is required"
[[ "$EUID" -eq 0 ]] || die "run with sudo/root"
[[ -d /run/systemd/system ]] || die "systemd is required"
command -v git >/dev/null || die "git is required"
command -v go >/dev/null || die "Go 1.22 or newer is required (install from your OS vendor)"
command -v openssl >/dev/null || die "openssl is required for secure credentials"
command -v curl >/dev/null || die "curl is required for health checks"
command -v systemctl >/dev/null || die "systemctl is required"
command -v useradd >/dev/null || die "useradd is required"
command -v userdel >/dev/null || die "userdel is required for rollback"
command -v install >/dev/null || die "install is required"
command -v stat >/dev/null || die "stat is required"
command -v flock >/dev/null || die "util-linux flock is required"

# Serialize source installs, release installs, updates, and password changes on
# one fixed, root-owned descriptor before reading installed state or staging.
LOCK_DIR="/run/network-ultra-server-update"
if [[ -L "$LOCK_DIR" || ( -e "$LOCK_DIR" && ! -d "$LOCK_DIR" ) ]]; then
  die "$LOCK_DIR must be a real directory"
fi
if [[ ! -e "$LOCK_DIR" ]]; then
  install -d -o root -g root -m 0700 "$LOCK_DIR"
fi
[[ "$(stat -c '%u:%g:%a' "$LOCK_DIR")" == "0:0:700" ]] || die "$LOCK_DIR must be root:root mode 0700"
LOCK_FILE="$LOCK_DIR/update.lock"
if [[ -L "$LOCK_FILE" || ( -e "$LOCK_FILE" && ! -f "$LOCK_FILE" ) ]]; then
  die "$LOCK_FILE must be a regular file"
fi
if [[ ! -e "$LOCK_FILE" ]]; then
  ( umask 077; set -o noclobber; : >"$LOCK_FILE" ) 2>/dev/null || true
fi
[[ -f "$LOCK_FILE" && ! -L "$LOCK_FILE" ]] || die "$LOCK_FILE creation failed"
[[ "$(stat -c '%u:%g:%a' "$LOCK_FILE")" == "0:0:600" ]] || die "$LOCK_FILE must be root:root mode 0600"
exec 9<>"$LOCK_FILE"
flock -n 9 || die "another server operation is already running"

validate_health_url() {
  local url=$1 port
  if [[ "$url" =~ ^http://127\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}:([1-9][0-9]{0,4})/healthz$ ]]; then
    port=${BASH_REMATCH[1]}
  elif [[ "$url" =~ ^http://\[::1\]:([1-9][0-9]{0,4})/healthz$ ]]; then
    port=${BASH_REMATCH[1]}
  else
    return 1
  fi
  (( port >= 1 && port <= 65535 ))
}

SOURCE_COMMIT="${NU_SOURCE_COMMIT:-}"
[[ "$SOURCE_COMMIT" =~ ^[0-9a-f]{40}$ ]] || die "NU_SOURCE_COMMIT must be an audited lowercase 40-character commit SHA"
RELEASE_VERSION="${NU_RELEASE_VERSION:-$SOURCE_COMMIT}"
[[ "$RELEASE_VERSION" =~ ^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$ ]] || die "NU_RELEASE_VERSION contains unsafe characters"

GO_MAJOR=$(go env GOVERSION | sed -E 's/^go([0-9]+)\.([0-9]+).*/\1/')
GO_MINOR=$(go env GOVERSION | sed -E 's/^go([0-9]+)\.([0-9]+).*/\2/')
(( GO_MAJOR > 1 || (GO_MAJOR == 1 && GO_MINOR >= 22) )) || die "Go 1.22 or newer is required"

SRC_PARENT=$(dirname -- "$SRC_DIR")
if [[ -L "$SRC_PARENT" || ( -e "$SRC_PARENT" && ! -d "$SRC_PARENT" ) ]]; then
  die "$SRC_PARENT must be a real directory"
fi
if [[ ! -e "$SRC_PARENT" ]]; then
  install -d -o root -g root -m 0755 "$SRC_PARENT"
fi
[[ "$(stat -c '%u:%g' "$SRC_PARENT")" == "0:0" ]] || die "$SRC_PARENT must be owned by root:root"
SRC_PARENT_MODE=$(stat -c '%a' "$SRC_PARENT")
(( (8#$SRC_PARENT_MODE & 0022) == 0 )) || die "$SRC_PARENT must not be group/other writable"
STAGE=$(mktemp -d "$SRC_PARENT/.network-ultra-install.XXXXXX")
OLD_SRC="${SRC_DIR}.rollback.$$"
NEW_BIN="${BIN_PATH}.new.$$"
ORIGINAL_SRC_PRESENT=0
if [[ -e "$SRC_DIR" ]]; then ORIGINAL_SRC_PRESENT=1; fi
HAD_OLD_SRC=0
HAD_OLD_BIN=0
HAD_OLD_SERVICE=0
HAD_OLD_CONFIG=0
HAD_CONFIG_DIR=0
HAD_STATE_DIR=0
CREATED_USER=0
SOURCE_MOVED=0
WAS_ACTIVE=0
WAS_ENABLED=0
MUTATED=0
SUCCESS=0
rollback_install() {
  local status=$?
  trap - EXIT INT TERM
  set +e
  if [[ "$SUCCESS" -ne 1 && "$MUTATED" -eq 1 ]]; then
    systemctl stop network-ultra-server >/dev/null 2>&1 || true
    if [[ "$HAD_OLD_BIN" -eq 1 ]]; then install -m 0755 "$STAGE/old.bin" "$BIN_PATH"; else rm -f -- "$BIN_PATH"; fi
    restore_source_tree "$SRC_DIR" "$OLD_SRC" "$STAGE/src" "$ORIGINAL_SRC_PRESENT" "$SOURCE_MOVED"
    if [[ "$HAD_OLD_SERVICE" -eq 1 ]]; then install -m 0644 "$STAGE/old.service" "$SVC_FILE"; else rm -f -- "$SVC_FILE"; fi
    if [[ "$HAD_OLD_CONFIG" -eq 1 ]]; then
      rm -f -- "$CFG_FILE"
      cp --preserve=mode,ownership,timestamps -- "$STAGE/old.config" "$CFG_FILE"
    else
      rm -f -- "$CFG_FILE"
    fi
    if [[ "$HAD_CONFIG_DIR" -ne 1 ]]; then rmdir -- "$CFG_DIR" >/dev/null 2>&1 || true; fi
    systemctl daemon-reload || true
    if [[ "$WAS_ENABLED" -eq 1 ]]; then systemctl enable network-ultra-server >/dev/null 2>&1 || true; else systemctl disable network-ultra-server >/dev/null 2>&1 || true; fi
    if [[ "$WAS_ACTIVE" -eq 1 ]]; then systemctl start network-ultra-server || true; fi
    if [[ "$HAD_STATE_DIR" -ne 1 ]]; then rm -rf -- "$STATE_DIR"; fi
    if [[ "$CREATED_USER" -eq 1 ]]; then userdel "$SERVICE_USER" >/dev/null 2>&1 || true; fi
  fi
  rm -f -- "$NEW_BIN"
  rm -rf -- "$STAGE"
  exit "$status"
}
trap rollback_install EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
install -d -m 0755 "$STAGE/src"
git -C "$STAGE/src" init
git -C "$STAGE/src" remote add origin "$REPO_URL"
git -c protocol.file.allow=never -C "$STAGE/src" fetch --depth 1 origin "$SOURCE_COMMIT"
git -C "$STAGE/src" checkout --detach FETCH_HEAD
ACTUAL_COMMIT=$(git -C "$STAGE/src" rev-parse HEAD)
[[ "$ACTUAL_COMMIT" == "$SOURCE_COMMIT" ]] || die "source commit verification failed"

export GOPROXY="https://proxy.golang.org,direct"
export GOSUMDB="sum.golang.org"
export CGO_ENABLED=0
cd "$STAGE/src"
go mod download
go mod verify
go test ./...
go build -trimpath -ldflags="-s -w -X main.buildVersion=$RELEASE_VERSION" -o "$STAGE/network-ultra-server" ./cmd/server
[[ "$("$STAGE/network-ultra-server" -version)" == "network-ultra-server $RELEASE_VERSION" ]] || die "built version verification failed"
[[ -f "$STAGE/src/systemd/network-ultra-server.service" ]] || die "pinned source is missing the systemd service unit"
cd /

if [[ -d "$CFG_DIR" ]]; then HAD_CONFIG_DIR=1; fi
if [[ -d "$STATE_DIR" ]]; then HAD_STATE_DIR=1; fi
if [[ -f "$CFG_FILE" ]]; then cp --preserve=mode,ownership,timestamps -- "$CFG_FILE" "$STAGE/old.config"; HAD_OLD_CONFIG=1; fi
if [[ -f "$BIN_PATH" ]]; then cp --preserve=mode,ownership,timestamps "$BIN_PATH" "$STAGE/old.bin"; HAD_OLD_BIN=1; fi
if [[ -f "$SVC_FILE" ]]; then cp --preserve=mode,ownership,timestamps "$SVC_FILE" "$STAGE/old.service"; HAD_OLD_SERVICE=1; fi
if systemctl is-active --quiet network-ultra-server 2>/dev/null; then WAS_ACTIVE=1; fi
if systemctl is-enabled --quiet network-ultra-server 2>/dev/null; then WAS_ENABLED=1; fi

CONFIG_FOR_HEALTH="$CFG_FILE"
if [[ ! -f "$CFG_FILE" ]]; then
  SERVER_PASSWORD="${NU_SERVER_PASSWORD:-$(openssl rand -hex 16)}"
  if NU_RAW_PASSWORD="$SERVER_PASSWORD" LC_ALL=C awk 'BEGIN { exit(ENVIRON["NU_RAW_PASSWORD"] ~ /[[:cntrl:]]/ ? 0 : 1) }'; then
    die "NU_SERVER_PASSWORD must not contain control characters"
  fi
  if NU_RAW_PASSWORD="$SERVER_PASSWORD" LC_ALL=C awk 'BEGIN { exit(length(ENVIRON["NU_RAW_PASSWORD"]) > 72 ? 0 : 1) }'; then
    die "NU_SERVER_PASSWORD exceeds bcrypt's 72-byte UTF-8 limit"
  fi
  TOML_SERVER_PASSWORD="${SERVER_PASSWORD//\\/\\\\}"
  TOML_SERVER_PASSWORD="${TOML_SERVER_PASSWORD//\"/\\\"}"
  ADMIN_TOKEN=$(openssl rand -hex 32)
  CONFIG_FOR_HEALTH="$STAGE/new.config"
  cat >"$CONFIG_FOR_HEALTH" <<EOF
[server]
listen = "127.0.0.1:18900"
health_listen = "127.0.0.1:18901"
udp_listen = ""
allow_insecure_public = false
allow_insecure_udp = false
max_rooms = 50
max_peers_per_room = 8
max_connections = 200
admin_token = "$ADMIN_TOKEN"
password = "$TOML_SERVER_PASSWORD"
trusted_proxies = []

[tls]
enabled = false
cert_file = ""
key_file = ""
auto_letsencrypt = false

[log]
level = "info"
format = "json"

[ratelimit]
hello_per_ip_per_minute = 10
room_create_per_peer_per_minute = 5
room_join_per_peer_per_minute = 30
room_list_per_peer_per_minute = 60
control_per_peer_per_minute = 120
audio_frames_per_peer_per_second = 200
password_checks_concurrent = 4
EOF
  chmod 0600 "$CONFIG_FOR_HEALTH"
fi
HEALTH_URL=$("$STAGE/network-ultra-server" -config "$CONFIG_FOR_HEALTH" -print-health-url) || die "config has no valid loopback health URL"
validate_health_url "$HEALTH_URL" || die "config health URL is not a strict loopback HTTP endpoint"

[[ ! -e "$OLD_SRC" ]] || die "rollback path already exists: $OLD_SRC"
[[ ! -e "$NEW_BIN" ]] || die "staged binary path already exists: $NEW_BIN"

MUTATED=1
if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
  useradd --system --home "$STATE_DIR" --shell /usr/sbin/nologin "$SERVICE_USER"
  CREATED_USER=1
fi
install -d -o root -g "$SERVICE_USER" -m 0750 "$CFG_DIR"
if [[ ! -f "$CFG_FILE" ]]; then
  install -o root -g "$SERVICE_USER" -m 0640 "$CONFIG_FOR_HEALTH" "$CFG_FILE"
fi
chown root:"$SERVICE_USER" "$CFG_FILE"
chmod 0640 "$CFG_FILE"

# The temporary binary is not live until the final rename.
install -m 0755 "$STAGE/network-ultra-server" "$NEW_BIN"
systemctl stop network-ultra-server >/dev/null 2>&1 || true

# Defer termination during the two same-filesystem source renames. The helper
# records a pending signal and completes the atomic pair before rollback runs.
switch_source_tree "$SRC_DIR" "$STAGE/src" "$OLD_SRC"

mv -- "$NEW_BIN" "$BIN_PATH"
install -m 0644 "$SRC_DIR/systemd/network-ultra-server.service" "$SVC_FILE"
systemctl daemon-reload
systemctl enable network-ultra-server >/dev/null
systemctl restart network-ultra-server

for _ in {1..10}; do
  HEALTH_BODY=$(curl --proto '=http' --globoff --noproxy '*' --connect-timeout 1 --max-time 2 -fsS "$HEALTH_URL" 2>/dev/null || true)
  if systemctl is-active --quiet network-ultra-server \
    && [[ "$HEALTH_BODY" == *'"status":"ok"'* ]] \
    && [[ "$HEALTH_BODY" == *"\"version\":\"$RELEASE_VERSION\""* ]]; then
    SUCCESS=1
    if [[ "$HAD_OLD_SRC" -eq 1 ]]; then rm -rf -- "$OLD_SRC"; fi
    printf 'installed commit %s; secure endpoint is loopback-only until TLS/reverse proxy is configured\n' "$SOURCE_COMMIT"
    exit 0
  fi
  sleep 1
done
die "health check failed; inspect journalctl -u network-ultra-server"
