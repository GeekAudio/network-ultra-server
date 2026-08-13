#!/usr/bin/env bash
# Network Ultra Server - 修改服务器连接密码
#
# 用法:
#   1. 交互式（推荐）:
#        sudo bash set-password.sh
#      脚本会提示你输入新密码（隐藏不回显），自动改 config + 重启服务。
#
#   2. 命令行参数:
#        sudo bash set-password.sh "newpassword"
#
#   3. 关闭密码（公开服务器，任何人可连）:
#        sudo bash set-password.sh --open
#
# 使用已安装、已验证的本地脚本，不要从可变分支或第三方代理直接 pipe 给 root:
#   sudo bash /opt/network-ultra-src/scripts/set-password.sh
#
set -euo pipefail

CFG_FILE="/etc/network-ultra/config.toml"
BIN_PATH="/usr/local/bin/network-ultra-server"
SERVICE="network-ultra-server"
TMP_CFG=""
BACKUP_CFG=""
CONFIG_REPLACED=0

rollback_password_change() {
  local status=$?
  trap - EXIT
  if [[ "$status" -ne 0 && "$CONFIG_REPLACED" -eq 1 && -n "$BACKUP_CFG" ]]; then
    cp --preserve=mode,ownership,timestamps -- "$BACKUP_CFG" "$CFG_FILE" || true
    systemctl restart "$SERVICE" >/dev/null 2>&1 || true
  fi
  if [[ -n "$TMP_CFG" ]]; then rm -f -- "$TMP_CFG"; fi
  exit "$status"
}
trap rollback_password_change EXIT

c_red()   { printf '\033[31m%s\033[0m\n' "$*"; }
c_grn()   { printf '\033[32m%s\033[0m\n' "$*"; }
c_blu()   { printf '\033[36m%s\033[0m\n' "$*"; }
die()     { c_red "$*"; exit 1; }

# 0. 前置检查
if [[ "${EUID}" -ne 0 ]]; then
  c_red "请用 root 权限执行(加 sudo)。"; exit 1
fi

command -v awk >/dev/null || { c_red "需要 awk。"; exit 1; }
command -v systemctl >/dev/null || { c_red "需要 systemctl。"; exit 1; }
command -v curl >/dev/null || { c_red "需要 curl。"; exit 1; }
command -v install >/dev/null || { c_red "需要 install。"; exit 1; }
command -v stat >/dev/null || { c_red "需要 stat。"; exit 1; }
command -v flock >/dev/null || { c_red "需要 util-linux flock。"; exit 1; }

LOCK_DIR="/run/network-ultra-server-update"
if [[ -L "$LOCK_DIR" || ( -e "$LOCK_DIR" && ! -d "$LOCK_DIR" ) ]]; then
  die "$LOCK_DIR 必须是真实目录。"
fi
if [[ ! -e "$LOCK_DIR" ]]; then
  install -d -o root -g root -m 0700 "$LOCK_DIR"
fi
[[ "$(stat -c '%u:%g:%a' "$LOCK_DIR")" == "0:0:700" ]] || die "$LOCK_DIR 必须是 root:root 且权限 0700。"
LOCK_FILE="$LOCK_DIR/update.lock"
if [[ -L "$LOCK_FILE" || ( -e "$LOCK_FILE" && ! -f "$LOCK_FILE" ) ]]; then
  die "$LOCK_FILE 必须是普通文件。"
fi
if [[ ! -e "$LOCK_FILE" ]]; then
  ( umask 077; set -o noclobber; : >"$LOCK_FILE" ) 2>/dev/null || true
fi
[[ -f "$LOCK_FILE" && ! -L "$LOCK_FILE" ]] || die "$LOCK_FILE 创建失败。"
[[ "$(stat -c '%u:%g:%a' "$LOCK_FILE")" == "0:0:600" ]] || die "$LOCK_FILE 必须是 root:root 且权限 0600。"
exec 9<>"$LOCK_FILE"
flock -n 9 || die "另一个服务器安装、更新或配置操作正在运行。"

if [[ ! -f "$CFG_FILE" ]]; then
  c_red "未找到 $CFG_FILE。请先跑 install-from-source.sh 完成初次安装。"
  exit 1
fi
[[ -x "$BIN_PATH" ]] || die "未找到可执行的 $BIN_PATH。"

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
HEALTH_URL=$("$BIN_PATH" -config "$CFG_FILE" -print-health-url) || die "配置中的健康监听地址无效。"
validate_health_url "$HEALTH_URL" || die "健康检查只允许明确的 loopback HTTP 地址。"

# 1. 解析新密码来源（参数 / 标准输入 / 交互）
NEW_PWD=""
if [[ $# -ge 1 ]]; then
  case "$1" in
    --open|-o|"")
      NEW_PWD=""
      c_blu "将关闭密码保护(公开服务器)"
      ;;
    --help|-h)
      sed -n '2,18p' "$0"
      exit 0
      ;;
    *)
      NEW_PWD="$1"
      ;;
  esac
else
  # 没参数 → 交互式输入。隐藏回显避免肩窥。
  if [[ ! -t 0 ]]; then
    c_red "未通过参数提供密码,且当前不是交互式终端(可能 stdin 来自 pipe)。"
    c_red "请改用: sudo bash set-password.sh \"新密码\"   或   sudo bash set-password.sh --open"
    exit 1
  fi
  echo ""
  c_blu "修改 Network Ultra 服务器密码"
  echo "  ① 直接回车 = 关闭密码(公开服务器,任何人可连)"
  echo "  ② 输入新密码后回车"
  echo ""
  read -r -s -p "  新密码: " NEW_PWD
  echo ""
  if [[ -n "$NEW_PWD" ]]; then
    read -r -s -p "  再输一次确认: " CONFIRM
    echo ""
    if [[ "$NEW_PWD" != "$CONFIRM" ]]; then
      c_red "两次输入不一致,已取消。"
      exit 1
    fi
  fi
fi

# TOML basic strings require quotes and backslashes to be escaped. Reject
# control characters instead of emitting a configuration the daemon cannot
# parse. Use ENVIRON below so awk -v cannot reinterpret backslash escapes.
if [[ -n "$NEW_PWD" ]] && NU_RAW_PASSWORD="$NEW_PWD" LC_ALL=C awk 'BEGIN { exit(ENVIRON["NU_RAW_PASSWORD"] ~ /[[:cntrl:]]/ ? 0 : 1) }'; then
  c_red "密码不能包含控制字符（换行、制表符等）。"
  exit 1
fi
if NU_RAW_PASSWORD="$NEW_PWD" LC_ALL=C awk 'BEGIN { exit(length(ENVIRON["NU_RAW_PASSWORD"]) > 72 ? 0 : 1) }'; then
  c_red "密码按 UTF-8 编码不能超过 72 字节（bcrypt 上限）。"
  exit 1
fi
ESCAPED_PWD="${NEW_PWD//\\/\\\\}"
ESCAPED_PWD="${ESCAPED_PWD//\"/\\\"}"

# 2. 改写 config.toml 的 password 行
#    用 awk 而不是 sed 是为了正确处理:
#      - password 含特殊字符($ \ / 等)
#      - [server] 段可能不存在 password 行(老 config 升级场景)
#      - 不动其它键
TMP_CFG="$(mktemp "$CFG_FILE.tmp.XXXXXX")"

NU_TOML_PASSWORD="$ESCAPED_PWD" awk '
  BEGIN { new_pwd = ENVIRON["NU_TOML_PASSWORD"]; in_server = 0; replaced = 0 }
  /^\[server\][[:space:]]*$/ { in_server = 1; print; next }
  /^\[/ {
    # 离开 [server] 段;若一直没遇到 password 行就在此处补一行
    if (in_server == 1 && replaced == 0) {
      print "password = \"" new_pwd "\""
      replaced = 1
    }
    in_server = 0
    print; next
  }
  {
    if (in_server == 1 && $0 ~ /^[[:space:]]*password[[:space:]]*=/) {
      print "password = \"" new_pwd "\""
      replaced = 1
      next
    }
    print
  }
  END {
    # 文件结束时若 [server] 还活着且没替换过(末尾段)
    if (in_server == 1 && replaced == 0) {
      print "password = \"" new_pwd "\""
    }
  }
' "$CFG_FILE" > "$TMP_CFG"

# 3. 校验:确认我们真的写了一行 password 进去
if ! grep -q '^password[[:space:]]*=' "$TMP_CFG"; then
  c_red "改写失败,$TMP_CFG 中未发现 password 行。原 config 未动。"
  exit 1
fi

# 4. 备份并替换
BACKUP_CFG="${CFG_FILE}.bak.$(date +%Y%m%d_%H%M%S).$$"
cp --preserve=mode,ownership,timestamps -- "$CFG_FILE" "$BACKUP_CFG"
chown --reference="$CFG_FILE" "$TMP_CFG"
chmod --reference="$CFG_FILE" "$TMP_CFG"
mv "$TMP_CFG" "$CFG_FILE"
TMP_CFG=""
CONFIG_REPLACED=1

# 5. 重启服务
echo ""
c_blu "重启 ${SERVICE}..."
systemctl restart "$SERVICE"
sleep 1

# 6. 健康检查 + 状态确认
HEALTH_OK=0
for _ in 1 2 3 4 5; do
  HEALTH_BODY=$(curl --proto '=http' --globoff --noproxy '*' --connect-timeout 1 --max-time 2 -fsS "$HEALTH_URL" 2>/dev/null || true)
  if systemctl is-active --quiet "$SERVICE" && [[ "$HEALTH_BODY" == *'"status":"ok"'* ]]; then
    HEALTH_OK=1; break
  fi
  sleep 1
done

if [[ "$HEALTH_OK" -ne 1 ]]; then
  c_red "重启后健康检查失败,请查看:journalctl -u ${SERVICE} -n 50"
  exit 1
fi

# 看 systemd 日志最后一条 password gating 状态
GATING_LINE=$(journalctl -u "$SERVICE" --since "30 seconds ago" --no-pager -o cat 2>/dev/null \
              | grep -F "password gating" | tail -1 || true)

echo ""
c_grn "✓ 服务已重启,运行正常"
if [[ -n "$NEW_PWD" ]]; then
  echo ""
  c_blu "新密码已生效。"
  echo "  请通过安全渠道把密码分发给信任的客户端使用者。"
  echo "  客户端在\"服务器密码\"栏填入此值才能连接。"
else
  echo ""
  c_blu "密码保护已关闭(公开服务器,任何人可连)"
fi
if [[ -n "$GATING_LINE" ]]; then
  echo ""
  echo "  服务端日志确认: $GATING_LINE"
fi
