# Network Ultra Server

[Network Ultra](https://github.com/GeekASMR/network-ultra-server/releases/latest) VST3 插件的中心转发服务器。让两个或多个异地音乐人在各自 DAW 中通过网络协作合奏。

```
┌──────────┐         ┌──────────────┐          ┌──────────┐
│ DAW (A)  │  wss:// │ Network      │  wss://  │  DAW (B) │
│ Network  ├────────►│ Ultra Server ├─────────►│ Network  │
│ Ultra    │◄────────┤ (this repo)  │◄─────────┤ Ultra    │
│ VST3     │         │              │          │ VST3     │
└──────────┘         └──────────────┘          └──────────┘
```

服务器是纯转发层（不解码音频、不存储数据），只负责房间编排 + 二进制帧 fan-out。客户端见 [Releases](https://github.com/GeekASMR/network-ultra-server/releases/latest)。

> **安全默认值（当前版本）**：服务只监听 `127.0.0.1:18900`，UDP 默认关闭。公网部署必须配置静态 TLS 证书或放在 TLS 反向代理后面。公网明文 WebSocket 和未加密 UDP 分别需要显式设置 `allow_insecure_public = true` / `allow_insecure_udp = true`；后者仅适合受信网络。TLS 不保护 UDP。`auto_letsencrypt` 尚未实现，启用会拒绝启动而不是降级为明文。UDP 关闭时，`welcome` 不会发送 `udpEndpoint`/`udpToken`，客户端自动留在 WebSocket binary 回退路径。反向代理地址还必须显式列入 `trusted_proxies`，否则服务会忽略所有转发头并以代理自身 IP 限流。
> 健康检查与指标端点始终是明文 HTTP，因此 `health_listen` 必须显式绑定 loopback；远程监控应通过鉴权反向代理或受控隧道。
> 启用 UDP 兼容数据面时，客户端每 5 秒发送一次已认证 ping；服务端约 15 秒未收到该绑定的有效 hello/ping/audio 就删除 UDP 路由，随后音频自动回退 WebSocket，避免失效 NAT 映射造成持续静默。

## Repo 内容索引

本仓库**同时承载服务端源码和客户端安装包发布**，[Releases](https://github.com/GeekASMR/network-ultra-server/releases) 列表里两类 artifact 用 title 前缀区分：

| 前缀 | 含义 | 安装方式 |
| --- | --- | --- |
| `[Client]` | Windows VST3 插件安装包 | 下载 `Network_Ultra_Setup.exe` 双击运行 |
| `[Server]` | 服务端预编译二进制 | 校验 tag、版本和独立取得的 SHA-256 后安装，或按下文从精确 commit 编译 |

服务端 release workflow 会为受支持平台生成带内嵌版本和 SHA-256 的制品；安装者仍须从独立可信渠道核对期望哈希。源码安装方式是先审计一个完整 40 位 commit SHA，再由 `scripts/install-from-source.sh` 从规范 GitHub 仓库拉取并复核该对象后编译；脚本不会跟随可变的 `main`，也不会关闭 Go checksum database。

客户端代码闭源在另一私有工作区维护，本仓只发布 Windows 安装包；服务端代码是这里 `cmd/` + `internal/`，MIT 协议自由二次开发。

## 2 分钟自建（小白版）

### 准备

- 一台 Linux 服务器（Ubuntu / Debian / CentOS / Alibaba / Tencent 都行）
- **最低配置 1 核 1G 内存 + 3 Mbps 上行带宽**就能跑——这台仓库的演示服务器就是这个规格
  - 1 核：纯转发不解码，CPU 几乎没压力，10 人房间也撑得住
  - 1G 内存：服务进程长期占用 < 50 MB
  - 3 Mbps 上行：双人 Opus 128k 双向约 ~256 kbps；FLAC 双向约 1.6 Mbps，2 人朝夕够用，3 人就紧张
  - 想稳跑 4 人 FLAC / 6 人 Opus → 升 5 Mbps 上行
- 推荐使用域名 + TLS 反向代理，只公开 `443/TCP`；服务本身保持 loopback。
- `18902/UDP` 是未加密的兼容数据面，默认关闭。只有受信网络明确接受风险后才开放。

### 一键安装

先从可信渠道取得并审计完整 commit，然后在已检出的仓库内运行：

```bash
export NU_SOURCE_COMMIT=<40位完整commit>
sudo -E bash scripts/install-from-source.sh
```

Go 1.22+ 必须预先通过系统管理员信任的软件源安装。脚本会：持有全局运维锁 → 在 `/opt` 同卷私有目录只取指定 commit → 启用 `sum.golang.org` → `go mod verify` → 测试/编译 → 生成安全配置 → 以 rename 事务切换完整源码树 → 注册非 root systemd 服务 → 按配置的 loopback `health_listen` 健康检查。失败或收到 TERM 时恢复旧源码、二进制、配置和服务状态；脚本不会获取或执行未钉扎的第三方代理代码。

成功的标志是健康检查通过。初始端点仅在 `127.0.0.1:18900`，完成静态 TLS 或反向代理配置后再从公网连接。

> **🔒 关于服务器密码（v1.3+）**
>
> 首次安装会使用 `NU_SERVER_PASSWORD`，未提供时生成强随机服务器连接密码。所有连进来的 VST 客户端必须填这个密码才能加入。
>
> 建议在执行前显式设置 `NU_SERVER_PASSWORD="..."` 并通过安全渠道分发。配置文件仅允许 root 和专用 `network-ultra` 服务组读取。不要通过 `curl | sudo bash` 执行可变分支脚本。

### 验证从外网能连

在你自己电脑（不是服务器上）打开 PowerShell 或终端：

```powershell
# Windows PowerShell
Test-NetConnection -ComputerName <你的TLS域名> -Port 443
# 看 TcpTestSucceeded 是否为 True
```

```bash
# Linux / macOS
nc -zv <你的TLS域名> 443
# 看是否输出 "succeeded"
```

如果不通，看下面 [常见坑](#常见坑) 排查。

### 在 VST 插件里使用

完成 TLS 配置后，打开 DAW，挂载 Network Ultra 插件，服务器地址填：

```
wss://<你的域名>
```

输入用户名 → 连接 → 创建/加入房间。

> **🔒 服务器要求密码时**
>
> 客户端 v1.3+ 第一次连接如果服务器启用了密码，会**自动弹窗**让你输入。输入正确后会本地加密保存（`%APPDATA%\Network Ultra\secrets.bin`，DPAPI 加密绑当前 Windows 用户），下次开 DAW 自动填入不用再输。
>
> 输错时弹窗会提示"服务器密码错误"，本地缓存会被清掉，让你重输一次。
>
> 老客户端 v1.2.1 及更早版本不支持服务器密码——服务器会拒绝它们的连接，请升级到 v1.3+ 安装包。

---

## 常见坑

### Q1：脚本卡在 `git clone` 报 `fatal: Unable to read current working directory`

**原因**：你 SSH 进来后正好 cd 到了 `/opt/network-ultra-src`，脚本第三步 rm 这个目录后当前 shell 失去 cwd。

**解决**：
```bash
cd /tmp
cd <已审计的仓库目录>
sudo -E bash scripts/install-from-source.sh
```

最新版脚本已修，但这个习惯始终保留更稳。

### Q2：服务器访问 GitHub 慢

不要改成跟随 `main` 的第三方代理脚本。由管理员配置系统 HTTPS 代理，仍然从规范 GitHub 仓库获取，并保持 `NU_SOURCE_COMMIT` 为已审计的完整 SHA。脚本会在编译前复核最终 `HEAD`；无法精确取得目标对象时会停止，保留旧版本。

### Q3：装完了，`ss -tlnp | grep 18900` 显示在监听，但外网连不上

这是安全默认值：服务只绑定 loopback。先配置静态 TLS 或 TLS 反向代理，再只放通所需的 TLS 端口。不要 flush 整个防火墙。UDP 仅在受信网络显式打开 `allow_insecure_udp` 后按最小范围放通。

### Q4：服务起不来 / `journalctl` 报错

```bash
journalctl -u network-ultra-server -n 50 --no-pager
```

最常见的：端口被其他进程占了。`ss -tlnp | grep 18900` 看是谁在用。

### Q5：怎么彻底卸载

```bash
sudo systemctl disable --now network-ultra-server
sudo rm -f /etc/systemd/system/network-ultra-server.service
sudo rm -f /usr/local/bin/network-ultra-server
sudo rm -rf /etc/network-ultra /opt/network-ultra-src
sudo systemctl daemon-reload
```

### Q6：怎么改服务器密码

跑这条一行命令（v1.3+）：

```bash
sudo bash /opt/network-ultra-src/scripts/set-password.sh
```

会弹两次密码提示（第二次确认避免输错），自动写入 `/etc/network-ultra/config.toml` + 重启服务 + 健康检查。脚本不会把密码回显到日志或终端。

也可以一次性命令式（无交互）：

```bash
# 一行设密码
sudo bash /opt/network-ultra-src/scripts/set-password.sh "你的新密码"

# 一行关密码（让服务器变成公开的）
sudo bash /opt/network-ultra-src/scripts/set-password.sh --open
```

改完后，新密码会用于所有后续连接；已经完成认证的现有连接会持续到其断开。应先通过安全渠道分发新密码，再安排客户端重连。

### Q7：忘了服务器密码

直接 SSH 上服务器：

```bash
sudo grep '^password' /etc/network-ultra/config.toml
```

会输出 `password = "xxx"`，明文存放（config.toml 是 root 0640 权限，普通用户读不到）。

如果连 SSH 都丢了，那就只能直接登腾讯云/阿里云控制台 web shell 上去看，或者重跑安装脚本生成新密码（旧 config 会被保留覆盖，重新设置）。

### Q8：手动改 config 不重启服务，密码会立刻生效吗

不会。服务器启动时把配置里的密码用 bcrypt 哈希一次塞内存里，运行期不重读。改完 `password` 字段必须 `sudo systemctl restart network-ultra-server`。建议直接用上面 Q6 的脚本，自带重启。

---

## 配置文件

`/etc/network-ultra/config.toml`：

```toml
[server]
listen = "127.0.0.1:18900"            # 默认仅本机，交给 TLS 反向代理
health_listen = "127.0.0.1:18901"     # 健康检查 + 指标（仅本地）
udp_listen = ""                       # 默认禁用未加密 UDP
udp_advertise_host = ""
allow_insecure_public = false
allow_insecure_udp = false
max_rooms = 50
max_peers_per_room = 8                # 协议 v1 固定支持 1..8；超出会拒绝启动
max_connections = 200
admin_token = "<安装脚本自动生成>"
password = "<安装脚本引导你设置;留空=公开服务器>"   # v1.3+
trusted_proxies = []                  # 仅填写实际连接本服务的代理 IP/CIDR

[tls]
enabled = false                       # loopback 后接 TLS 反向代理
cert_file = ""
key_file = ""
auto_letsencrypt = false              # 当前未实现；true 会拒绝启动
domain = ""
email = ""

[log]
level = "info"
format = "json"
path = ""                             # 空 = stdout（systemd 自动收集）

[ratelimit]
hello_per_ip_per_minute = 10
room_create_per_peer_per_minute = 5
room_join_per_peer_per_minute = 30
room_list_per_peer_per_minute = 60
control_per_peer_per_minute = 120
audio_frames_per_peer_per_second = 200
password_checks_concurrent = 4
```

协议 v1 输入边界：用户名去除首尾空白后为 1..32 个 Unicode 字符，房间名为 1..64 个 Unicode 字符，二者必须是有效 UTF-8 且不能含控制字符；服务器密码和房间密码按 UTF-8 编码最多 72 字节（bcrypt 的输入边界）；一次 `subscribe` 最多包含 8 个互不重复的合法 UUID。超界请求会返回明确的协议错误，不会进入 bcrypt 或房间列表。

修改配置后：`sudo systemctl restart network-ultra-server`

安装、源码安装、升级和改密脚本共享 `/run/network-ultra-server-update/update.lock`，并会在读取/修改已安装状态前非阻塞加锁；请等待正在执行的运维操作结束，不要删除锁文件规避互斥。脚本从已验证配置生成健康 URL，支持 loopback IPv4/IPv6 和自定义端口，拒绝公网地址、主机名及带路径/命令片段的输入。

## TLS 模式

| 配置 | 模式 | 适用场景 |
| --- | --- | --- |
| `enabled = false` + loopback | 由反向代理终止 TLS | 推荐公网部署 |
| `enabled = true` + `cert_file` + `key_file` | 静态证书 | 已有 SSL |

> Auto-LE 当前未实现；配置为 true 会 fail closed。不要把 TLS 误认为 UDP 加密。

TLS 反向代理模式还要把**实际连接服务端的代理源地址**加入 `[server]`，例如代理通过 IPv4 loopback 连接时：

```toml
trusted_proxies = ["127.0.0.1/32"]
```

只有直连来源命中该列表时，服务端才读取标准 `Forwarded` 或 `X-Forwarded-For`，并从右向左跳过可信代理、选取最近的不可信客户端 hop。代理必须覆盖或规范追加这些头；不要填写 `0.0.0.0/0`、`::/0` 或比实际代理更宽的网段（配置会拒绝两个全网段）。未配置时所有转发头都被忽略，这是安全默认值，但同一反向代理后的连接会共享 IP 限流预算。

## 端点

- `ws://127.0.0.1:18900` — 默认本机控制 + 音频回退通道
- `udp://0.0.0.0:18902` — 仅在明确接受未加密风险并启用后出现
- `http://127.0.0.1:18901/healthz` — JSON 状态
- `http://127.0.0.1:18901/metrics` — Prometheus 指标

## 常用运维

```bash
systemctl status network-ultra-server
journalctl -u network-ultra-server -f
systemctl restart network-ultra-server
curl http://127.0.0.1:18901/healthz
curl http://127.0.0.1:18901/metrics
```

## 协议

- **控制消息**：WebSocket text frame，JSON UTF-8
  - `hello` / `welcome` / `room_create` / `room_join` / `room_left` / `peer_*` / `ping` / `pong` / `error`
  - UDP 启用时 `welcome` 携带 `udpEndpoint` + `udpToken`；禁用时两个字段均省略
- **音频消息**：WebSocket binary frame，或受信网络显式启用的 UDP datagram
  - 24 字节定长 header（type + codec_id + sourcePeerId 16B + seq 2B + length 2B）+ payload
  - codec_id：0=PCM / 1=FLAC / 2=Opus 192k / 3=Opus 128k(默认) / 4=Opus 64k
  - PCM 一帧 1920 字节 + 24 头 ~ 3 Mbps，Opus 128k 一帧 ~256 字节，FLAC 一帧 ~700 字节
- **UDP 数据面（v1.2+）**：客户端先 WS 握手取 token，然后用 token 在 UDP 18902 上 hello。
  token 是每个 WS 会话的随机值；服务器首次绑定 source addr 后拒绝用同一 token 跨地址重绑定。
  握手失败时自动回落到 WebSocket binary frame（兼容老服务器和受限网络）。

服务器对 payload 完全不感知，只检查长度并 fan-out 给同房间其他 peer。

## License

服务器（本仓库）：MIT — 见 [LICENSE](./LICENSE)。

客户端 VST3 插件：闭源商业产品，不接受 PR / 源码请求。仅在 [Releases](https://github.com/GeekASMR/network-ultra-server/releases/latest) 提供 Windows 安装包。
