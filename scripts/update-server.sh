#!/usr/bin/env bash
# Update to an exact source commit and roll back automatically on failure.
set -euo pipefail

TEST_ROOT="${NU_UPDATE_TEST_ROOT:-}"
if [[ -n "$TEST_ROOT" ]]; then
  [[ "$EUID" -ne 0 ]] || {
    printf 'error: fixture mode is forbidden for root\n' >&2
    exit 1
  }
  [[ "$TEST_ROOT" =~ ^/tmp/network-ultra-update-fixture\.[A-Za-z0-9]+$ ]] || {
    printf 'error: NU_UPDATE_TEST_ROOT is reserved for the isolated fixture\n' >&2
    exit 1
  }
  SRC_DIR="$TEST_ROOT/opt/network-ultra-src"
  BIN_PATH="$TEST_ROOT/usr/local/bin/network-ultra-server"
  CFG_FILE="$TEST_ROOT/etc/network-ultra/config.toml"
  SVC_FILE="$TEST_ROOT/etc/systemd/system/network-ultra-server.service"
  REPO_URL="${NU_UPDATE_TEST_REPO:?NU_UPDATE_TEST_REPO is required in fixture mode}"
  FETCH_PROTOCOL_POLICY=always
else
  SRC_DIR="/opt/network-ultra-src"
  BIN_PATH="/usr/local/bin/network-ultra-server"
  CFG_FILE="/etc/network-ultra/config.toml"
  SVC_FILE="/etc/systemd/system/network-ultra-server.service"
  REPO_URL="https://github.com/GeekASMR/network-ultra-server.git"
  FETCH_PROTOCOL_POLICY=never
fi
SVC_NAME="network-ultra-server"

die() { printf 'error: %s\n' "$*" >&2; exit 1; }
if [[ -z "$TEST_ROOT" ]]; then
  [[ "$(uname -s)" == Linux ]] || die "Linux is required"
  [[ "$EUID" -eq 0 ]] || die "run with sudo/root"
  [[ -d /run/systemd/system ]] || die "systemd is required"
fi

# Serialize the entire update before reading installed state or creating a
# stage. Production uses a fixed lock under root-only /run; fixture mode uses
# the same open-file-description locking semantics through Perl because Git
# for Windows does not ship util-linux flock(1).
if [[ -n "$TEST_ROOT" ]]; then
  LOCK_DIR="$TEST_ROOT/run/network-ultra-server-update"
  mkdir -p -- "$LOCK_DIR"
  LOCK_FILE="$LOCK_DIR/update.lock"
  : >>"$LOCK_FILE"
  exec 9<>"$LOCK_FILE"
  command -v perl >/dev/null || die "Perl is required for the update fixture lock"
  perl -e 'flock(STDIN, 6) or exit 1' <&9 || die "another server operation is already running"
else
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
    # noclobber maps to an exclusive create: two first-ever invocations cannot
    # replace the pathname with different inodes and accidentally lock each one.
    ( umask 077; set -o noclobber; : >"$LOCK_FILE" ) 2>/dev/null || true
  fi
  [[ -f "$LOCK_FILE" && ! -L "$LOCK_FILE" ]] || die "$LOCK_FILE creation failed"
  [[ "$(stat -c '%u:%g:%a' "$LOCK_FILE")" == "0:0:600" ]] || die "$LOCK_FILE must be root:root mode 0600"
  exec 9<>"$LOCK_FILE"
  command -v flock >/dev/null || die "util-linux flock is required"
  flock -n 9 || die "another server operation is already running"
fi

if [[ -n "$TEST_ROOT" && -n "${NU_UPDATE_TEST_HOLD_AFTER_LOCK:-}" ]]; then
  : >"${NU_UPDATE_TEST_HOLD_AFTER_LOCK}.ready"
  while [[ ! -e "${NU_UPDATE_TEST_HOLD_AFTER_LOCK}.release" ]]; do
    /usr/bin/sleep 0.05
  done
fi

[[ -d "$SRC_DIR/.git" ]] || die "$SRC_DIR is not a git checkout"
if [[ -n "$TEST_ROOT" ]]; then
  [[ -f "$BIN_PATH" ]] || die "$BIN_PATH is not an installed file"
else
  [[ -x "$BIN_PATH" ]] || die "$BIN_PATH is not an installed executable"
fi
command -v go >/dev/null || die "Go is required"
command -v git >/dev/null || die "git is required"
command -v curl >/dev/null || die "curl is required for health checks"
command -v systemctl >/dev/null || die "systemctl is required"
[[ -z "$(git -C "$SRC_DIR" status --porcelain)" ]] || die "$SRC_DIR has local changes; refusing to overwrite them"
SOURCE_COMMIT="${NU_SOURCE_COMMIT:-}"
[[ "$SOURCE_COMMIT" =~ ^[0-9a-f]{40}$ ]] || die "NU_SOURCE_COMMIT must be an audited lowercase 40-character commit SHA"
RELEASE_VERSION="${NU_RELEASE_VERSION:-$SOURCE_COMMIT}"
[[ "$RELEASE_VERSION" =~ ^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$ ]] || die "NU_RELEASE_VERSION contains unsafe characters"

OLD_REV=$(git -C "$SRC_DIR" rev-parse HEAD)
SRC_PARENT=$(dirname -- "$SRC_DIR")
STAGE=$(mktemp -d "$SRC_PARENT/.network-ultra-update.XXXXXX")
NEW_SRC="$STAGE/src"
OLD_SRC="${SRC_DIR}.rollback.$$"
BACKUP="$STAGE/network-ultra-server.rollback"
NEW_BIN="${BIN_PATH}.new.$$"
HAD_OLD_SERVICE=0
MUTATED=0
SUCCESS=0
WAS_ACTIVE=0
WAS_ENABLED=0
if systemctl is-active --quiet "$SVC_NAME" 2>/dev/null; then WAS_ACTIVE=1; fi
if systemctl is-enabled --quiet "$SVC_NAME" 2>/dev/null; then WAS_ENABLED=1; fi
rollback_update() {
  local status=$?
  trap - EXIT INT TERM
  set +e
  if [[ "$SUCCESS" -ne 1 && "$MUTATED" -eq 1 ]]; then
    systemctl stop "$SVC_NAME" >/dev/null 2>&1 || true
    if [[ -f "$BACKUP" ]]; then install -m 0755 "$BACKUP" "$BIN_PATH" || true; fi
    if [[ "$HAD_OLD_SERVICE" -eq 1 ]]; then
      install -m 0644 "$STAGE/old.service" "$SVC_FILE" || true
    else
      rm -f -- "$SVC_FILE"
    fi
    if [[ -e "$OLD_SRC" ]]; then
      if [[ -e "$SRC_DIR" ]]; then rm -rf -- "$SRC_DIR"; fi
      mv -- "$OLD_SRC" "$SRC_DIR" || true
    fi
    systemctl daemon-reload >/dev/null 2>&1 || true
    if [[ "$WAS_ENABLED" -eq 1 ]]; then
      systemctl enable "$SVC_NAME" >/dev/null 2>&1 || true
    else
      systemctl disable "$SVC_NAME" >/dev/null 2>&1 || true
    fi
    if [[ "$WAS_ACTIVE" -eq 1 ]]; then
      systemctl start "$SVC_NAME" >/dev/null 2>&1 || true
    else
      systemctl stop "$SVC_NAME" >/dev/null 2>&1 || true
    fi
  fi
  rm -f -- "$NEW_BIN"
  rm -rf -- "$STAGE"
  exit "$status"
}
trap rollback_update EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Fetch, verify, test, and build without touching the installed checkout. The
# staging directory shares SRC_DIR's filesystem so the later source swap is a
# pair of same-volume renames.
install -d -m 0755 "$NEW_SRC"
git -C "$NEW_SRC" init
git -C "$NEW_SRC" remote add origin "$REPO_URL"
git -c "protocol.file.allow=$FETCH_PROTOCOL_POLICY" -C "$NEW_SRC" fetch --depth 1 origin "$SOURCE_COMMIT"
git -C "$NEW_SRC" checkout --detach FETCH_HEAD
[[ "$(git -C "$NEW_SRC" rev-parse HEAD)" == "$SOURCE_COMMIT" ]] || die "source commit verification failed"
[[ -f "$NEW_SRC/systemd/network-ultra-server.service" ]] || die "pinned source is missing the systemd service unit"

export GOPROXY="https://proxy.golang.org,direct"
export GOSUMDB="sum.golang.org"
export CGO_ENABLED=0
cd "$NEW_SRC"
go mod download
go mod verify
go test ./...
go build -trimpath -ldflags="-s -w -X main.buildVersion=$RELEASE_VERSION" -o "$STAGE/network-ultra-server.new" ./cmd/server
[[ "$("$STAGE/network-ultra-server.new" -version)" == "network-ultra-server $RELEASE_VERSION" ]] || die "built version verification failed"
HEALTH_URL=$("$STAGE/network-ultra-server.new" -config "$CFG_FILE" -print-health-url) || die "new config has no valid loopback health URL"
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
validate_health_url "$HEALTH_URL" || die "new config health URL is not a strict loopback HTTP endpoint"

[[ ! -e "$OLD_SRC" ]] || die "rollback path already exists: $OLD_SRC"
[[ ! -e "$NEW_BIN" ]] || die "staged binary path already exists: $NEW_BIN"
cp --preserve=mode,ownership,timestamps "$BIN_PATH" "$BACKUP"
if [[ -f "$SVC_FILE" ]]; then
  cp --preserve=mode,ownership,timestamps "$SVC_FILE" "$STAGE/old.service"
  HAD_OLD_SERVICE=1
fi
install -m 0755 "$STAGE/network-ultra-server.new" "$NEW_BIN"
MUTATED=1
systemctl stop "$SVC_NAME"
mv -- "$SRC_DIR" "$OLD_SRC"
mv -- "$NEW_SRC" "$SRC_DIR"
mv -- "$NEW_BIN" "$BIN_PATH"
install -m 0644 "$SRC_DIR/systemd/network-ultra-server.service" "$SVC_FILE"
systemctl daemon-reload
if [[ "$WAS_ENABLED" -eq 1 ]]; then
  systemctl enable "$SVC_NAME" >/dev/null
else
  systemctl disable "$SVC_NAME" >/dev/null
fi
if [[ "$WAS_ACTIVE" -eq 1 ]]; then
  systemctl start "$SVC_NAME" || die "new service failed to start; rollback requested"

  HEALTH_OK=0
  for _ in {1..10}; do
    HEALTH_BODY=$(curl --proto '=http' --globoff --noproxy '*' --connect-timeout 1 --max-time 2 -fsS "$HEALTH_URL" 2>/dev/null || true)
    if systemctl is-active --quiet "$SVC_NAME" \
      && [[ "$HEALTH_BODY" == *'"status":"ok"'* ]] \
      && [[ "$HEALTH_BODY" == *"\"version\":\"$RELEASE_VERSION\""* ]]; then
      HEALTH_OK=1
      break
    fi
    sleep 1
  done
  if [[ "$HEALTH_OK" -ne 1 ]]; then
    die "health check failed; rollback requested"
  fi
fi
SUCCESS=1
rm -rf -- "$OLD_SRC"
if [[ "$WAS_ACTIVE" -eq 1 ]]; then
  printf 'updated %s -> %s with verified health check\n' "$OLD_REV" "$SOURCE_COMMIT"
else
  printf 'updated inactive service %s -> %s; service remained stopped\n' "$OLD_REV" "$SOURCE_COMMIT"
fi
