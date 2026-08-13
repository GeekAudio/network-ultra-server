#!/usr/bin/env bash
# Install a pinned prebuilt release. The release checksum is mandatory.
set -euo pipefail

REPO="GeekASMR/network-ultra-server"
BIN_PATH="/usr/local/bin/network-ultra-server"
CFG_DIR="/etc/network-ultra"
CFG_FILE="$CFG_DIR/config.toml"
SVC_FILE="/etc/systemd/system/network-ultra-server.service"
SERVICE_USER="network-ultra"
STATE_DIR="/var/lib/network-ultra"

die() { printf 'error: %s\n' "$*" >&2; exit 1; }
[[ "$(uname -s)" == Linux ]] || die "Linux is required"
[[ "$EUID" -eq 0 ]] || die "run with sudo/root"
[[ -d /run/systemd/system ]] || die "systemd is required"
command -v openssl >/dev/null || die "openssl is required for secure credentials"
command -v curl >/dev/null || die "curl is required"
command -v sha256sum >/dev/null || die "sha256sum is required"
command -v systemctl >/dev/null || die "systemctl is required"
command -v useradd >/dev/null || die "useradd is required"
command -v userdel >/dev/null || die "userdel is required for rollback"
command -v install >/dev/null || die "install is required"
command -v stat >/dev/null || die "stat is required"
command -v flock >/dev/null || die "util-linux flock is required"

# Every script that mutates the installed server holds this same fixed,
# root-owned lock for its complete lifetime. Validate both path components
# before opening the descriptor so an unprivileged process cannot pre-place a
# symlink or swap the first lock inode during concurrent first use.
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
TAG="${NU_RELEASE_TAG:-}"
TAG_RE='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
[[ "$TAG" =~ $TAG_RE ]] || die "NU_RELEASE_TAG must pin an exact canonical semver tag (for example v1.4.0); latest is forbidden"
EXPECTED_SHA256="${NU_EXPECTED_SHA256:-}"
[[ "$EXPECTED_SHA256" =~ ^[0-9a-fA-F]{64}$ ]] || die "NU_EXPECTED_SHA256 must be the independently verified release hash"
case "$(uname -m)" in
  x86_64) TARGET=linux-amd64 ;;
  aarch64) TARGET=linux-arm64 ;;
  *) die "unsupported CPU architecture" ;;
esac

TMP=$(mktemp -d /tmp/network-ultra-release.XXXXXX)
HAD_OLD_BIN=0
HAD_OLD_SERVICE=0
HAD_OLD_CONFIG=0
HAD_CONFIG_DIR=0
HAD_STATE_DIR=0
CREATED_USER=0
WAS_ACTIVE=0
WAS_ENABLED=0
MUTATED=0
SUCCESS=0
rollback_release_install() {
  local status=$?
  if [[ "$SUCCESS" -ne 1 && "$MUTATED" -eq 1 ]]; then
    systemctl stop network-ultra-server >/dev/null 2>&1 || true
    if [[ "$HAD_OLD_BIN" -eq 1 ]]; then install -m 0755 "$TMP/old.bin" "$BIN_PATH"; else rm -f -- "$BIN_PATH"; fi
    if [[ "$HAD_OLD_SERVICE" -eq 1 ]]; then install -m 0644 "$TMP/old.service" "$SVC_FILE"; else rm -f -- "$SVC_FILE"; fi
    if [[ "$HAD_OLD_CONFIG" -eq 1 ]]; then
      rm -f -- "$CFG_FILE"
      cp --preserve=mode,ownership,timestamps -- "$TMP/old.config" "$CFG_FILE"
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
  rm -rf -- "$TMP"
  exit "$status"
}
trap rollback_release_install EXIT
BASE="https://github.com/$REPO/releases/download/$TAG/network-ultra-server-$TARGET"
curl --proto '=https' --tlsv1.2 -fsSL "$BASE" -o "$TMP/server"
curl --proto '=https' --tlsv1.2 -fsSL "$BASE.sha256" -o "$TMP/server.sha256"
(cd "$TMP" && printf '%s  server\n' "$(awk 'NR==1 {print $1}' server.sha256)" | sha256sum -c -)
ACTUAL_SHA256=$(sha256sum "$TMP/server" | awk '{print $1}')
[[ "${ACTUAL_SHA256,,}" == "${EXPECTED_SHA256,,}" ]] || die "binary does not match NU_EXPECTED_SHA256"
chmod 0755 "$TMP/server"
EXPECTED_VERSION="${TAG#v}"
[[ "$("$TMP/server" -version)" == "network-ultra-server $EXPECTED_VERSION" ]] || die "binary version does not match pinned tag"

# Build a prospective first-install config in the private stage. This lets the
# new binary validate the exact health endpoint before any installed file or
# service state is changed.
CONFIG_FOR_HEALTH="$CFG_FILE"
if [[ ! -f "$CFG_FILE" ]]; then
  PASSWORD=$(openssl rand -hex 16)
  ADMIN_TOKEN=$(openssl rand -hex 32)
  CONFIG_FOR_HEALTH="$TMP/new.config"
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
password = "$PASSWORD"
trusted_proxies = []

[tls]
enabled = false
auto_letsencrypt = false

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
HEALTH_URL=$("$TMP/server" -config "$CONFIG_FOR_HEALTH" -print-health-url) || die "config has no valid loopback health URL"
validate_health_url "$HEALTH_URL" || die "config health URL is not a strict loopback HTTP endpoint"

if [[ -d "$CFG_DIR" ]]; then HAD_CONFIG_DIR=1; fi
if [[ -d "$STATE_DIR" ]]; then HAD_STATE_DIR=1; fi
if [[ -f "$CFG_FILE" ]]; then cp --preserve=mode,ownership,timestamps -- "$CFG_FILE" "$TMP/old.config"; HAD_OLD_CONFIG=1; fi
if [[ -f "$BIN_PATH" ]]; then cp --preserve=mode,ownership,timestamps "$BIN_PATH" "$TMP/old.bin"; HAD_OLD_BIN=1; fi
if [[ -f "$SVC_FILE" ]]; then cp --preserve=mode,ownership,timestamps "$SVC_FILE" "$TMP/old.service"; HAD_OLD_SERVICE=1; fi
if systemctl is-active --quiet network-ultra-server 2>/dev/null; then WAS_ACTIVE=1; fi
if systemctl is-enabled --quiet network-ultra-server 2>/dev/null; then WAS_ENABLED=1; fi
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
install -m 0755 "$TMP/server" "$BIN_PATH"
cat >"$SVC_FILE" <<'EOF'
[Unit]
Description=Network Ultra Audio Server
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
User=network-ultra
Group=network-ultra
ExecStart=/usr/local/bin/network-ultra-server -config /etc/network-ultra/config.toml
Restart=on-failure
RestartSec=5
LimitNOFILE=65536
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
ReadOnlyPaths=/etc/network-ultra
StateDirectory=network-ultra
StateDirectoryMode=0700
[Install]
WantedBy=multi-user.target
EOF
chmod 0644 "$SVC_FILE"
systemctl daemon-reload
systemctl enable network-ultra-server >/dev/null
systemctl restart network-ultra-server
for _ in {1..10}; do
  HEALTH_BODY=$(curl --proto '=http' --globoff --noproxy '*' --connect-timeout 1 --max-time 2 -fsS "$HEALTH_URL" 2>/dev/null || true)
  if systemctl is-active --quiet network-ultra-server \
    && [[ "$HEALTH_BODY" == *'"status":"ok"'* ]] \
    && [[ "$HEALTH_BODY" == *"\"version\":\"$EXPECTED_VERSION\""* ]]; then
    SUCCESS=1
    printf 'installed pinned release %s\n' "$TAG"
    exit 0
  fi
  sleep 1
done
die "health check failed"
