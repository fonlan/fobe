# fobe — 探针面板设计方案 v1

> 单用户、单端口、Docker Compose 自托管的多探针管理面板：集成 sing-box 服务端生命周期、流量与硬件监控、延迟测量、Web 终端、AI 助手、订阅输出。
>
> **接入层不在本仓库范围内**：反向代理、TLS 终止与证书签发/续期由你自备的 nginx 完成，fobe 只监听一个普通 HTTP 端口。nginx 该怎么配见 `README.md`。
>
> 本文档由 `/grill-me` 四轮盘问收敛而成，**每条决定后面都标了它绑定的代价**。标 ⚠ 的是已接受但仍需你知道的风险。

---

## 0. 关键决策速览

| # | 决策 | 选择 | 绑定的代价 |
|---|---|---|---|
| 1 | 产品闭环 | 面板输出订阅 `/sub/<token>`，sing-box + Clash 双格式 | 需要订阅渲染 + token 生命周期 |
| 2 | 网络通道 | 外部 nginx 单端口接入，全路径转单一上游；agent 走 WSS | 你必须自备 nginx + 真证书；探针必须能解析面板域名 |
| 3 | 流量口径 | 整机网卡 `/proc/net/dev`，agent 自动探测默认路由出口并上报全部网卡；面板可选其一 | 配额含系统更新与其他服务流量；需计数器回绕/重启检测 |
| 4 | 使用率语义 | 分母 = 周期配额；4 模式决定取哪个方向 | 每节点必须落库：配额值、流量周期类型、下次重置时间；时区以 agent 上报为准 |
| 5 | Web 终端 | 浏览器 → server → agent 本地 PTY（不依赖 sshd、不存凭据）（2026-09-15 修订：原 SSH + 凭据托管方案废弃） | agent 需要 PTY 权限；协议保留 `mode` 字段仅为滚动升级兼容 |
| 6 | 平台 | x86 Linux + x86 OpenWrt（v1 硬需求） | procd/init.d、musl 静态、flash 写最小化 |
| 7 | 到期/超量 | 只提醒，不自动停服 | 需要告警通道，且没有自动止损 |
| 8 | AI 模型 | OpenAI 兼容 API，自填 base_url + key，服务端加密存储 | key 在服务端；服务端需能出网 |
| 9 | AI 执行权 ⚠ | **默认放行**：AI 自评有风险才弹确认 | 见 §12，间接提示注入可静默拿到探针 root |
| 10 | 构建形态 | server / agent 分开编译；前端**不嵌入 Go 二进制**但**编进生产镜像**；agent 产物进服务端镜像 | 共享 protocol 模块 + 产物分发链路；镜像构建多一个前端 stage；**挂载卷会遮蔽镜像内的 agent 产物，服务端启动时得把它补进卷**（§5.5） |
| 11 | 数据库 | SQLite（WAL），文件挂载在容器外 | 单写者；指标靠保留期控盘 |
| 12 | 订阅模型 | 单用户 + 多订阅，每订阅独立节点集与模板文件 | 模板管理 + 双格式引擎 |
| 13 | 探针凭据 | 一个入站 + 一个**全局共享** anytls 密码 | 无法按订阅吊销代理访问，只能全局轮换 |
| 14 | 流量重置 | 独立周期类型（无 / 按月 / 按年）+ 下次重置时间；填一次自动滚动 | 需处理月末、闰日边界、秒级时间与探针时区 |
| 15 | 缴费周期 | 周期类型（无 / 按月 / 按天 / 按年，2026-09-15 增按年）+ 周期长度 + 下次到期日，手动改；**周期长度单位随类型（天/月/年），类型只作记账口径、不参与任何到期计算**；**无续费按钮、无历史** | 查不到"上期什么时候交的" |
| 16 | 指标保留 | 明细只存 7 天；另存永久「按天流量」表 | 7 天以外的曲线不可得（月曲线靠日表） |
| 17 | 延迟测量 | 探针**主动**测面板配置的目标；本地测量频率由设置项 `latency.interval_seconds` 控制（默认 5s）、60s 批量上报；ICMP + TCP 握手两种 | 拿不到"用户→探针"的真实延迟 |
| 18 | 登录加固 | 失败 3 次拉黑 IP（持久化）+ CLI 解封；**不做 2FA** | 黑名单依赖 XFF 信任链；无第二因子 |
| 19 | 告警 | Telegram Bot + 通用 Webhook + **飞书**（2026-09-16 增：应用机器人 / 群自定义机器人） | 一条告警要么进 Telegram、要么进飞书或你自己的 Webhook——通道各自独立，配一个是一个 |
| 20 | 接入层 | **不在 fobe 内实现**：外部 nginx 提供 TLS 与反代，证书自备自管 | fobe 不碰证书；你必须让 nginx 传对 XFF 与 Upgrade 头（见 §3 与 README） |
| 21 | agent 自更新 | **一直跟随服务端**：版本不一致就切（含降级），agent 自发、免确认；Kill Switch on 时冻结（§5.5） | 服务端版本号成为对外契约；不保留 `.prev` ⇒ **没有本地回滚**；存量探针必须人工重装一次才进入自动跟随 |

---

## 1. 范围

**v1 做**：单用户面板；探针注册与心跳；硬件/网络指标；流量配额与四种口径；缴费与重置周期；sing-box 安装/更新/回滚/启停；anytls 自签证书服务端；订阅渲染（sing-box / Clash 模板）；Web 终端；延迟测量；IP 与国旗；Telegram/Webhook/飞书 告警；中英双语；明暗主题；AI 助手；**外部接入文档（README 给出可直接复制的 nginx 配置）**。

**v1 不做**：**反向代理与证书签发/续期（由你自备的外部 nginx 负责，fobe 内不含任何反代组件）**；多用户与权限体系；在线支付；到期自动停服；TOTP；非 x86 架构；除 anytls 以外的协议；探针集群编排；Prometheus 导出。

---

## 2. 系统架构

```
        外部 nginx（不在本仓库，你自备；配置见 README.md）
                 ┌──────────────── 公网 :443（唯一暴露端口）────────────────┐
  浏览器 ────────►│ TLS 终止 + 证书签发续期（你自己管）                       │
                 │ location / → 单一上游 http://127.0.0.1:8080（原样透传）   │
                 └───────────────────────────────┬───────────────────────────┘
                                                 │
                                        ┌────────▼─────────┐
                                        │  server (Go)     │  SQLite(WAL) 挂载在 /data
                                        │  ├ /api  /ws/*   │  AI key 等敏感设置 AES-GCM 加密
                                        │  ├ /sub /install │
                                        │  ├ /dl → /data/dl│  agent 与 sing-box 产物直出
                                        │  ├ /   → /srv/web│  前端 dist（镜像内直出）
                                        │  + scheduler     │
                                        │  + CLI admin     │
                                        └────────┬─────────┘
                                                 │ WSS（探针主动外连，无需开放入站）
        ┌────────────────────────────────────────┼───────────────────────────────────┐
        │                                        │                                   │
   ┌────▼─────┐                            ┌─────▼────┐                        ┌─────▼────┐
   │ probe A  │                            │ probe B  │                        │ probe C  │
   │ fobe-agent (root)                      │ ...      │                        │ x86 OpenWrt │
   │ ├ 指标采集 /proc                        └──────────┘                        └──────────┘
   │ ├ sing-box 生命周期 + 健康闸门
   │ ├ 自签证书生成（私钥不出探针）
   │ ├ 延迟测量（ICMP / TCP）
   │ └ SSH 客户端 + 本地 PTY
   └──────────┘
```

要点：

- **探针永不监听控制端口**，只主动外连面板 → 不需要在探针上开管理端口，NAT 后面也能用。
- **单端口**：所有控制面流量都是普通 HTTPS/WSS 的路径，外部 nginx 只需一个 `location /` 透传，不需要 stream 模块做协议嗅探。
- **server 只绑定回环端口**（`127.0.0.1:8080`），对外唯一入口是你自备的 nginx；fobe 仓库内不含任何反代组件，也不生成/管理证书。**（实现修订 2026-09-15：compose 默认端口映射改为 `8080:8080`——面板明文 HTTP 直达宿主机全接口；要回到"nginx 唯一入口"的形态，把 compose 的 ports 改回 `127.0.0.1:8080:8080` 或用防火墙限制来源。容器内 `FOBE_LISTEN` 恒为 `0.0.0.0:8080`，绑定收敛只在端口映射层做。）**
- 前端编译产物**在镜像构建阶段打进镜像**（`/srv/web`）由 **server 直出**（不嵌入 Go 二进制，避免体积膨胀；也不做宿主机挂载）。

---

## 3. 接入层（外部 nginx，不在本仓库范围内）

fobe **不实现**反向代理，也**不做**证书签发与续期。它只做一件事：监听一个普通 HTTP 端口（默认 `8080`），把上面列出的全部路径都当作自己的职责——静态前端、订阅、产物下载、WebSocket 全由 server 自己处理。

**外部 nginx 必须满足的四件事**（完整可复制配置与逐条解释见 `README.md`）：

1. **单端口承接**：一个 `server` 块（同一域名、同一 `443`）收下全部流量，`location /` 原样透传到 `http://127.0.0.1:8080`。路径不需要任何分发规则。
2. **WebSocket 升级**：`proxy_http_version 1.1` + `Upgrade`/`Connection` 头 + `proxy_read_timeout 3600s`。少任何一条，探针长连接或浏览器终端就连不上（表现是登录正常、节点永远离线）。
3. **真实 IP 传递**：`Host` / `X-Real-IP` / `X-Forwarded-For` / `X-Forwarded-Proto`。传错的话，黑名单会封错对象，安装命令与订阅里生成的域名也会错。
4. **体量与缓存**：`client_max_body_size` 放宽（模板上传），`/sub/` 关闭缓存（否则客户端换了订阅还是旧节点）。

**信任链**：server 只信任 `FOBE_TRUSTED_PROXIES`（默认 `127.0.0.1/32,::1/128,172.16.0.0/12`）范围的上游所传的 `X-Forwarded-For`，并且只取最左侧地址；上游不在白名单内时**忽略 XFF，改用 socket 源地址**。若你日后在前面再叠一层 CDN/反代（Cloudflare 等），必须把它的回源网段加进来。

**替代做法（可选）**：若你想让 nginx 直接吐静态文件，需自己产出前端文件（`cd web && npm run build`，或 `docker cp <容器>:/srv/web ./dist`），再把 `location /` 指向该目录，但**必须保留** `/api`、`/ws/`、`/sub/`、`/install.sh`、`/dl/` 转发到 server。默认推荐前者：配置最短，且不存在"静态文件与 API 走不同域名导致 Cookie/WS 跨域"的坑。

---

## 4. 认证与安全模型

### 4.1 面板

- 单用户，密码 `argon2id` 存库。
- 会话：HttpOnly + Secure + SameSite=Lax Cookie，服务端存 session 表，可一键吊销全部。
- **不做 2FA**（你的选择）。补偿：登录失败黑名单（见 4.3）+ 强制 HTTPS + 绑域名。
- CLI 逃生口（进容器执行，不依赖 Web）：
  - `fobe-server admin unblock <ip|all>` 解封
  - `fobe-server admin reset-password`
  - `fobe-server admin list-sessions --revoke`
  - `fobe-server admin kill-switch on|off`（冻结所有 AI 执行）

### 4.2 探针凭证

- **服务端访问网址**：设置项 `server.public_url`，由用户填写探针可达的完整 `http(s)` 地址；添加节点生成的安装命令和 `/install.sh` 默认值优先使用它，未配置时才回退当前请求的 `Host`。
- **注册 token**：面板添加节点时生成，**节点名称必填**、单次有效、TTL 30 分钟；token 绑定节点名称与可选备注，探针注册后名称写入 `nodes.name`。
- 安装脚本用注册 token 调 `POST /api/agent/register`，服务端返回 `node_id` + `node_secret`（只此一次明文下发，服务端只存 hash）。
- 之后 agent 用 `node_id + node_secret` 建立 WSS，服务端按 `machine_id` 去重：
  - 已有同一 `machine_id` 且凭证校验通过 → **复用节点**，更新 IP/版本，不产生垃圾节点；
  - 已存在但凭证不符 → 拒绝注册，面板显示"疑似重复安装"，由你手动选择接管。
- agent 侧默认落盘：`/etc/fobe-agent/{config.json,machine-id,update-state.json}`（0600，root）。OpenWrt 上 `/etc` 是 overlay，重启保留。**实现修订 2026-09-16（非特权模式）**：`machine-id` 与 `update-state.json` 一律从 `-config` 所在目录派生；因此 `--unprivileged` 的 `/opt/fobe-agent/config.json` 会把全部探针本地状态收进同一个用户可写目录。

### 4.3 登录失败黑名单

- 连续 3 次失败 → 拉黑该 IP；持久化到 SQLite（重启不丢）。
- 真实 IP 来源：`FOBE_TRUSTED_PROXIES` 白名单内上游传来的 `X-Forwarded-For` 最左侧地址；上游不在白名单内 → **忽略 XFF，改用 socket 源地址**（防伪造头绕过或嫁祸）。
- **防自锁**（三条硬规则）：
  1. 回环与私有网段（含 Docker 网段、`X-Forwarded-For` 里的内网地址）**永不加黑**；
  2. 上游代理地址与 `FOBE_TRUSTED_PROXIES` 网段永不加入黑名单；
  3. 黑名单表带 `expires_at`，CLI 可无条件清空。
- 面板提供"当前封禁列表 + 一键解封"页面，不逼你非进容器不可（CLI 是兜底）。

### 4.4 密钥托管

- 主密钥 `FOBE_MASTER_KEY`（32 字节，环境变量 / Docker secret）。用它 AES-GCM 加密：AI API Key、Telegram Bot Token、订阅模板中的敏感段。（2026-09-15：不再加密任何 SSH 凭据——Web 终端已改走 agent 本地 PTY，见 §11。）
- 未设置主密钥时，服务端**拒绝启动**并打印生成命令（不静默降级为明文）。**（实现修订 2026-09-15：镜像入口脚本 `deploy/docker-entrypoint.sh` 在「未设置」时先行兜底，优先级 env > `FOBE_MASTER_KEY_FILE` > 生成随机 32 字节落盘到数据卷 `/data/.master_key`（0600，重启复用）。服务端的 fail-closed 语义不变——生成失败（如 `/data` 不可写）容器直接退出，绝无明文回退；admin CLI 跳过密钥解析，逃生口永不被堵。代价是密钥与密文同卷，见 §20.12；要分开就显式设置 `FOBE_MASTER_KEY`。）**
- 所有审计写 `audit_logs`：谁、何时、对哪个节点、什么动作、命令原文、来源 IP。

---

## 5. 探针 agent

### 5.1 形态

- 单个静态二进制（`CGO_ENABLED=0`，`linux/amd64`，musl 兼容）——**OpenWrt 与常规发行版同一份产物**。
- **不调用外部命令做采集**：CPU/内存/磁盘/网络全部读 `/proc`、`/sys`、`statfs`。原因：OpenWrt 是 busybox，字段与工具集都可能缺。
- 默认 root 安装（安装 systemd unit/procd 服务、写 `/etc`、ICMP raw socket）；常规 systemd Linux 可选「一次性 root 引导、日常非 root」模式（§5.3）。ICMP 依次尝试 raw socket（root / `CAP_NET_RAW`）与内核 ping socket（`SOCK_DGRAM`，受 `ping_group_range` 控制）；两者皆不可用时才降级 TCP-only，并上报能力位。

### 5.2 安装流程

面板"添加节点" → 生成注册 token → 给出一条命令：

```sh
curl -fsSL https://panel.example.com/install.sh | bash -s -- --token <REGTOKEN> --server https://panel.example.com
```

> 命令不带 `sudo`：脚本以当前用户运行，root 直接安装；非 root 且装有 sudo 时自动提权；两者都不满足时明确报错（多数纯净 VPS 的 root 用户没有 sudo）。

`install.sh` 由 server 动态渲染（注入 server URL 与 token），脚本做：

1. 探测架构（v1 只接受 `x86_64`，否则明确报错退出，不做半吊子兼容）；
2. 从 `https://panel.example.com/dl/agent/<version>/linux-amd64` 下载二进制，**校验 sha256**（校验值由脚本内嵌，来自服务端版本清单）；
3. 安装到 `/usr/local/bin/fobe-agent`（OpenWrt 用 `/usr/bin/fobe-agent`）；
4. 写 `/etc/fobe-agent/config.json`；
5. 注册 → 拿到 `node_id` + `node_secret` 落盘；
6. 安装并启动系统服务；
7. 立即上报一次完整信息（IP、系统、CPU 核数、版本）。

`--unprivileged` 是可选的 systemd-only 分支：引导阶段仍需 root/sudo，脚本创建 `fobe-agent` 系统用户、将二进制与本地状态装入 `/opt/fobe-agent/`（0750，用户所有），生成 `User=fobe-agent` 且仅带 `CAP_NET_RAW` ambient capability 的 `fobe-agent.service`。已有 root 安装切换时复制 config/machine-id/update-state 以保留同一节点，并停用旧 sing-box 单元（`one-sing.service` 与改名前的 `fobe-singbox.service` 都停，非特权 agent 自己没有 systemctl 权限去清理后者），避免它和非特权 fallback child 抢端口；procd/fallback 明确拒绝此模式。

> **实现修订 2026-09-16（OpenWrt 实机首装暴露，三处）**：① 落盘一律走 `put_file`（`rm -f` + `cp` + `chmod`）而不是 `install -m`——实测 Kwrt 的 busybox 没编 `install` applet，首装在第 3 步直接 `sh: install: not found`；先 unlink 还避开重装时的 `ETXTBSY`（`cp` 原地写正在运行的二进制会被内核拒绝；unlink 让活进程留在旧 inode，由随后的服务 restart 换入新二进制，与上文 enable/restart 陷阱同一语义）。② procd 分支曾把**二进制**误装到 `/etc/init.d/fobe-agent`（`$TMP/fobe-agent` 手误，写出来的 initd 脚本是死代码）——真机后果是拿 ELF 覆盖 init 脚本、`enable`/`restart` 变成带怪参数跑 agent、procd 服务管理整体失效。已改为安装 `$TMP/fobe-agent.initd`，heredoc 去引号让 initd 里的 agent 路径跟随 `$BIN_DIR` 探测结果，不再可能与它分叉。③ procd 分支收尾 `restart` 在**首装**时会打印 `Command failed: Not found`——rc.common 的 restart 就是 stop+start，而 procd 从没见过这个服务，stop 步骤的 `ubus call service delete` 回 `NOT_FOUND`（退出码仍是 0，纯化妆性噪音，agent 实际正常上线）。改为 `stop >/dev/null 2>&1 || true` + `start`：首装输出干净，start 的真实错误仍然可见，重装时依旧真的停掉旧进程。三分支已在 busybox 容器（stub procd/systemd/fallback）里做过首装+重装实跑验证。

### 5.3 服务管理抽象

| 平台 | 检测 | 服务形态 |
|---|---|---|
| 常规 Linux | 存在 `/run/systemd/system` | systemd unit（`fobe-agent.service` / `one-sing.service`，后者与 one-sing.sh 同名，§9.3） |
| x86 OpenWrt | 存在 `/sbin/procd` 或 `/etc/rc.common` | `/etc/init.d/fobe-agent`、`/etc/init.d/sing-box`（procd，`USE_PROCD=1`） |
| 兜底（容器/WSL） | 两者皆无 | 前台进程 + pidfile + 看门狗 |

`sing-box` 的服务文件由 agent 自己写（面板只下发期望状态），这样才能保证不同平台一致。

> **实现修订 2026-09-15（检测谓词用错，systemd 全被误判为 fallback）**：`service.Detect()` 用 `fileExists("/run/systemd/system")` 检测第一行，而同一个包里的 `fileExists` 语义是"存在**且不是目录**"（它对 unit 文件/二进制/配置是对的）——`/run/systemd/system` 恰恰是 systemd 自己建的**目录**，于是**每一台 systemd 机器（所有常规 Debian/Ubuntu VPS）都返回 `KindFallback`**。这不是"这台没有 supervisor"，是压根没检测到 supervisor，症状分散在三处、互相看起来无关：
> - 当时的 `hello.Caps` 报 `systemd=false / fallback=true`，旧版 §5.5 又把自更新错误绑定到 supervisor，于是 `self_update=false`，面板把这台"其实一直跑在 systemd 下"的探针标成"不支持自更新 / no service manager to restart the agent"；2026-09-16 改为 `exec` 后能力位已只看 Linux、可解析自身路径和二进制目录可写；
> - sing-box 落到 **spawn 兜底分支**（agent 自己 fork + reap）而不是自己写的 systemd 单元（当时的 `fobe-singbox.service`，2026-09-16 起叫 `one-sing.service`，见 §9.3），于是它不受 systemd 管、开机不自动起；
> - 面板的 sing-box 启停/重启（`commands.go` 与 §9.x 动作）一律回 `no service manager detected`。
>
> 修法：检测存在性用新加的 `pathExists()`（`os.Stat` 成功即可，文件或目录都算），`fileExists()` 保持"文件"语义并只用于它该用的地方；`Detect()` 拆出 `detectAt(root)` 以便用假根目录单测（`internal/agent/service/detect_test.go` 钉住"目录必须算存在"）。**同批修掉被这次修正才第一次走到的 native 分支的两个潜伏问题**：① `singboxManager.start` 改用 `restart`（`systemctl start` 对已在跑的服务是 no-op，改了 config 也不会重载，而闸门③只看到"端口有人应答"；这与 §5.5 里 install.sh 的 `enable --now` 陷阱是同一类）；② 迁移到 native 前用 `dropOrphan()` 清掉 fallback 分支遗留的孤儿 sing-box——它被 reparent 到 init 后仍占着入站端口，会让新 unit 起不来、`Restart=always` 反复重启，闸门③却去怪新版本。另外 `InstallSingbox` 补上 `systemctl enable`：unit 文件里的 `WantedBy` 不会自己创建 `multi-user.target.wants` 符号链接。
>
> **实现修订 2026-09-16（非特权运行）**：`euid != 0` 的 agent 不能写 `/etc/systemd/system`，也不能经 system bus 管理 unit（会被 polkit 拒绝），因此 sing-box **显式强制**走既有 fallback 分支（agent `spawn` + watchdog），而不是把宿主的 `caps.systemd=true` 错当成自己可用的管理权限。面板端口本来就限制为 10000–60000（§9.3），非 root 可直接绑定；工作目录从 `-config` 同级的 `one-sing/` 派生，`FOBE_SINGBOX_HOME` 可显式覆盖，服务端下发 JSON 里的 `/etc/one-sing` 证书前缀在 agent 写盘、哈希比对时同步重定位。root 的 legacy 布局迁移与 `one-sing.service` 接管跳过：前者只会产生 EACCES，后者无权执行；已有 root 服务占端口时由闸门③如实失败上报。`install.sh --unprivileged` 是此形态的唯一受支持引导路径（§5.2）。

### 5.4 OpenWrt 专项

- 二进制与配置统一放 `/etc/one-sing/`（`sing-box` + `config.json` + `cert/`），与 one-sing.sh 同一套路径（实现修订 2026-09-15，见 §9.3）；`/etc` 在 OpenWrt 上同样是 overlay 持久。
- **写放大控制**：指标与日志不落盘（内存环形缓冲），仅上报；错误日志按行数上限写 `/tmp`（tmpfs）。
- 内存：agent 目标常驻 < 30MB；采集周期在低内存设备上可从 60s 放宽（面板可配）。
- 首次运行检测 overlay 剩余空间，低于阈值时拒绝安装 sing-box 并给出提示（避免把路由器写满）。**实现修订 2026-09-16（阈值必须随产物大小走）**：原先只查一个平坦的 64 MiB，而 agent 要写的 sing-box 二进制自 1.14 起已近 90 MiB（见 §9.2 实现修订）——闸门等于放行一个必然写满 overlay 的安装。现在分两道：收敛前的粗筛仍查 64 MiB / 100 inode（此时还不知道产物多大），拿到响应头后按 `64 MiB + 产物` 复核；**替换已有二进制时再加一份**，因为它要被留成 `.prev` 供回滚（与 §5.5 的 2× 规则同源，只是首装没有那份 `.prev`，不该为不存在的副本拒绝）。空间扫描不到时一律放行（fail open）。

### 5.5 agent 自更新（跟随服务端）（实现修订 2026-09-15）

> 目标：**探针 agent 一直跟着服务端走**——服务端换了版本，探针就换成与之匹配的那一版，**不看高低**（降级与升级共用同一套逻辑）。本节取代 §9.2 里"agent 自更新走同一条链路"那句：自更新**不复用 sing-box 的三道闸门**（sing-box 有 `check` 与 30s 观察期，agent 没有可比的"先验后启"手段），换来的是**旁路自检 + 原子提交**，且**不保留 `.prev`**。

- **判据只有一个：服务端版本号**。服务端在 `hello_ack` 里下发 `agent_target_version`，agent 拿它和自己的 `internal/agent.Version` 比，不等就切换（含降级）。两端版本由构建时同一个 `$VERSION` 注入（`cmd/server` 的 `main.version` 与 `internal/agent.Version`），所以"服务端版本"就是"该配哪一版 agent"。
- **发布门槛（fail-closed）**：仅当 ① 服务端版本是发布形态（非空、非 `dev`）且 ② `<FOBE_DL_DIR>/agent/<version>/{linux-amd64,linux-amd64.sha256}` 都在时，才下发 target。任一不满足 → 不下发，面板显示停用原因。理由：dev 构建没有"发布"概念，而"下发一个取不到的版本"只会让每台探针反复重试 404。
- **触发 = agent 自发，不需要任何人确认**：agent 每次握手看到不一致就自己动手。这是**系统行为**，不属于 §12.3 的"元操作"清单；**Kill Switch on 时服务端不下发 target**，因此"永远跟随"在这段时间让位给止损闸。
- **错峰由服务端编排**：服务端重启会让全部探针同时重连、同时发现不一致，因此 `hello_ack` 里带 `agent_update_after`（unix 秒），服务端按 `hash(nodeID + target)` 在 0–5 分钟内给出确定性偏移（同一 node+target 稳定，target 变则重排），并把计划时刻写进 `nodes.agent_update_planned_at` 供面板显示"计划中/进行中"。**这不是概率问题**：不错峰就是 N 台同时拉 10MB，而那一刻服务端刚起来。
- **执行链**（agent 侧，收到 target 且已过 `update_after`）：
  1. 下载到**目标二进制所在目录**里的临时文件（同目录才能 `rename`，跨文件系统会 `EXDEV`）；目标路径由 `os.Executable()` 解析（跟随符号链接）——不猜 `BIN_DIR`，systemd 装的是 `/usr/local/bin`、OpenWrt 是 `/usr/bin`；
  2. 与 `linux-amd64.sha256` 比对 sha256，不符即 `terminal` 失败；
  3. 以 `-selfcheck` 跑一次**新二进制自己**：用同一份 config 连服务端，`hello` 带 `selfcheck:true`，hub 只回 `hello_ack` 后立即关闭——**不注册、不落库、不顶掉线上连接**（`hub.go` 的重复注册会 `old.close()`：天真地握手会把正在跑的 agent 踢下线，还会让面板显示一个并没生效的版本号）。自检还必须校验 `hello_ack.agent_target_version`：非空且不等于自己的 `Version` 即失败，挡住"目录名与二进制内版本错位"这类产物错放（对方为空说明服务端此刻已不再下发 target，那是环境变化而不是这个产物的问题，不作为失败）；超时 60s；
  4. 自检通过 → `rename` 覆盖目标二进制 → **`exec` 原地换入**新版本。PID 不变，systemd/procd 若存在只负责崩溃恢复；Go 默认 CLOEXEC 关闭旧 socket，`main()` 重新初始化连接与状态。**不保留 `.prev`**：不留就没有本地回滚，代价是"自检过但 `-run` 起不来"这种残余情形只能 SSH 重装（§20）；换来的是 OpenWrt overlay 上少 10MB 常驻占用；
  5. 自检或下载失败 → 线上二进制**一动不动**。
- **fallback 也参与**（实现修订 2026-09-16）：`exec` 不依赖 supervisor，`nohup`/容器 fallback 只要二进制所在目录可写就能跟随；若 exec 真失败（ENOEXEC/权限），才回退旧语义 `exit(0)`，交给可能存在的 supervisor。
  ⚠️ **`unsupported` 是能力判定，不是失败（实现修订 2026-09-15）**：它 `attempts=0`、什么都没试过，所以服务端**不为它建失败告警**（并顺手 recover 掉该节点既有的失败/环境告警，否则一条永远无法自愈的 high 风险告警会长期挂在面板上），`audit_logs` 的 risk 记 `low`；面板把它渲染成中性提示（`agent_update_reason`）而不是红字"上次错误"，并按「目录不可写/无法解析自身路径」与「旧二进制未上报能力位」给出不同的重装提示文案。`caps.fallback` 仅描述宿主的服务管理环境，不再决定自更新能力。
- **能力位与失败分类（两本账）**（实现修订 2026-09-16）：`hello.Caps.self_update` 不再判「是否发现 supervisor」，而是判「linux + 可解析自身二进制 + 该目录可写」（`rename` 看目录权限，不看二进制文件自身 mode）；`unsupported` 原因如实报告「无法定位自身二进制 / 目录不可写」。
  - `terminal`（不再重试，等 target 变更或人工重试）：sha256 不符 / 无法 exec / 自检报版本不符；
  - `transient`（退避 1m → 1h 封顶，不计入熔断）：连不上服务端、`/dl` 404/5xx、同目录剩余空间 < 2×产物、目标不可写。
- **熔断双记账**：agent 本地 `update-state.json`（默认 `/etc/fobe-agent/`，非特权模式与 `-config` 同目录）记 `target / attempts / last_error / class`（跨 re-exec 有效），**同一 target 连续 3 次 `terminal`** 就停手并上报；服务端 `nodes` 表同样记一份（面板可见 + 人工解锁），target 变化时两边计数清零。本地那份是唯一能在"替换无效、反复重启"时救命的账，服务端那份负责可见性——**只留一边都会在某个场景下失效**。
- **状态与面板**：`nodes` 增 `agent_target_version / agent_update_state / agent_update_attempts / agent_update_error / agent_update_planned_at / agent_update_done_at`（`migrateAdditive`，幂等）；节点页显示 当前版本 / 期望版本 / 计划时刻 / 上次结果与原因；`POST /api/nodes/{id}/agent/retry` 清计数（人工解锁）。每次尝试写 `audit_logs`（`actor=system`）。
- **告警三档**：`terminal` 立即；`transient` 连续 3 次；分发后 15 分钟仍未收敛的节点**汇总一条**（与 §9.5 同一口径与去重窗口）。
- **存量探针只能人工重装一次**：今天已装的 agent 二进制里没有这段代码，服务端下发 target 它也不认识（Go 忽略未知 JSON 字段，它会照常跑）。判定靠**能力位而非版本号猜测**：新 agent 在 `hello.Caps` 里报 `self_update=true`，不报的一律在面板标"需人工重装（不支持自更新）"并给出重装命令。**不许假装它会自动跟上。**
- **产物供给**：镜像里**不能**把 agent 产物放在 DL 目录——compose 把 `./data` 挂到 `/data`，`FOBE_DL_DIR=/data/dl` 在卷内，挂载在容器启动时就已生效，镜像里 COPY 进卷的东西**根本不可见**（不是"被覆盖"，是读不到）。因此镜像把产物放在卷外的 `/srv/agent-seed/agent/<version>/`，服务端启动时**复制进 DL 卷**（缺什么补什么，已有则不覆盖；`FOBE_AGENT_SEED_DIR` 可改，置空即关闭；与 §9.5 的 sing-box 缓存互不影响）。同一个启动步骤还会把卷里的 `agent/latest` 指向**服务端自己这一版**——否则升级容器后 `/install.sh` 会继续装上一版的 agent（`latest` 是本地脚手架留下的真实目录时不动它）。不这么做，"跟随服务端"在标准 compose 部署里默认就是 404。
- **混版承诺**：同一大版本内双向兼容（新增字段一律可选、未知字段/帧忽略并记日志）。升级/降级过渡期必然是混版，"版本不匹配就拒绝"会把探针直接锁死。`protocol.Version` 保持 `1`。
- **开关**：`settings.agent.auto_update`（默认开，面板可关）。关掉或 Kill Switch on = 不下发 target。AI 侧**不新增任何工具**（§12.2）：状态随既有节点查询返回，触发/冻结/解锁/重试都只能由人在面板操作。
- **面板与接口**（全部走会话鉴权，`GET /api/agent/update` 集群状态、`POST /api/nodes/{id}/agent/retry` 人工解锁、`POST /api/nodes/{id}/agent/reinstall-command` 生成一次重装命令）。节点视图新增 `agent_target_version / agent_update_state / agent_update_attempts / agent_update_error / agent_update_planned_at / agent_update_done_at` 与能力位 `agent_self_update`（外加 `agent_caps_seen`：没握过手的节点不算"不支持"，否则新建节点会被误标重装）；`agent_update_state` 取 `planned|downloading|verifying|committed|failed|transient|suppressed|unsupported`。
- **"重试"要能推动在线的探针**：`hello_ack` 之外的载体是普通 `desired` 帧（`DesiredState` 里带上同样两个字段），因此解锁不必等下一次握手；服务端由同一个函数同时填两个载体，不可能填出不一致的两份。
- **两个必须失败关闭的点**（都在实现里显式处理）：① 临时文件必须落在**目标二进制同目录**再 `rename`（跨文件系统 `EXDEV`；`open+truncate` 覆盖正在运行的文件会 `ETXTBSY`）；② 同目录可用空间 < 2×产物即判 `transient` 拒绝，宁可不动也不能写半个二进制。
- **dev 环境怎么让"跟随"真的可验证（实现修订 2026-09-15）**：设计上 `dev`/`compose` 不是发布形态，而两端版本号又恒等（判据是"相等"，不是"新旧"）⇒ 测试环境**永远判已收敛**，这条链路在 dev 里一次都跑不到；"把 `IsReleaseVersion` 对 dev 放开"是死路——等值判定在门槛之前，两端都是 `dev` 照样什么都不发生，还会让面板把陈旧探针显示成已收敛。`scripts/dev.sh` 因此自己产 agent 产物、并给自己一个**内容寻址**的版本号：先用固定占位版本 `dev-pending` 交叉编译一份 agent，对产物取 sha256 前 12 位作为 `dev-<fp>`（全字母时补一位数字，保证过 `IsReleaseVersion`），再用这个号正式编一次（Go 对相同输入确定性：实测同 flags、不同输出路径两次构建哈希一致）。产物落 `data/agent-seed/agent/<版本>/`（`FOBE_AGENT_SEED_DIR`），由 server 启动时的 `Seed` 复制进 DL 卷——和容器同一条路径，顺带覆盖"挂载卷遮蔽"。
  **为什么不是时间戳**：AGENTS.md 要求"改了后端必须重启 `dev.sh`"，时间戳会让**每次后端热改都触发全网探针下一次 10MB 并重启一次**，`data/dl/agent/<版本>` 还会每次多一个 ~10MB 目录。内容寻址把触发条件收紧成"agent 真正编出来的东西变了"（改 `internal/protocol` 这类两端共用包也会换号，这是正确的）；**纯注释/格式改动不换号**——二进制没变，探针本来就不该动（实测：改一个日志字符串换号，还原后回到原号）。
  **语义后果**：版本号是**占位构建**产物的哈希，不是交付产物的哈希——它与 `linux-amd64.sha256`（交付产物的摘要）没有推导关系，别互相校验。`FOBE_VERSION=<旧号>` 可显式钉住版本号 ⇒ 降级演练就是"用旧号重跑 dev.sh"（旧产物还在 DL 卷里，`Seed` 不覆盖）。
  **compose 本地构建同样跟进（实现修订 2026-09-16）**：`--build` 形态此前缺省注入恒定的 `compose`，同样过不了 `IsReleaseVersion`，且等值判定下两端都叫 `compose` 永远"已收敛"——本地构建部署的探针**永不**跟随服务端。`Dockerfile.server` 的 golang 阶段现在对占位 VERSION（空/`dev`/`compose`）做与 dev.sh 同样的事：先用 `<base>-pending` 编一份占位 agent、取 sha256 前 12 位（全字母时前置 `0`），得到 `compose-<fp>`（不带 build-arg 直接 `docker build` 时是 `dev-<fp>`）；显式传入的真实版本（发布管线传 tag）原样通过。于是"改 agent 代码 ⇒ 重新 `--build` ⇒ 换号 ⇒ 探针自更新"在容器形态也成立；agent 源码没变的重 build 不换号、不折腾探针。
  ⚠️ **一次性动作**：`data/dl/agent/latest` 若是**真实目录**（本地手工 staged 的产物），`ownedLatest` 判定"不是我们的布局"、永不重指，而 `/install.sh` 固定取 `dl/agent/latest/linux-amd64`（重装命令走的也是它）——于是"重装"会把你装回那份旧二进制，人却在困惑它为什么永远不跟随。删掉它，让 `pointLatest` 建符号链接布局。
- **错峰可参数化（`FOBE_AGENT_UPDATE_STAGGER`）**：默认仍是 5 分钟；1–2 台探针的测试机设 `1s` 即"立即"（偏移取 `hash%span`，span=1s 时恒为 0）。理由是"还没到计划时刻"在面板上与"功能没生效"长得一模一样。≤0 视为未设——`agentupdate.New` 会把非正值回落到默认 5 分钟，静默吃掉 `0` 本身就是一个陷阱。生产不设。
- **重装命令必须让新二进制真的上场（2026-09-15 修订）**：`install.sh` 的 systemd 分支原本收尾是 `systemctl enable --now`，procd 分支是 `/etc/init.d/fobe-agent start`——**对已经在跑的 agent 两者都是 no-op**。而面板的「重装命令」恰恰是为"已经在跑、只是版本旧"的节点准备的（§5.5 存量探针路径），于是它只把磁盘上的文件换掉，进程仍跑着被替换掉的旧 inode：`strings $(command -v fobe-agent)` 显示新代码（用户以为装好了），`systemctl status` 的 `Active since` 却远早于这次安装，节点永远报旧版本、期望版本箭头永不消失。改成 `enable` + `restart`（`restart` 对未运行的单元等同启动，首次安装同样覆盖），procd 侧同样用 `restart`；fallback（`nohup`）分支则先 `pkill -f "^$BIN_DIR/fobe-agent"` 再起，否则重装会留下两个 agent 抢同一个节点凭据（hub 会把先连上的那个顶掉）。排查口径：`systemctl status fobe-agent` 的 `Active since` 是否是刚刚；与 `/proc/$(systemctl show -p MainPID --value fobe-agent)/exe` 里的版本号对照磁盘上的二进制。

---

## 6. 数据模型（SQLite，WAL）

| 表 | 关键字段 | 说明 |
|---|---|---|
| `users` | id, password_hash | 单行 |
| `sessions` | id, created_at, last_seen, ua, ip, revoked | 可批量吊销 |
| `ip_blacklist` | ip, reason, fail_count, created_at, expires_at | 持久化 |
| `settings` | key, value, encrypted | 全局 anytls 密码、AI 配置、Telegram、保留期、延迟测量频率（`latency.interval_seconds`，默认 5）等 |
| `reg_tokens` | token_hash, note, expires_at, used_at | 单次 |
| `nodes` | id, name, machine_id, node_secret_hash, status, last_seen, agent_version, os, arch, kernel, distro_id, distro_version, cpu_cores, primary_ip, country_code, tz；**自更新（§5.5）**：agent_target_version, agent_update_state, agent_update_attempts, agent_update_error, agent_update_planned_at, agent_update_done_at | 探针主表；`tz` 与发行版（os-release 自动探测，§16）由 agent 上报，只读 |
| `node_ips` | node_id, ip, family, scope, is_primary, manual_primary | 多 IP 全量上报 |
| `node_interfaces` | node_id, name, is_default, updated_at | agent 上报的可选网卡清单与默认路由标记 |
| `node_network` | node_id, iface, mode(in/out/both/max), quota_bytes, cycle_type(none/month/year), next_reset_at | 流量口径、配额与独立流量周期；空 `iface` 表示 agent 自动选择默认路由 |
| `node_billing` | node_id, cycle_type(none/month/day/year), cycle_days(单位随 cycle_type), next_due_at, note | 缴费周期 |
| `traffic_counters` | node_id, iface, direction, last_raw, last_ts, period_start, period_used | 回绕/重启检测 |
| `traffic_daily` | node_id, date, rx_bytes, tx_bytes | **永久**，月曲线与配额靠它 |
| `metrics_samples` | node_id, ts, cpu, mem_used, mem_total, disk_used, disk_total, net_rx_rate, net_tx_rate, uptime | 保留 7 天 |
| `latency_targets` | id, name, kind(icmp/tcp), host, port | 面板配置的目标 |
| `node_latency_targets` | node_id, target_id | 每探针选用的目标集 |
| `latency_samples` | node_id, target_id, ts, icmp_ms, tcp_ms, loss | 保留 7 天 |
| `subscriptions` | id, name, token_hash, format, template_id, enabled, ua_filter | 多订阅 |
| `subscription_nodes` | subscription_id, node_id | |
| `templates` | id, name, format(singbox/clash), content | 完整配置模板 |
| `node_singbox` | node_id, version, desired_version, config_hash, status, last_error, cert_pem, cert_sha256, port | sing-box 期望/实际状态 |
| `commands` | id, node_id, kind, payload, status, created_at, sent_at, finished_at, result | 指令队列 |
| `audit_logs` | ts, actor, node_id, action, command, risk, source_ip, ai_session_id | |
| `alerts` | id, kind, node_id, payload, created_at, delivered_at | |

索引要点：`metrics_samples(node_id, ts)`、`latency_samples(node_id, target_id, ts)`、`traffic_daily(node_id, date)`。

**保留策略**：定时任务每 10 分钟删除 `metrics_samples`/`latency_samples` 中超过 7 天的行，并 `PRAGMA incremental_vacuum`。

> **Schema 兼容策略（实现修订 2026-09-16，起因是宿主机重启导致主库页级损坏、settings 全丢的事故）**：
> - **只做增量是铁律**：schema 变更只允许「新表 / 带默认值的新列 / 新索引」（`schema.sql` 的 `IF NOT EXISTS` + `migrateAdditive`，幂等）。所有查询**显式列名**，这样新表/新列对旧二进制是惰性的——旧版本直接打开新库也能服务，**降级不需要迁移**。
>   **唯一的例外（2026-09-16）**：删掉 `node_singbox.rollback_version`。它从来没有任何写入方（面板渲染的「可回滚到 v」永远不可能出现），留着一个死字段比破一次铁律更糟。代价写清楚：跨过这条线**降级不安全**——旧二进制按名字 SELECT 这一列会直接报错，而 `SchemaVersion` 3→4 的 bump 保证每个现有库在迁移前先拍一张快照（§19.3），操作员要回退就用那张。`migrateAdditive` 里删列单独成段并注明原因，别把它当常规手段。
> - **版本追踪**：`PRAGMA user_version` ↔ `store.SchemaVersion`，改 schema 必须同时 bump。`db < build`（升级）：先自动快照再跑迁移、写版本号；`db > build`（降级，§5.5 钉版本演练是真实场景）：只打 WARN、**不迁移、不回写版本号**——本次构建不知道新版本做过什么，重置版本号会让半知的升级重跑。
> - **打开即 `quick_check`**：损坏的库拒绝启动（容器进入重启循环、日志给出恢复路径），而不是像事故里那样全站随机 500、每次写入把洞挖深。
> - **`synchronous=FULL`**（原 NORMAL）：WAL+NORMAL 是常规推荐，但库文件在 Docker bind mount 上，fsync 顺序只与宿主文件系统一样可靠（OrbStack/virtiofs 等）。FULL 每提交一次 fsync，本面板写频是秒级批次、代价测不出来；换来崩溃时最多丢最后一次提交而不是文件完整性。
> - **快照默认开启**：启动即拍 + 每 24h 一轮 + 迁移前必拍（时间戳命名，与每日快照共用 3 份池，§19.3 复核 2026-09-16：库体量随节点数增长，池子刻意压小）；`FOBE_BACKUP_DIR` 缺省 `<db 目录>/backup`，`off` 显式关闭。事故复盘：备份循环此前"配置着"但用 24h ticker、首次触发要等满一天，部署 churn 下容器从未活满——备份一次没跑过，坏的是「有机制、无快照」这种假安全感。

---

## 7. Agent ↔ Server 协议

传输：单条 WSS（`/ws/agent`），JSON 信封，版本化：

```json
{ "v": 1, "type": "metrics", "id": "uuid", "ts": 1730000000, "payload": { } }
```

**核心设计：声明式期望状态（desired state）**。服务端只下发"我要什么"（sing-box 版本、配置、anytls 参数、延迟目标集、终端开关），agent 负责收敛并回报实际状态。好处：agent 离线重连后自动补齐，服务端不需要重放命令序列。

| 方向 | type | 说明 |
|---|---|---|
| agent → server | `hello` | machine_id、版本、os/arch、时区、发行版（os-release 的 `distro_id`/`distro_version`，2026-09-16）、网卡清单与能力位（含 `self_update`，§5.5） |
| | `agent_update` | 自更新逐次上报：phase / class(terminal\|transient) / error / attempts（§5.5） |
| | `metrics` | 60s 一次；面板打开详情页时服务端可请求 5s 实时流 |
| | `traffic` | 60s 一次：选定网卡的累计计数 + 日增量 |
| | `latency` | 60s 一次批量；本地采样点数量按 `latency.interval_seconds`（默认 5s）变化 |
| | `state` | 节点信息、IP/网卡清单、sing-box 实际状态 |
| | `cmd_result` | 指令执行结果（stdout/stderr/exit code，截断） |
| | `terminal` | 终端输出/关闭 |
| server → agent | `hello_ack` | 期望状态全量下发（含流量网卡选择、`agent_target_version` / `agent_update_after`，§5.5） |
| | `desired` | 增量下发期望状态（流量网卡、sing-box 版本/配置/端口/密码/证书要求） |
| | `cmd` | 一次性命令（AI 执行、面板操作） |
| | `terminal_open/input/resize/close` | 终端会话 |
| | `probe_metrics` | 请求临时高频指标采集 5s |
| | `latency_config` | 立即更新本地延迟测量频率（全局设置变更时推送） |

- 心跳：agent 每 15s 一次 `ping`（或空帧），服务端 90s 无心跳判定离线并告警。
- 重连：指数退避 + 抖动（1s → 5min 上限）。
- 离线指令：入队 `commands`，重连后下发；TTL 10 分钟，超时标记 `timeout` 并在面板显示（避免你三小时前点的"重启"突然生效）。
- 所有指令带 `id`，agent 幂等执行（同 id 重复下达只执行一次）。
- **自更新（§5.5）靠三个字段落地，不新增"下发一条更新命令"的路径**：`hello.Caps.self_update`（能力位，旧 agent 不报）、`hello_ack.agent_target_version` + `agent_update_after`（服务端判定与错峰，`DesiredState` 里镜像一份，使人工"重试"能借普通 `desired` 帧推动在线探针——agent 读的是 `DesiredState` 那份，两个载体由服务端同一个函数填写，不会不一致）、`hello.selfcheck`（自检握手——hub 收到带该标记的 hello 后只回 `hello_ack` 就关闭，**不注册、不落库、不关闭同节点的现有连接**；实现里这条路由由 `X-Fobe-Selfcheck` 请求头选择，payload 里的 `selfcheck:true` 必须同时存在）。

---

## 8. 指标采集与流量口径

### 8.1 采集项

| 指标 | 来源 |
|---|---|
| CPU 使用率 | `/proc/stat` 两次采样取差；核数 `/proc/cpuinfo` |
| 负载 | `/proc/loadavg` |
| 内存使用率 | `/proc/meminfo`：优先 `MemAvailable`，缺失时 `MemFree+Buffers+Cached`（OpenWrt 兼容） |
| 交换 | `SwapTotal/SwapFree` |
| 磁盘 | `statfs` 遍历挂载点，面板选主分区展示 |
| 网络累计 | `/proc/net/dev`（选定网卡） |
| 网络速率 | 累计值差分 / 时间差 |
| 运行时长 | `/proc/uptime` |
| 重启检测 | `/proc/sys/kernel/random/boot_id` + 累计计数回绕双重判定 |
| 发行版 | `/etc/os-release`（回退 `/usr/lib/os-release`）的 `ID` + `VERSION_ID`；`ID_LIKE` 含 `openwrt` 或存在 `/etc/openwrt_release` 时归入 openwrt 家族（iStoreOS/ImmortalWrt 等衍生版改写 `ID`，靠这两处识别）（2026-09-16，§16） |

网卡通过 `/proc/net/dev` 与 `/proc/net/route` 探测；agent 将完整可选清单及默认路由出口随 `hello`/`state` 上报。面板的「自动」不猜网卡，始终由 agent 选择默认路由；人工选择只允许该探针已上报的网卡，并以 `desired` 即时下发到 agent。时区同样由 agent 自动上报到 `nodes.tz`，面板只读显示，不允许人工覆盖。

### 8.2 流量累计与重启处理

1. 每次上报 `(last_raw_rx, last_raw_tx)`。
2. `delta = now - last`；若 `delta < 0`（回绕/重启/网卡重置）→ 判定异常，取 `now` 为新基准，**不把负数计入**，并记一条 `counter_reset` 事件供你排查。
3. `delta` 累加进 `traffic_daily`（按探针时区切日）与 `traffic_counters.period_used`。
4. 周期用量 = 周期起点至今日表的和（日表是权威，`period_used` 只是缓存）。

### 8.3 四种模式

设周期内 `IN` = 入向累计（rx），`OUT` = 出向累计（tx），配额 = `quota_bytes`：

| 模式 | 已用量 | 使用率 |
|---|---|---|
| `in` | IN | IN / quota |
| `out` | OUT | OUT / quota |
| `both` | IN + OUT | (IN+OUT) / quota |
| `max` | max(IN, OUT) | max(IN,OUT) / quota |

- 模式是**每台探针**独立配置；切换模式只影响计算与展示，原始数据始终两个方向都存。
- 未设配额（`quota_bytes = NULL`）时，只显示累计用量与实时速率，不显示百分比。
- 达到 80% / 100% 触发告警（阈值可配），**不自动停服**（你的选择）。
- **流量周期独立于缴费周期**（2026-09-16 修订）：每节点设置 `none|month|year` 与「下次重置时间」（Unix 秒，表单精确至秒）。`none` 不隐式回退到自然月，按累计流量计算；月/年模式将已到期的下次重置时间依探针时区向前滚动，保留设置时的日、时、分、秒。月末和 2 月 29 日落到不存在日期时钳制到该月最后一天，下一次仍以原始日期规则计算。缴费周期仅用于到期提醒，与流量统计互不驱动。

---

## 9. sing-box 生命周期与 anytls

### 9.1 期望状态

服务端为每个节点生成完整 `config.json`（inbound + log + 必要的出站），存 `config_hash`；面板改任何参数 → hash 变化 → 下发 → agent 应用。

> **实现修订 2026-09-16（生成的配置要能自己追上新模板）**：每台被管理的节点，其 `config.json` 只在**变更时**生成一次，然后作为 `singbox_config:<node_id>` 缓存在设置里，之后没有任何东西重新推导它。生成器稳定时这没问题，但**生成器本身改了**（§9.4 那次：地址式 DNS 段必须去掉）就出问题了——存量节点会永远收到那份陈旧字节，而 agent 只比 hash（磁盘 == 期望）所以没有任何理由重写文件，**唯一出路是操作员再点一次"安装"**。因此服务端启动时多一步 `SyncSingboxConfigs()`：遍历所有 `desired_version` 非空的节点，用当前模板重新生成，**只有字节不同才**写回 setting + 刷新 `config_hash` + 下发（写 `audit_logs`，actor=system）；幂等，普通重启一个字节都不动。没有 `anytls_password` 或端口非法（未分配/越界）的节点跳过并记 WARN——那不是这一趟的事。

### 9.2 安装/更新三道闸门

```
下载 → sha256 校验 → 备份当前二进制为 .prev
   ↓
① config:  sing-box check -c config.json        （失败即中止，不动线上）
   ↓
② start:   写入新二进制 + 配置 → 启动服务
   ↓
③ verify:  30s 观察期：进程存活 && 入站端口 TCP 可连 && TLS 握手成功
   ↓ 失败
rollback:  恢复 .prev 二进制 + 旧配置 + 重启 → 告警"回滚已执行"
```

> **实现修订 2026-09-16（下载闸门的尺寸预算 + "安装中"不再卡住）**：起因是一次真实的安装失败——告警里是 `download 1.15.0-alpha.4: artifact exceeds 67108864 bytes (rolled back)`。
>
> - **产物上限必须按真实产物定**：`/dl/singbox/<version>/linux-amd64` 直出的是**解包后的单文件二进制**（不是 33 MB 的 tar.gz），而官方 linux-amd64-musl 构建自 1.14 起已近 90 MiB（1.14.1 = 91,891,552 B、1.15.0-alpha.4 = 92,895,232 B）。agent 侧的 64 MiB 上限因此**否掉了当时每一个版本**，而且是在**把整个文件传完之后**才拒绝——探针白付一次带宽、再回滚，面板上只看到"安装中 → 异常"。上限提到 256 MiB（与 §9.5 服务端 `defaultMaxDownloadBytes` 对齐），并**先读响应头再决定要不要接收**（`Content-Length` 触顶直接拒绝，不再下载一遍）。整包下载预算 5 min → 15 min：5 分钟等于要求 90 MiB 在 >2.5 Mbit/s 的链路上完成，远程探针达不到时的现象与"产物被拒"一模一样（下载错 → 回滚）。
> - **空间闸门随产物大小**（§5.4）：见该节的实现修订。
> - **失败原因在上报里是粘性的**：退避窗口（5 min）内的周期性 `reportOnly` 原本把 `last_error` 报成空串，而服务端只在 `running` 或"本次带错误"时改 `status` ⇒ 节点长期停在 `degraded` 却没有任何原因可看。现在 agent 在**尚未健康**（进程未在跑，或版本仍不是期望版本）期间继续上报上一次的失败原因，跑起来且版本正确、或期望版本变更时才清空（新的失败自然覆盖旧的）。"版本对了"不等于收敛——闸门①拒绝时二进制已经落盘，只看版本会把这个错误当成历史清掉（真机上就是这么又丢了一次原因）。
> - **门后面的坑不在门上**：修完尺寸上限，同一台探针立刻在闸门①倒下——生成的 config 用了 1.14.0 已移除的地址式 DNS 段，见 **§9.4 实现修订 2026-09-16**。这条链路的失败是一个接一个暴露的，别看到第一个错误消失就收工。
> - **面板不能对着一次快照发呆**：节点编辑页的 sing-box 卡片原先只在挂载与操作后各读一次，而收敛是异步的（agent 收到期望状态、执行、再上报）⇒"安装中 + 期望状态已下发,等待探针应用并回报"会一直挂在屏幕上，哪怕探针几秒后就已经报回失败。现在 `status=installing` 且探针在线时每 5s 重读一次（离线时改为如实显示"探针离线,期望状态已记录"，因为状态不可能变），收敛或失败后自动清掉"已下发"的提示。

- 版本**必须显式指定**（面板展示可选版本，来自服务端 release 清单），不追 latest。一键批量更新里的 `latest` 只在**点击那一刻**解析成具体版本号并固化后下发，不变量不被破坏（见 §9.5）。
- 产物来源：优先面板 `/dl/singbox/<version>/linux-amd64`（服务端缓存，规避国内拉 GitHub 的问题），失败回退官方地址。**缓存由服务端进程自己填充**（启动时/手动重试，见 §9.5）——agent 侧只拉面板、不补官方回退。
- agent 自更新**不走**这条链路（实现修订 2026-09-16）：agent 没有 `check` 与 30s 观察期可用，改用「旁路自检 + 原子提交 + `exec` 原地换入」，且**不保留 `.prev`**——详见 **§5.5**。
- 每次变更写 `audit_logs`，并在面板节点页显示"当前版本 / 期望版本 / 上次操作结果"。

> **实现修订 2026-09-16（闸门①在所有"要启动 sing-box"的路径上都跑）**：闸门①原本只在**变更路径**（`apply`）里执行；"版本与配置都已就位、只是没在跑"那条路径直接 `start` 就进闸门③。真机上这被证明是个诊断陷阱：一份 sing-box **拒绝加载**的配置（§9.4 的地址式 DNS）留在盘上，文件永远不变 ⇒ 变更路径永不进入 ⇒ `check` 永不执行 ⇒ 每一轮都报闸门③的 `sing-box process exited during observation window`，而真原因（`config check: …`）一次都没出现过。现在 `checkConfig()` 是两条路径共用的独立闸门；那条路径上没有任何东西被替换，所以失败**不需要回滚**，如实上报即可。
>
> 这一条与 §9.1 的启动同步是同一次故障的两半：同步负责把陈旧的配置**换掉**，闸门负责在配置**换不掉**时把真话说出来。

> **实现修订 2026-09-15（服务端产物缓存 + 一键批量更新）**：本节原本假定"版本清单里总是有货"，但从没有一条链路负责**把货取回来**——服务端只直出 `/dl` 目录里已有的文件，agent 也只会从面板拉取。本次补齐这条链路，并且**不新增任何安装路径**：
>
> - **服务端产物缓存**：server 在启动时（仅当 `<FOBE_DL_DIR>/singbox` 里没有任何有效版本）后台下载"当时最新稳定版"到 `<FOBE_DL_DIR>/singbox/<version>/`，已有缓存则**完全不联网**；可用 `FOBE_SINGBOX_AUTO_DOWNLOAD=0` 关闭。下载校验 **fail-closed**，失败只记 WARN + 设置页状态 + 手动重试，不阻塞启动。
> - **一键批量更新**：设置页点「更新 sing-box」→ 先展示受影响节点名单 → 二次确认 → 把目标版本写进每个已启用 sing-box 节点的 `desired_version`（保持端口与配置不变）→ 复用 agent 现有的三道闸门与 `hello_ack` 收敛。异步 job + SSE 进度 + 每节点结果表；分发后 15 分钟未收敛的节点汇总成一条告警。
> - 详细规则、接口与错误码见 **§9.5**；目录与卷见 **§17**。

### 9.3 anytls 与自签证书

> **实现修订 2026-09-15（探针侧布局与服务管理对齐 one-sing.sh）**：入站配置与服务管理参考 `one-sing.sh`（同类需求的成熟实现），落地三件事，**协议与订阅语义不变**：
> - **目录/证书布局（root 模式）**：探针上统一为 `/etc/one-sing/{sing-box,config.json,cert/{cert.crt,private.key}}`（原 `/usr/local/bin/sing-box`、`/etc/sing-box/{config.json,cert/{cert.pem,key.pem}}`）。同名同路径意味着**已经用 one-sing.sh 管起来的机器可以直接被接管**，不必重下二进制或重签证书。证书文件名由服务端写进 `config.json`，两侧必须一致，`internal/server/singbox` 有一条跨包测试盯着（`TestCertPathsMatchAgentLayout`）。
> - **anytls `padding_scheme`**：采用 one-sing.sh 的方案（`stop=6` / `0=30-30` / `1=80-120` / `2=350-550,c` / `3=900-1400` / `4=250-600` / `5=250-600`），不再用 sing-box 默认值。它进 `config.json`，因此**改这一项等于改 `config_hash`，会在下一次收敛时把全部节点重新下发一遍**——有意的、一次性的代价。
> - **systemd 单元对齐**：单元补上 `CapabilityBoundingSet` / `AmbientCapabilities`、`ExecReload=/bin/kill -HUP $MAINPID`、`LimitNOFILE=infinity`、`RestartSec=10s`（保留 `User=root`、`NoNewPrivileges=true`）。**单元名（2026-09-16 修订）＝ `one-sing.service`，即 one-sing.sh 自己的名字**：二进制与配置路径本来就与那个脚本同名同路径（`/etc/one-sing/{sing-box,config.json}`），单元名却是 fobe 自有的，结果是两套工具各自管一个单元、互相 disable——面板按下启动，脚本那边看到的还是"服务已停"。改名后两边指向同一个单元，脚本的 `systemctl {restart,status}` 直接作用于 fobe 的 sing-box。**代价（镜像是旧的也是真的）**：单元从此是共享物，谁最后写谁的内容生效（fobe 每次 apply 都会重写它），卸载 fobe 会删掉脚本也认得的单元；反过来，脚本 `create_systemd_service` 只在二进制缺失时才写，而 fobe 保证二进制在，所以那条路平时不会触发。procd 侧同时从 `/etc/init.d/sing-box` 改为 `/etc/init.d/one-sing`——OpenWrt 上 `sing-box` 这个名字常年属于发行版自己的包，fobe 此前一直在覆盖别人的启动脚本。
>   **同名的边界（别指望混用协议配置）**：共享的是**单元**，不是**配置文件**。`one-sing.sh` 用 `jq` 往 `/etc/one-sing/config.json` 里追加 SS2022/VLESS/Socks5 入站，而那份 config.json 是 fobe 的期望状态产物——下一次收敛（最多 60s）发现 hash 不符就会**整份重写**，脚本加的入站会消失（fobe 的闸门②也随之重启服务）。要用脚本的那些协议，就别让 fobe 管这台机器的 sing-box；二者共享单元名的收益是"面板与脚本对同一个服务生效"，不是"配置可以混着改"。
>   **改名要带迁移**：改名前的 `fobe-singbox.service` / `/etc/init.d/sing-box` 仍然 enable 且在跑，和 `one-sing.service` 抢同一个二进制、配置与入站端口 ⇒ agent 启动时（root）`RetireLegacySingboxUnit()` 停掉、disable 并删掉旧单元；procd 侧**只删自己写的那个**（脚本内容含 one-sing 布局才算我们的，`ownsLegacyInitScript`），发行版的 `/etc/init.d/sing-box` 一律不碰。旧的 `AdoptForeignSingboxService`（把 one-sing.service 停掉并 disable）随之删除——它现在会 disable 掉 fobe 自己刚建的单元。`install.sh --unprivileged` 两个名字都停：非特权 agent 没有 systemctl 权限，只能靠引导脚本收尾。
> - **迁移是幂等的**：agent 启动时 `MigrateSingboxLayout` 只在「目标不存在且源存在」时搬文件（二进制连同 `.prev`/`.download`、配置连同 `.prev`、证书改名 `cert.pem→cert.crt`、`key.pem→private.key`），搬完删空的旧目录；搬不动只记 WARN，收敛循环照样能把缺的东西重新下载回来。
> - **`config.json` 是 fobe 独占的**：同一台机器上又跑 one-sing.sh 又由 fobe 托管，两边会互相覆盖同一份 `config.json`（one-sing.sh 加的协议会消失）。要共存就改路径，别只改服务名。

> **实现修订 2026-09-16（非特权目录重定位）**：`--unprivileged` 不可写 `/etc`，故布局为 `dirname(-config)/one-sing/`（安装路径即 `/opt/fobe-agent/one-sing/`，`FOBE_SINGBOX_HOME` 优先）。服务端仍生成 root 布局的绝对证书路径，agent 在写盘与 config hash 比对时同步替换前缀；否则每次 60 秒收敛都会误判配置变化并重启。非特权模式不迁移 root 的旧布局，也不接管 `one-sing.service`。

- **私钥永不离开探针**：首次启用时由 agent 用 Go 标准库 `crypto/x509` 现场生成自签证书（不依赖 openssl——OpenWrt 常常没有），存 `/etc/one-sing/cert/{cert.crt,private.key}`（0600）。密钥算法保持 ECDSA P-256（而非 one-sing.sh 的 RSA-4096）：探针是小机器、密钥在设备上现场生成，而客户端是按 SHA256 指纹 pinning 的，算法对客户端不可见。
- agent 上报**证书 PEM + SHA256 指纹 + 有效期**给面板（不含私钥）。
- 订阅渲染时把证书 PEM 写进客户端的 `tls.certificate` 字段做 **pinning**，而不是让客户端 `insecure: true`。这样自签也不会被中间人。
- 入站口令 = `settings.anytls_password`（全局共享，见 §11.3 的取舍）。
- 端口：默认随机高位端口（10000-60000），面板可改；改端口时 agent 尝试自动放行防火墙（ufw / firewalld / nft / OpenWrt fw4），失败则返回需要你手动执行的命令原文。

### 9.4 sing-box 入站模板（生成物示意）

> **实现修订 2026-09-16（DNS 段必须用 1.12+ 的 type 形式）**：下发的是**完整 config.json**（log + dns + inbounds + outbounds），而这份 dns 段一直用 `{"tag":"local-dns","address":"local","detour":"direct"}`——地址式 DNS server **在 1.12.0 弃用、1.14.0 移除**，于是任何一次现行版本的安装都在闸门①原地失败（`config check: exit status 1: .servers[0]: legacy DNS server formats … removed in sing-box 1.14.0`），日志与告警里看起来像"配置坏了"，实际是模板过期。现在写 `{"type":"local","tag":"local-dns"}`（`local` 就够了：入站只有 anytls、出站是 `direct`，解析交给系统 resolver；`detour` 是**远程** server 的 dialer 选项，这里没有意义）。**代价**：这份模板的兼容下限被钉在 sing-box ≥ 1.12（type 形式 1.12 才有），面板的版本列表仍列出更老的 release，装 <1.12 会反过来在闸门①失败。已用真实二进制（1.14.1 / 1.15.0-alpha.4）对模板跑过 `check`。**注意 config_hash 因此变了**：所有节点会在下一次收敛时重新下发一遍配置（与 §9.3 改 padding_scheme 同性质，一次性）。

```json
{
  "type": "anytls",
  "tag": "anytls-in",
  "listen": "::",
  "listen_port": 23456,
  "users": [{ "name": "default", "password": "<全局密码>" }],
  "padding_scheme": ["stop=6", "0=30-30", "1=80-120", "2=350-550,c", "3=900-1400", "4=250-600", "5=250-600"],
  "tls": {
    "enabled": true,
    "server_name": "www.bing.com",
    "certificate_path": "/etc/one-sing/cert/cert.crt",
    "key_path": "/etc/one-sing/cert/private.key"
  }
}
```

### 9.5 服务端产物缓存与批量更新（实现修订 2026-09-15）

> 背景：§9.2 假定"面板能列出可选版本"，但**从来没有任何一条链路负责把产物取回来**——`/dl` 只是直出 `<FOBE_DL_DIR>` 里已有的文件，`GET /api/singbox/versions` 只读 `<FOBE_DL_DIR>/singbox/<version>/manifest.json`。缓存空的部署里，面板永远只有一个空版本列表。本节补齐"取货"与"分发"两段，且**不新增安装路径**：agent 侧的下载 → 校验 → 备份 → 三道闸门 → 回滚完全不变，批量更新只是把 `desired_version` 批量改掉。

> **实现修订 2026-09-15（版本列表 = 一版本一行 + 显式下载）**：设置页的 sing-box 区块就是缓存列表本身，**一行一个本地版本**，行尾两个图标按钮——**发布**（把这一行的版本下发到所有已启用 sing-box 的节点，仍走 §9.5.2 的影响面 + 二次确认）与**删除**（从服务端磁盘删除，仍被 `desired_version` 引用时要 force）。下载中的版本也是同一张表里的一行：`download` 快照带 `version`，面板把「不在磁盘上且正在下载/刚失败」的版本合成一行，就地显示阶段、字节与速度；失败的那一行保留原因并给一个重试图标——服务端在 release 查找失败时会补一个 `failed` 相位（`failSingboxDownload`），否则这一行会永远停在「获取校验信息」。
>
> 两条新接口，都是操作员显式触发，**页面渲染永不联网**：
>
> - `GET /api/singbox/releases`：上游 release 列表（新 → 旧，只取**一页 30 条**，不翻页——一页解压后约 10 MB），10 分钟内存缓存，`?refresh=1` 绕过；每条带 `cached`（磁盘上已有）与 `latest_stable`（该页最大的非 prerelease）。上游不可达时返回上一次的好列表并置 `stale:true`（一次都没成功过才 502）。
> - `POST /api/singbox/versions/{version}/download`：按**显式版本**下载，不接受 `latest`（§9.2「版本必须显式指定」不破）。已缓存 → 200 `cached:true`；另一个版本正在下载 → 409 `download_in_progress`（面板只有一根进度条，不做队列）；受理 → 202，进度沿用同一个 `singbox_download` 快照。写 `audit_logs`（`singbox_version_download`）。受理后立刻合成 `resolving` 相位，否则「查找 release」这段时间（最多走满几页）面板上没有任何东西可显示。

#### 9.5.1 缓存布局与下载源

```
<FOBE_DL_DIR>/singbox/<version>/          # version 去掉前导 "v"，如 1.10.0
├─ linux-amd64                            # 0755，官方单文件静态二进制（agent 既有的下载 URL）
├─ linux-amd64.sha256                     # "<hex>  linux-amd64"（两个空格，sha256sum 格式）
└─ manifest.json                          # 版本/大小/sha256/来源 asset/下载时间，供面板与 /dl 阅读
```

- **下载源**：GitHub Release（默认 `SagerNet/sing-box`）。`FOBE_SINGBOX_API_BASE`（默认 `https://api.github.com`）覆盖 release 列表来源，`FOBE_SINGBOX_DOWNLOAD_BASE`（默认 `https://github.com`）用于拼路径；两者都指向 GitHub 兼容镜像即可在拉不到 GitHub 的环境里工作。
- **选版本**：只认**稳定版**——跳过 draft，跳过 prerelease；语义化版本比较（`1.10.0 > 1.9.9`，不是字典序）。**列表按页取，且尽早停**（实现修订 2026-09-15，见下）。
- **列表页很大，别按"小接口"对待**（实现修订 2026-09-15）：releases 列表把每个 release 的 ~170 个 asset 全量返回，`per_page=100` 一页解压后约 33 MB（gzip 约 2 MB；上游单个 release 约 300 KB）。因此：① 解码上限为 64 MiB，且触顶时报 `response exceeds N bytes`——此前 8 MiB 的 `io.LimitReader` 会把 body 截断，json 报出误导性的 `unexpected EOF`（真实原因是尺寸，不是网络）；② 「当前最新稳定版」只请求 `per_page=30`，并在**第一个含稳定版的页就停**（页按创建时间倒序，能超过该页最大稳定版的版本只可能创建得更晚、即更靠前），单版本查找命中即停。原实现固定走满 5 页 × 33 MB（实测 ~18 s），已贴着面板 `latest` 解析的 20 s 上限。
- **产物变体固定为 `linux-amd64-musl`**：官方标准构建是 tar.gz 且带 `libcronet.so`，而 agent 的安装路径是**单文件二进制**（下载 → sha256 → 直接写 `/etc/one-sing/sing-box`），两者不兼容。musl 变体是同一个 tar.gz 里唯一的静态单文件产物，装到常规发行版也能跑（无 glibc 依赖），代价是失去 cronet 相关能力（fobe 只用 anytls 入站，不受影响）。

#### 9.5.2 校验：fail-closed

校验值来源按顺序回退，**任何一步取不到就拒绝安装**（fail-closed，绝不"跳过校验先装上"）：

1. GitHub API asset 的 `digest`（`sha256:<hex>`，新 release 才有）；
2. 同目录的 `<asset>.sha256`（镜像/离线源常见做法）。

不匹配（`singboxdl: sha256 mismatch`）或解包不出 sing-box 二进制（`singboxdl: sing-box binary not found in archive`）时：**中止，且不污染缓存**——下载与解包全程在 `<FOBE_DL_DIR>/singbox/.tmp-*` 里进行，只有校验通过才 `rename` 进 `<version>/`；半成品目录对 `ScanCache` 不可见，并由清理例程回收。

#### 9.5.3 启动自动下载、开关与状态

- **仅在缓存为空时联网**：`<FOBE_DL_DIR>/singbox` 里已有任何**有效**版本（存在 `linux-amd64` 文件的目录）就完全不请求上游，升级/重启不会重复下载。
- **开关**：`FOBE_SINGBOX_AUTO_DOWNLOAD`（默认开，置 `0`/`false`/`off`/`no` 关闭）。关闭或未配置 `FOBE_DL_DIR` 时状态为 `disabled` 并记录原因。
- **不阻塞启动**：下载在后台 goroutine 里跑，服务端进程该起就起；失败只写 WARN 日志 + 状态，面板设置页显示失败原因与「重试」按钮（`POST /api/singbox/cache/retry`，后台执行，结果经 `/ws/events` 的 `singbox_cache` 事件回归）。开发模式的"面板拉不到 GitHub"因此不会拖垮整个 UI。
- **状态落库**（服务端自有设置，客户端不可写）：
  - `singbox.cache_status` = `{state: ok|disabled|failed|pending, version, updated_at, error, auto_download}`
  - `singbox.dl_mount_ok` = `"true"`/`"false"`，**仅容器内**写入：检测到 `/.dockerenv` 或 cgroup 标记时，检查 `FOBE_DL_DIR` 是否为 `/proc/self/mountinfo` 中的独立挂载点；不是则以 WARN 提示"升级容器会丢失已下载版本"并在设置页标红。非容器环境跳过（避免 dev 噪声）。
- **旧版本不自动删**：缓存里的历史版本一直保留；设置页显示每个版本的占用、下载时间与被多少节点引用，手动删除（`DELETE /api/singbox/versions/{version}`），仍被 `desired_version` 引用时需要显式 force。
- **下载进度可见 + 同一产物只下一次**（实现修订 2026-09-15）：安装过程经 `singboxdl.Config.Progress` 上报阶段（`waiting` / `resolving` / `downloading` / `verifying` / `extracting` / `publishing` / `done` / `failed`；下载阶段带已下载字节与总大小，末次必报精确值），服务端折成一个 `download` 快照：既随 `GET /api/singbox/cache` 返回（中途打开页面也有进度条），也走 `/ws/events` 的 `singbox_download` 事件（已打开的页面实时刷新；字节级事件按 250ms 节流，相位变化、首字节与终态立即推送）。快照**只存内存**——字节计数器若落库就是每秒多次写 SQLite，撞 §5 的单写者。同时**全进程只有一个 `singboxdl.Client`**（`httpapi.Server.SingboxDL()`，`cmd/server` 把它交给启动缓存管理器）：`Install` 按该 client 的互斥量串行，启动自动下载 / 手动重试 / 批量更新撞同一个版本时后来者只报 `waiting` 并排队，拿到锁后发现版本已发布即返回 `ErrVersionExists`（调用方按成功处理），**同一个 tarball 不会被下载第二次**。此前每个请求各自 `singboxdl.New` 一个 client，"启动自动下载"与"更新 sing-box"各持一把锁，同一版本会被并行抓两遍。面板在下载进行中将「更新 sing-box」置灰并提示原因。

#### 9.5.4 一键批量更新

```
设置页版本行尾的「发布」图标（实现修订后没有全局选择器：发布的永远是这一行的版本）
  → GET  /api/singbox/update/impact        列出受影响节点（已启用 sing-box、desired_version 非空）
  → 二次确认（强制，不可跳过；弹窗列出节点名单与目标版本）
  → POST /api/singbox/update {version|latest, confirm:true}   → 返回 job id
  → GET  /api/singbox/update/{job}         进度 + 每节点结果（页面刷新后仍可见）
  → /ws/events 的 singbox_update 事件      实时进度（不必轮询）
  → 15 分钟后复查收敛，未收敛节点汇总为一条告警
```

- **目标版本**：请求可以是 `latest`，也可以是一个具体版本号（缓存里没有就先把它下载下来）。`latest` **在点击那一刻**经上游解析成具体版本号并固化——落库、审计、下发的都是具体版本号，因此 §9.2 的"版本必须显式指定、不追 latest"不变量**不被破坏**（追 latest 的只是"点击"这个动作，不是 agent 的常态行为）。目标版本不在缓存里时，job 先下载它（同样 fail-closed），下载失败则整个 job 失败且不写任何 `desired_version`。
- **改写期望状态而非发命令**：对每个目标节点写 `desired_version`（**保持既有端口与配置不变**），然后对在线节点 `pushDesired`；离线节点什么都不发，等它重连时由 `hello_ack` 全量下发自然收敛（§7 声明式期望状态）。
- **结果分档**（每档逐节点带原因）：`already_current`（本来就已是该版本且期望一致）/ `pushed`（已写入并推送成功）/ `offline_pending`（已写入，agent 离线，待重连收敛）/ `failed`（写入或推送失败，附原因）。
- **job 持久化**：状态写在 `settings.singbox.last_update`，页面刷新、甚至服务端重启后都能看到同一次更新的进度与结果；服务端重启时按持久化的 deadline **重新武装**收敛检查（未完成的 job 标记为失败）。
- **审计**：每一次更新、重试、删除版本都写 `audit_logs`。
- **15 分钟收敛告警**：分发后 15 分钟（`DefaultConvergeAfter`）复查各目标节点上报的 `node_singbox.version`，仍未等于目标版本的节点**汇总成一条**告警（kind `singbox_update_stale`），走既有的 `deliverAlerts` → Telegram/Webhook 通道，并在 1 小时窗口内去重。告警只提示、不自动回滚（同 §0 决策 7 的取向）。

#### 9.5.5 边界与取舍

- **不暴露给 AI 助手**：本节新增的接口不在 §12.2 工具集内。批量更新影响面覆盖所有节点，不适合落进"默认放行"的模型执行路径；单节点更新仍可用既有的 `update_singbox` 工具。
- **保持 panel-only**：agent 侧**不补官方回退**——§9.2 里"失败回退官方地址"这句对 agent 依然不成立，产物只从面板拉。理由：让"探针从哪里拿二进制"只有一个答案，出问题时可复现；服务端缓存本身已经承担了规避 GitHub 不可达的职责（镜像源/手动投放）。
- **本机手动投放**：把产物按上面 9.5.1 的布局放进 `<FOBE_DL_DIR>/singbox/<version>/` 即可，无需联网——这也是完全离线环境的兜底用法。

---

## 10. 订阅与模板

- 路径：`/sub/<token>`；`token` 高熵随机，库中只存 hash。
- 格式决策：`?format=singbox|clash` 显式指定；缺省按 UA 嗅探（`clash` / `sing-box` / `mihomo` / `surge`），嗅探失败默认 `singbox`。
- 输出 = **模板渲染**：模板是每格式一份完整配置文件（Clash YAML / sing-box JSON），使用 `{{nodes}}`、`{{rules}}` 占位符；同一模板可被多个订阅复用。
- **模板中禁止出现可手填的凭据**：节点数据统一由 `{{nodes}}` 注入，密码来自全局设置。这样"轮换密码"才不会漏改。
- 面板功能：模板编辑器（含语法校验 + 预览渲染结果）、订阅的节点多选、UA 过滤、一键轮换 token、访问日志（时间/IP/UA）。
- 节点渲染为 anytls outbound：`server`（探针主 IP 或你指定的域名）、`server_port`、`password`、`tls.certificate`（内嵌 PEM pinning）、`tls.server_name`。

### 10.1 共享密码的取舍 ⚠

所有订阅共用一个 anytls 密码 → **订阅 token 只能吊销 URL，吊销不了代理访问**。真泄密时的补救链路是：

```
面板「轮换代理密码」→ 更新 settings → 批量下发所有节点 desired state
                    → agent 重载 sing-box（不断开现有连接，新连接用新密码）
                    → 重新渲染所有订阅 → 客户端重新拉订阅
```

面板必须在轮换对话框里写明："此操作会要求所有客户端重新拉取订阅，旧密码立即失效"。建议也顺手实现"节点级密码覆盖"字段，方便你以后想隔离时不必重构数据模型（v1 UI 可以先不暴露）。

---

## 11. Web 终端（2026-09-15 修订：单一 Agent PTY，SSH 模式与凭据托管移除）

- 前端 xterm.js ↔ `/ws/terminal?node=<id>` ↔ server 转发 ↔ agent；agent 收到 `terminal_open` 一律用 `creack/pty` 起本地 shell，**不再内置 SSH 客户端、不依赖探针 sshd、不在面板保存任何 SSH 凭据**。适用于禁密码登录的 VPS、dropbear 行为怪异的 OpenWrt。
- 代价绑定：探针必须能起 PTY（容器内挂载 devpts）；无法再"借道"探针上已有的 sshd 配置。
- 兼容：`TerminalOpen.Mode` 字段保留在 v1 wire 上（server 恒写 `pty`）；旧浏览器发来的 `ssh` 由 server 规范化为 `pty`，旧 agent 收到 `ssh` 也只起本地 PTY。旧 `node_ssh` 凭据表在服务端启动迁移时删除。
- 会话初始化时记审计：操作者、节点、来源 IP、会话 ID、开始/结束时间。浏览器帧限 1 MiB，会话 ID 由 server 生成并覆写，浏览器不能伪造。
- 终端与 AI 执行共用 agent 指令通道 → 审计口径统一。
- 终端页三个盒子（2026-09-15 修订）：卡片头一行（左标题「Agent Web 终端」，右会话 ID +「重连」+ 连接状态点）→ `.terminal-shell`（深色屏幕框，8px padding 让文字不贴边）→ `.terminal-host`（FitAddon 的量测盒）。卡片自身用默认面板底色，屏幕是页面上唯一的深色面；页面不再另起标题。
- **padding 不能放在 `.terminal-host` 上**：xterm 的 `.xterm` 是绝对定位盒，绝对定位子元素相对宿主 **padding box** 定位，`inset: 0` 会把字形区直接铺满 padding、把留白盖掉（这就是"padding 设了却依然贴边"的成因）。所以外框（padding/border/深色底）与量测宿主必须是两个元素。
- `.terminal-host` 高度**必须由卡片所在的网格行决定、不能被 xterm 内容撑高**：宿主 `flex: 1 1 auto`（不写内容高度）、内部 `.xterm` 绝对定位填满，否则每次 fit 都会把外框的 padding+border 折成行数加回去，现象是终端每帧长高一行；fit 只由 rAF 防抖的 `ResizeObserver` 触发（`onResize` 里调 fit 会同步递归），宿主不可见（宽高 ≤0）时跳过 fit，避免把活着的 PTY 缩成 2×1。
- v1 不做 PTY 全量录制（体积与隐私成本高），但保留 `session_id`，便于后续开启录制。

---

## 12. AI 助手

### 12.1 形态

- 服务端代理到 OpenAI 兼容 API：`base_url`、`api_key`（AES-GCM 加密存 `settings`）、`model` 均由面板配置；支持流式输出。
- 三键缺一即视为**未配置**（2026-09-15 修订）：判定收敛为服务端唯一函数，`GET /api/settings` 额外返回派生字段 `ai_configured`（不是 setting，不受 `allowedKeys` 影响），终端页只在它为 `true` 时渲染 AI 侧栏——否则侧栏每次提交都只会拿到 503 `ai_not_configured`，不如不显示，终端独占整宽。
- 上下文注入（默认）：节点列表、当前指标、流量与配额、延迟、sing-box 版本与状态、最近告警。
- **默认不注入原始日志**：需要时由你在对话里显式打开"附带日志（最近 N 行）"开关。这既减少 token，也显著缩小注入面——**但不改变你选的默认放行语义**。

### 12.2 工具集

| 工具 | 类型 | 说明 |
|---|---|---|
| `list_nodes` / `get_metrics` / `get_traffic` / `get_latency` | 只读 | 结构化查询 |
| `get_singbox_status` / `tail_logs` | 只读 | 需显式开关日志 |
| `restart_singbox` / `stop_singbox` / `start_singbox` | 变更 | 走同一闸门 |
| `install_singbox` / `update_singbox` | 变更 | 指定版本 |
| `set_singbox_port` / `set_anytls_password` | 变更 | 影响面大 |
| `run_shell` | 变更 | 裸 shell |

**不在工具集里**：§9.5 的服务端产物缓存与**一键批量更新**接口（`/api/singbox/cache*`、`/api/singbox/update*`、`DELETE /api/singbox/versions/{version}`）。它们影响面覆盖全部节点、且会写服务端缓存，不适合落进"默认放行"的执行路径；单节点更新仍走上面的 `update_singbox`（需显式版本）。

**agent 自更新（§5.5）不新增任何工具**（连只读的也不加）：期望版本、计划时刻、尝试次数与失败原因随既有节点查询一并返回，AI 看得到但做不了——不能触发、冻结、解锁或改 target。系统自动跟随是服务端版本变更触发的行为，不在模型的可达范围内。

### 12.3 执行策略（⚠ 你签下的风险）

**你的选择：默认放行。** 即：

- 模型返回的 tool_call **直接执行**；
- 仅当**模型自己**把该动作标记为 risky 时，前端弹出确认框展示完整命令；
- 确认框不提供"记住选择/不再询问"（不给攻击者一次性扩权到永久的机会）。

我按你的决定实现，同时**无条件**加上这几条补偿控制（它们不改变默认体验，成本极低）：

1. **全量审计**：每条命令的原文、模型给出的理由、风险标记、结果全部落 `audit_logs`；
2. **全局 Kill Switch**：面板一键冻结所有 AI 执行（CLI 也能开），冻结后 AI 退化为只读；
3. **速率与影响面熔断**：单节点每分钟命令数上限、连续失败自动暂停该节点的 AI 执行；
4. **元操作强制确认**：涉及面板密码、主密钥、AI 自身配置、**由 AI 发起的** agent 自更新的动作，无论模型怎么判定都强制确认（防"AI 给自己扩权"）。注意区分：§5.5 的**系统自动跟随**不在这一列——那是服务端版本变更触发的系统行为，没有人在点它；实际实现里 AI 侧连写工具都没有，只有只读查询（§12.2）。
5. `run_shell` 的 stdout/stderr 截断入审计，防止把凭据回灌进上下文。

### 12.4 已知未缓解风险（写在这里以便你日后反悔）

> **间接提示注入 → 静默执行 → 探针 root。**
> 攻击者只要能连上任意一个代理节点，就能影响探针上的日志/进程名/连接来源等文本；这些文本一旦进入模型上下文，模型可能被诱导调用 `run_shell`。由于默认放行，该命令会被静默执行，且以 root 身份。攻击者在拿下第一台探针后，可以横向影响你在面板里配置的其他节点。
>
> 缓解到"白名单外默认确认"只需改一个配置项（`ai.default_policy: allow|confirm`），数据模型与工具集都不用动。建议你至少在暴露面扩大（加节点、给别人用订阅）之前重新评估一次。

---

## 13. 延迟测量

- 目标（面板配置，可复用）：`name` + `kind` + `host` + `port`。
  - `icmp`：ICMP echo RTT。优先 raw socket（root 或 `CAP_NET_RAW`）；其次 Linux 内核 ping socket（`SOCK_DGRAM`，仅当 `net.ipv4.ping_group_range` 放行当前组）；两者皆不可用才跳过并标记（实现修订 2026-09-16）。
  - `tcp`：TCP 三次握手 RTT（连接目标 host:port 后立即关闭）。
- 每台探针选择自己用哪些目标（多选）。
- 采集：**探针本地按全局设置 `latency.interval_seconds` 测量，默认每 5s 一次**（允许 1–3600 秒），本地保留 60s 窗口并**每 60s 批量上报**。设置变更立即推送给在线探针，离线探针会在下一次 `hello_ack` 收到；不是逐点上报，否则 10 探针会出现 2 次/秒的常驻写入，且网络抖动会污染测量本身。
- 存储：`latency_samples` 保留 7 天。
- 数据量估算（默认 5s）：5s × 7d = 120,960 点 / (探针×目标) 对；10 探针 × 5 目标 ≈ 600 万行，SQLite 可承受（每行 ~40B，约 250MB 量级）。更短的频率会按比例增加数据量。**图表必须降采样**：1h 视图取原始点，24h/7d 视图按 5min/30min 桶取平均 + P95。
- 前端：折线图 + 丢包率；每探针一张"多目标对比"图。

---

## 14. IP 检测与国旗

- agent 首次启动、网络变化（每 5 分钟检查 IP 集合是否有变化）、以及面板手动触发时，枚举本机地址（`net.Interfaces`，排除 loopback/link-local）→ **全量上报**。
- 服务端判定：
  1. 本地 GeoLite2-Country MMDB（离线，面板可上传更新，**也会自动下载**——见 §14.1）；
  2. 未命中且可配置在线 API（ip-api / ipinfo，可选配 key）作为回退。
- 国旗 = ISO 3166-1 alpha-2 → emoji/图标资源（前端内置，不依赖 CDN）。
- 主 IP 选择：默认第一个公网 IPv4，否则第一个公网 IPv6；面板可手动指定（`node_ips.is_primary` + `manual_primary`），订阅渲染使用主 IP（或你填的域名）。**手动主 IP 的口径（修订 2026-09-15）**：`nodes.primary_ip` 是列表/卡片/订阅实际读取的字段，手动选择必须落在它上面——`ReplaceNodeIPs` 在每次全量上报时若手动选择仍在上报集内，就把 `nodes.primary_ip` 同步写回该地址（同事务）；hub 只在当前 primary 为空或不在上报集内时才重新指认（含 agent 未标 primary 的回退分支）。此前实现只在 `node_ips` 里保住标志、不同步 `nodes.primary_ip`，被旧版覆盖逻辑带偏的值（仍在上报集内）永远不会自愈，表现为列表一直显示 agent 自选地址。
- **手动国旗（修订 2026-09-16）**：geoip 对 CDN/中转/隧道后的机器经常判错，且主 IP 一变（换网、手动换主 IP）旗子就跟着错，所以国旗要能手动改。编辑服务器页新增「国家 / 地区」字段，走 `PATCH /api/nodes/{id}` 的 `country_code`：填两位字母即钉住（`nodes.country_manual=1`，镜像 `manual_primary` 的先例），此后主 IP 变化/重新指认**不再**回退为 IP 库结果——`countryFor` 遇 manual 直接短路，且 `SetNodePrimaryIP` 在 SQL 里 `CASE WHEN country_manual=1` 保留原值，"手动优先"是行本身的性质，不依赖每个调用方记得判；清空提交即取消钉住并**当场**用当前主 IP 重查一次（`GeoIPResolver` 未命中则保留现值），"回到自动"不用等下次 IP 变化。校验只认 `[A-Z]{2}`，其余回 `invalid_country_code`。schema 加列 `nodes.country_manual`（SchemaVersion 2→3）。

### 14.1 数据库的自动下载与更新（实现修订 2026-09-15）

本节原本只有"面板可上传"一条路径：**安装即空白，除非操作者自己找一个 MMDB 传上去**。补齐后，数据库自己有生命周期，上传降级为离线逃生口。

- **来源：只用免密钥镜像，不碰 MaxMind License Key。** 默认按序尝试三个镜像（`raw.githubusercontent.com/P3TERX/GeoLite.mmdb` → `cdn.jsdelivr.net` 同一文件 → `Loyalsoldier/geoip` 最新 release），都是官方 GeoLite2-Country 数据的自动构建；挂一个还有下一个。**不做 License Key 支持**——MaxMind 官方下载要注册账号，让"开箱可用"依赖一个凭据是反向取舍。内网/离线部署用 `geoip.url`（或 `FOBE_GEOIP_URL`，后者优先）钉一个 URL，钉了就不回退到内置镜像：操作者既然配了私有镜像，静默回落到 GitHub 会绕过他的网络策略。
- **节奏：每天检查一次，数据库文件超过 `geoip.max_age_days`（默认 7 天，MaxMind 每周更新）就换。** 检查本身是一次本地 `stat` 与一次设置读取，只有过期才会联网；`0` 表示不做定期更新，只在文件缺失时下载。面板上的 `geoip.auto_update` 开关（缺省开）管的是这条路；`FOBE_GEOIP_AUTO_UPDATE=0` 是运维的硬闸——连启动时补缺都不做（手动更新与上传不受影响），此时面板开关置灰并说明原因。
- **安装必须原子且必须能解析**。MMDB **没有文件头 magic**：真实文件以搜索树开头、以 `\xab\xcd\xefMaxMind.com` 元数据标记结尾。此前上传接口检查的是前缀 `MMDB`——没有任何真实数据库长这样，所以**每一次合法上传都被拒**（测试用造出来的假头，掩盖了这一点）。现在统一走 `geoip.WriteMMDB`：临时文件 → 尺寸上限（64MB）→ `geoip2.Open` 真解析（空搜索树也算无效）→ `chmod 0644` → `rename`。下载与上传共用这一条，任何一步失败都**不会**动到线上文件（`Ensure` 的测试专门盯这一点：所有镜像都失败时，磁盘上的旧库必须还在）。
- **进度与结果都可见**：更新阶段（`connecting` / `downloading` / `verifying` / `done` / `failed`）经 `/ws/events` 的 `geoip_update` 事件推送（字节级 250ms 节流，相位变化立即推），并作为 `download` 快照挂在 `GET /api/geoip/status` 上——中途打开页面也有进度条。落库的只有 `geoip.status`（状态、来源 URL、上次成功/检查时间、失败原因），字节计数**只存内存**，理由同 §9.5.3：每秒多次写 SQLite 会撞 §5 的单写者。
- **手动更新是异步的**：`POST /api/geoip/update` 只启动后台任务并回 `202`（`accepted:false` 表示已有一个在跑，不是错误）。9MB 的下载不该占着一个 HTTP 响应，面板靠事件与状态端点收敛。
- **一个进程只有一个 MMDB 句柄**（`geoip.NewWith`）：hub 的国别判定与面板的状态视图共用它，更新成功后 `Reload()` 立即生效。此前 `cmd/server` 建了两个句柄，第二个要等自己的 stat 检查才发现文件变了。
- 设置页新增：状态两块（数据库状态、数据日期）、进度条、「立即更新」、「上传 MMDB」，以及三个设置项（自动更新、最长天数、自定义下载源）。改这三项会**当场**跑一次策略检查（`Manager.Check`），不必等下一个日切。来源 URL 与上次检查时间仍由 `GET /api/geoip/status` 返回（数据来源不占面板位置，需要时再加回）。

---

## 15. 告警

| 类型 | 触发 | 默认阈值 |
|---|---|---|
| 到期提醒 | `node_billing.next_due_at` | 提前 7 / 3 / 1 天 |
| 流量阈值 | 周期使用率 | 80% / 100% |
| 探针离线 | 90s 无心跳 | — |
| sing-box 异常 | 进程退出 / 端口不可连 / 回滚发生 | 立即 |
| sing-box 批量更新未收敛 | 分发后 15 分钟仍未报告目标版本（§9.5.4） | 15 分钟 |
| 计数器重置 | 流量计数回绕 | 立即（信息级） |

通道：**Telegram Bot** + **通用 Webhook**（JSON POST，HMAC 签名头）+ **飞书**（2026-09-16 修订）。

- 同类告警去重合并（同一节点同一类型 1 小时内只发一次，恢复时补一条 recovery）。
- 面板内"通知中心"保留全部历史，`alerts` 表为权威。

### 15.0 渠道与事件开关（2026-09-16 修订：通知独立成页）

设置里**通知不再是基础设置页上的一张卡片**，而是与「服务器」同级的一个子页（`/settings/notifications`，§16）：页面自上而下是**渠道**（Telegram / 飞书 / 通用 Webhook，同级并列，各自带开关、配置与"发送测试消息"）→ **事件开关** → **流量阈值**。

- **渠道开关**：`notify.telegram_enabled` / `notify.webhook_enabled` / `notify.feishu_enabled`。关掉只停推送、**不清空配置**（重新打开不用重填凭据），也不影响"发送测试消息"——测试走 `Deliver`，开关只管调度。`Configured()` 语义保持"有配置"，与开关正交。
- **事件开关**：`notify.event.<组>`，组是按操作者心智划分的（不是按 kind 一一对应）：`node_status`（探针上线下线，含恢复）、`traffic`（流量阈值：warn/crit）、`billing`（缴费到期：7/3/1 天与逾期）、`singbox`（sing-box 异常：退出/回滚）、`updates`（sing-box 批量更新未收敛、探针自更新失败/落后）、`counter_reset`（计数器重置）。未知 kind 归入 `updates`，保证新告警类型天然可关。
- **两侧都默认开**：设置项缺省（或值不可解析）= 开，否则一次升级就会静默掉已经生效的通道/事件。
- **关掉 ≠ 攒着**：被开关拦下的告警**直接出队**（`scheduler.deliverOne` 对"没有任何渠道接受"返回已处理）。取舍：重新打开开关不会补推关闭期间的历史——把静音期当成"待发队列"会在重新打开时一次性倒灌，那不是"别打扰我"的意思；`alerts` 表仍然是全量权威记录，面板告警页照常可见。某个渠道失败（非开关原因）仍然留在队列里下轮重试。
- 流量阈值（`alert.traffic_warn_pct` / `alert.traffic_crit_pct`，默认 80/100）也落在这一页——它们只被 `traffic` 事件使用，放在别处等于让用户满仓库找旋钮。

### 15.1 飞书通道（2026-09-16 修订）

两条子通道，共用 `internal/server/notify` 里的一个 `Feishu` 通知器（**应用模式优先于群机器人模式**，两者都配时只走应用）：

1. **应用机器人（推荐，支持"直接扫码接入"）**：面板调用 `POST https://accounts.feishu.cn/oauth/v1/app/registration`（RFC 8628 设备授权风格）拿 `device_code` + `user_code`，把 `verification_uri_complete` 渲染成二维码（前端用 `qrcode.react`，自托管、不引 CDN）。用户用飞书 App 扫码 → 在**自己的租户里确认创建一个自建应用**（`archetype=PersonalAgent`，预填应用名/描述，并经 URL 上的 gzip+base64url `addons` 预授权 `im:message:send_as_bot`）→ 面板轮询 `action=poll` 拿到 `client_id/client_secret` 与扫码人的 `open_id`，加密落库并把**扫码人本人**设为接收者。此后告警由机器人以私聊形式送达：`POST /open-apis/im/v1/messages`（`tenant_access_token` 进程内缓存，过期前 60s 刷新，token 失效自动换新重试一次）。若轮询响应里 `user_info.tenant_brand=lark`，后续请求整体切到 `accounts.larksuite.com` / `open.larksuite.com`（与官方 SDK 一致），域名记在 `notify.feishu_domain`。
- 代价：这一步**需要一个飞书账号并联网访问飞书的域名**（面板自己出网，不需要公网入站/回调地址，因此局域网部署也能用）；扫码确认页由飞书托管，**面板无法替用户跳过"创建应用"那一次确认**；会话是进程内状态（重启即失效，重新扫一次即可，不影响已绑定的配置）。手动填写 `App ID/App Secret/接收者 ID` 是同一条发送路径的备选入口（老应用、或因组织策略不便扫码的场景），接收者 ID 按前缀判定类型（`ou_` 用户 / `oc_` 群 / `on_` union），也可手填 `chat_id` 把告警发进群。
- 安全：`app_secret`、群机器人 webhook URL（路径里自带 token）与加签密钥都按 §4.4 经 `security.Cryptor` 加密存储、GET 不回读；解除绑定是**删除**设置行（空值仍会被当作"已配置"，见 store 的 `DeleteSetting`）。
2. **群自定义机器人**：在飞书群里添加"自定义机器人"拿到的 webhook 地址（+ 可选加签密钥）直接粘贴即可，不需要应用凭据；POST `{msg_type:"text", content:{text}, timestamp, sign}`，`sign` 为 HMAC-SHA256（**密钥是 `"<unix秒>\n<加签密钥>"`、消息为空**）、base64 编码，与 Telegram/通用 Webhook 的签名方案不同，别混用。

---

## 16. 前端

- 技术：**React + TypeScript + Vite**；构建产物输出到 `web/dist`。**生产镜像在构建阶段把产物烤进镜像**（`/srv/web`），由 server 直出（`FOBE_WEB_DIR`）——不嵌入 Go 二进制（避免体积膨胀），也不再依赖宿主机挂载前端目录。若想让外部 nginx 直接吐静态文件，见 §3 末尾的替代做法。
- **本地开发不走容器**：前端 `npm run dev`（Vite dev server，把 `/api`、`/ws`、`/sub`、`/install.sh`、`/dl` 代理到 `http://127.0.0.1:8080`，**WebSocket 代理必须开 `ws: true`**），后端 `go run ./cmd/server`。此时把 `FOBE_WEB_DIR` 留空 → server 进 **API-only 模式**：`/` 返回一句"请访问 Vite dev server"的提示（不 404、不白屏），其余接口行为与生产一致。
- **`scripts/dev.sh` 与自更新（2026-09-15 修订）**：dev.sh 现在除 server 外还交叉编译 linux/amd64 的 agent 产物，并给两端注入同一个内容寻址版本号 `dev-<哈希>`（§5.5 实现修订），所以本机 dev 也能真跑"探针跟随服务端"；`go run ./cmd/server` 没有 `-ldflags`、版本仍是 `dev`，不会下发目标。降级演练用 `FOBE_VERSION=<旧号> scripts/dev.sh`；改后端必须重启 dev.sh 这条老规矩不变，但版本号只在 agent 源码真的变了才变——重启本身不再惊动探针。
- 页面：登录 / 概览（卡片墙）/ 节点详情（**只读监控**：指标 + 图表 + 流量 + 延迟历史）/ 订阅与模板 / 延迟目标 / 告警 / 终端（全屏）/ AI 助手（侧栏）/ 设置（**通知**、AI、GeoIP、保留期、主密钥状态、**服务器**、**审计日志**）。（2026-09-15 修订；2026-09-16 修订：详情页「命令」卡片移除，见下；2026-09-16 修订：通知从基础设置页独立为子页，见 §15.0）
- **配置入口收敛（2026-09-15；2026-09-16 列完善）**：设置里新增「服务器」页，表格列出已接入的服务器，明确显示文字状态、主 IP、过期时间、版本，行尾为编辑/删除图标按钮；未配置缴费周期时过期时间留空，已配置时显示距离 `next_due_at` 的剩余或逾期时间。「编辑服务器」页集中承载该服务器的全部配置：节点设置（名称/备注/agent 上报的网卡下拉/统计模式/配额/只读时区）、**独立的流量周期**（无/按月/按年与秒级下次重置时间）、缴费周期、延迟测量端点选择、IP 列表（含手动主 IP）、sing-box 服务端配置（版本/启停/端口）。节点详情页不再承载配置表单与删除按钮——监控与配置分离，删除服务器统一走设置页（带确认）。
- **顶栏与表格的窄屏适配（2026-09-16）**：顶栏全部按钮带图标，退出/语言切换补上图标后与其他按钮一致；手机竖屏（≤640px 且 portrait）顶栏按钮只显示图标（文案保留在 `title`/`aria-label`），actions 禁止换行，sticky topbar 高度不被按钮文字撑高。设置→服务器列表的状态列（圆点+文字）`nowrap` 保证单行；编辑服务器页 IP 列表的超长 IPv6 在单元格内折行（`overflow-wrap: anywhere`，只有它会压缩 min-content 宽度），不再把 auto 布局的表格顶出卡片。基础设置页与订阅页里「卡片内裸表格」统一包进 `.table-wrap`（`overflow-x: auto`）——卡片既不裁剪也不滚动，宽行（审计日志、UA、mono 值）原本会画出卡片外，现在横向滚动由容器接管。
- **详情页「命令」卡片移除（实现修订 2026-09-16）**：面板不再提供对探针的任意 shell 下发入口——前端命令卡片与 `POST /api/nodes/{id}/commands` 路由一并删除。指令队列本体（`commands` 表、`enqueueCommand`、离线排队与幂等）保留：AI 执行（§12）与面板动作（sing-box 启停/重启）仍走同一条队列与审计；`GET /api/nodes/{id}/commands` 保留，供 AI 面板轮询执行结果。取舍：普通面板用户少一个"顺手敲 shell"的危险面，命令执行的入口收敛到 AI 确认流（§12.3）。
- **基本信息新增「发行版」（2026-09-16）**：agent 读 `/etc/os-release`（回退 `/usr/lib/os-release`，仍只做文件读取，遵守不调外部命令的约定）取 `ID` + `VERSION_ID`，随 `hello` 与注册请求上报，落 `nodes.distro_id / distro_version`（增量迁移，`SchemaVersion=2`）；详情页基本信息显示「Debian 13」式标签与自绘发行版徽标（内联 SVG、自托管，不引 CDN）。OpenWrt 衍生系统（iStoreOS、ImmortalWrt 等）改写 `ID` 但保留 `ID_LIKE="lede openwrt"`，按 `ID_LIKE` 归一化为 openwrt，并在 os-release 缺失时回退 `/etc/openwrt_release`。没有 `VERSION_ID` 的滚动发行版（如 Arch）只显示名称；未知 ID 回退首字母徽标，旧 agent 未上报时显示 `-`。
- **审计日志独立成页（2026-09-16）**：审计从基础设置页的卡片移出，成为设置的子页 `/settings/audit`（子导航入口「审计日志」），表格结构与 `GET /api/audit` 的其余字段不变。节点列显示节点名称而非 ID：`ListAudit` 读取时 `LEFT JOIN nodes` 带出 `node_name`（`COALESCE` 成空串，避免无节点记录的 NULL 扫描错误），改名后的历史记录显示当前名称，节点已删除时前端回退显示原始 ID；审计表本身不回写，无 schema 变更。
- 实时（2026-09-15 修订）：`/ws/events` 只覆盖状态类变化（节点增删改、sing-box、订阅、设置、GeoIP）——**常规指标上报不产生任何事件**，所以数据新鲜度必须靠「轮询 + 高频上报」两条腿：
  - **概览**：挂载且标签页可见期间，对每个在线节点打开 §16 的 5s 探测流，并 5s 拉一次列表。打开探测流是必要的：不打开就只能等 60s 基线节奏，卡片墙看起来像「不自动更新」。
  - **详情**：5s 拉节点快照（与探测流对齐），30s 拉图表 / 流量 / 延迟曲线（重查询）。
  - **标签页隐藏时全部交还 60s 基线并停轮询**（`visibilitychange`），避免一个忘记关闭的标签页把每个 agent 永久钉在 5s。离线节点 409 直接忽略；节点重新上线后会重新打开探测流。
- 节点卡片展示：名称、国旗、IP、CPU%、内存%、磁盘%、CPU 核数、流量使用率（按所选模式）、今日上下行、到期倒计时、延迟摘要。**连接状态改由卡片自身样式表达（实现修订 2026-09-16）**：卡片头右侧的位置改为发行版徽标（复用详情页的自绘 SVG，未知/未上报 ID 回退首字母徽标），不再画在线状态小圆点；离线卡片红边框 + 底色红色斜纹（`--red-weak`，明暗主题各有一档），状态文字保留在卡片 `title` 提示里。
- **i18n**：`zh-CN` + `en-US`，前端 i18n 框架；后端只返回错误码与结构化数据，**不出中文文案**（避免后端拼字符串导致双语文案漏翻）。
- **主题**：CSS 变量 + `prefers-color-scheme` 默认跟随系统 + 手动切换持久化（localStorage 与服务端设置双写，跨设备一致）。
- 全部静态资源自托管，不引 CDN（离线/内网可用）。

---

## 17. 部署与目录

```
fobe/
├─ cmd/
│  ├─ server/main.go          # 面板服务端 + scheduler + admin CLI
│  └─ agent/main.go           # 探针 agent
├─ internal/
│  ├─ protocol/               # 共享消息定义、版本、校验（server 与 agent 共用）
│  ├─ server/{httpapi,hub,store,quota,geoip,notify,scheduler,security,singbox}
│  │  ├─ singboxdl/           # §9.5 产物下载/校验/落盘（GitHub release → <DLDir>/singbox/<ver>/）
│  │  ├─ singboxcache/        # §9.5 启动自动下载策略 + 容器挂载校验 + 状态落库
│  │  └─ singboxupdate/       # §9.5 一键批量更新 job + 15 分钟收敛告警
│  └─ agent/{collect,singbox,latency,terminal,cert,service,netinfo}
├─ web/                       # React + TS + Vite（src/、vite.config.ts、产物 dist/）
├─ docker-compose.yml         # 仓库根：默认拉 GHCR 预构建镜像，--build 落回本地构建；零必填环境变量
├─ deploy/
│  ├─ Dockerfile.server       # 多阶段：node 构建前端 → go 构建 server/agent → 运行镜像（不含 nginx）
│  └─ docker-entrypoint.sh    # 主密钥解析（env > KEY_FILE > 自动生成进 /data 卷），admin 子命令直通
├─ .github/
│  └─ workflows/release.yml   # v* tag → gofmt/vet/test + 前端构建 → 镜像 → ghcr.io/<owner>/<repo>
├─ scripts/
│  ├─ build.sh                # 本地/CI 三件套构建（镜像内自行多阶段构建，不依赖它）
│  └─ install.sh.tmpl         # 安装脚本文本模板（服务端注入 token 后下发）
├─ data/                      # 宿主单一数据目录（gitignore；compose 挂为容器 /data）
│  ├─ fobe.db
│  ├─ .master_key             # 未显式设置主密钥时由入口脚本生成（§4.4）
│  ├─ geoip/                  # GeoLite2-Country.mmdb（§14.1，与 fobe.db 同卷）
│  ├─ dl/                     # 容器内 /data/dl：agent 产物（§5.5）+ sing-box 缓存（§9.5）
│  │  ├─ agent/<version>/     # 二进制 + .sha256 + manifest.json（首次启动由 seed 目录补进卷）
│  │  └─ singbox/<version>/   # 服务端自动下载或手动投放的 sing-box 产物
│  └─ backup/                 # 每日 VACUUM INTO 快照（容器内 /data/backup）
└─ docs/design.md
```

`docker-compose.yml` 要点（实现修订 2026-09-15：从 `deploy/` 移到仓库根，默认拉 GHCR 预构建镜像，本地构建作 `--build` 兜底；主密钥由镜像入口脚本自动生成进 `/data` 卷（§4.4）；同日把 `/data`、`/backup`、`/srv/dl` 三个挂载**合一为单一 `./data:/data` 卷**，DL 与备份成为卷内子目录——compose 里不再需要任何环境变量，镜像 `ENV` 自带全部固定路径）：

```yaml
services:
  server:
    image: ghcr.io/fonlan/fobe:latest   # v* tag 时由 .github/workflows/release.yml 推送
    build:
      context: .
      dockerfile: deploy/Dockerfile.server
      args: { VERSION: "${FOBE_VERSION:-compose}" }  # 本地构建专用；占位缺省会被镜像内换成内容寻址 compose-<哈希>（见下）
    ports:
      - "8080:8080"                # 默认全接口映射（明文 HTTP）；要收敛改回 127.0.0.1:8080:8080 或防火墙限来源
    volumes:
      - ./data:/data               # 唯一数据卷：db、.master_key、geoip/、dl/、backup/
    environment:
      FOBE_ADMIN_PASSWORD: "${FOBE_ADMIN_PASSWORD:-}" # 唯一保留的可选项；主密钥/镜像源等要设就在这里照样式加
```

- **没有 nginx 服务，也没有证书卷**：接入层完全外部化（见 §3 与 `README.md`）。
- **单一 `/data` 数据卷**（实现修订 2026-09-15：原先 `/data`、`/backup`、`/srv/dl` 三个挂载合一，DL 与备份成为卷内子目录，镜像 `ENV` 定为 `FOBE_DL_DIR=/data/dl`、`FOBE_BACKUP_DIR=/data/backup`；`VOLUME` 只声明 `/data`）。**DL 目录不能落在容器可写层**：那里的下载在升级/重建容器时全部丢失。挂载自检（`singboxcache/mount.go`）的判据是「`FOBE_DL_DIR` 的覆盖挂载（mountinfo 里最长的父挂载点）是否只是 `/`」——挂在 `/data` 卷下的 `/data/dl` 算通过；落在可写层才写 WARN 并置 `singbox.dl_mount_ok=false`（设置页标红）。症状与处置见 `README.md` 排障表。
- **agent 产物必须放在卷外的 seed 目录**（实现修订 2026-09-15）：compose 把 `./data` 挂到 `/data`，`FOBE_DL_DIR=/data/dl` 在卷内，而挂载在容器启动时就生效——镜像里 `COPY` 进卷的东西**运行时根本读不到**，所以标准部署下 `/dl/agent/<version>/linux-amd64` 默认 404，`/install.sh` 也拉不到 agent。镜像因此把产物放进 `/srv/agent-seed/agent/<version>/`（`FOBE_AGENT_SEED_DIR`，非卷路径），服务端启动时**复制进 DL 卷**并把 `agent/latest` 指向自己这一版。这跟 §9.5 的 sing-box 缓存是两件事：sing-box 的产物由服务端自己联网下载，agent 的产物只能来自镜像（服务端不会自己编译 agent）。
- **本地构建的 VERSION 缺省不再是恒定的 `compose`**（实现修订 2026-09-16）：占位值（空/`dev`/`compose`）会在 golang 阶段被换成**内容寻址**的 `compose-<哈希>`（不带 build-arg 直接 `docker build` 时是 `dev-<哈希>`）——先编一份占位 agent、取产物 sha256 前 12 位，机制与 `scripts/dev.sh` 完全一致（§5.5）。此前 `--build` 部署版本恒为 `compose`：既过不了 `IsReleaseVersion`，等值判定下也永远"已收敛"，探针**永不**跟随服务端；现在改 agent 代码后重新 `--build` 会真的换号并触发自更新，agent 源码没变的重 build 不换号。显式 `FOBE_VERSION=<真实版本>`（发布管线传 tag）原样注入，不受影响。
- **前端产物在镜像里，不挂载**：`Dockerfile.server` 的 node 阶段产出 `web/dist` 并 `COPY` 到 `/srv/web`。改前端 = 重新 `docker compose build`，不存在"改了源码忘了构建/挂载路径写错"这类事故。
- **`/api/*` 永远不吃 SPA 兜底**（实现修订 2026-09-15）：静态处理器是最后的兜底（`mux.HandleFunc("/", s.handleStatic)`），未匹配的 `/api/...` 现在直接回 `404 {"error":{"code":"unknown_endpoint"}}`，不再吐出 API-only/`index.html` 那一页。原因是这个组合会造成一个很难查的假象：**旧代码的 server**（没重启的 dev 进程、没重建的镜像、没重启的容器）遇到新前端调用的新接口，会以 `200 text/html` 应答，前端 `resp.json()` 解析失败——而 `apiErrorMessage` 把任何非 `ApiError` 都当成 `network_error`，于是面板报"网络错误,无法连接服务器"，把人往 DNS/防火墙方向带，实际连接完全正常。现在同类情况会明确说是"接口不存在(服务端可能是旧版本)"。前端侧也补了 `bad_response`：2xx 但非 JSON 的响应单独报错，不再伪装成网络故障。（副作用：因为有 `/` 兜底模式，Go 1.22 ServeMux 的自动 405 在 `/api` 下不会触发，方法/路径不匹配统一落到这个 404。）
- 注意：即使你从宿主机 `127.0.0.1` 发起请求，容器内看到的源地址通常是 Docker 网关（如 `172.17.0.1`），所以 `FOBE_TRUSTED_PROXIES` 默认包含 Docker 私网段。

- **构建管线**：`Dockerfile.server` 是多阶段构建——`node:22-alpine` 阶段构建 React 前端 → `golang` 阶段构建 `server` 与 `agent`（agent 交叉编译 `linux/amd64`，`CGO_ENABLED=0`）→ 运行镜像里同时含：server 二进制、`/srv/web`（前端产物）、`/srv/agent-seed/agent/<version>/`（agent 产物 + 带 sha256 的 `manifest.json`，启动时播种进 DL 卷，安装脚本与面板版本选择都读它）。`scripts/build.sh` 提供同一套产物的本地/CI 构建，供不进容器的开发方式使用。
- **发布管线**（实现修订 2026-09-15）：推 `v*` tag 触发 `.github/workflows/release.yml`——先过 `gofmt`/`go vet`/`go test ./...`/前端 `tsc+vite build`，全绿才用 buildx 构建 `linux/amd64` 镜像推送到 `ghcr.io/<owner>/<repo>`，标签 `{version, v<tag>, latest}`；`$VERSION` = tag 去掉 `v` 前缀，同时注入 server 与 agent。镜像默认单平台、关 provenance：探针产物本就只有 linux/amd64（§10），attestation manifest list 会让旧 docker 引擎匿名拉取失败。compose 的 `image:` 指向它，`docker compose up -d` 即用预构建镜像；首次发布是 GHCR 私有包，转公开或 `docker login` 后再 `up`。
- **备份**（实现修订 2026-09-16，默认开启）：启动即拍 + 每 24h 一轮 + 数据库迁移前必拍，`VACUUM INTO` 快照到 `./data/backup/fobe-<时间戳>.db`，保留 3 份；`FOBE_BACKUP_DIR` 换位置、`FOBE_BACKUP_DIR=off` 关闭。另提供面板导出/导入 JSON（不含凭据明文）。损坏恢复流程见 `README.md` 排障表。

---

## 18. 里程碑

| 阶段 | 交付 | 验收标准 |
|---|---|---|
| **M1 骨架** | 多阶段镜像（含 React 前端产物）+ compose + SQLite + 登录（含黑名单+CLI）+ agent 注册/心跳 + 节点列表 + **README 外部 nginx 接入文档** | 一条安装命令能在 Debian 上装出节点并出现在面板；按 README 配好外部 nginx 后可从公网访问；本地 `go run` + `npm run dev` 跑通同一套接口 |
| **M2 监控** | 指标采集 + 实时面板 + 7 天明细 + 日流量表 + 四模式配额 + 重置锚点 + 缴费周期 | 面板 CPU/内存/磁盘/流量数字与 `top`、机房账单对得上 |
| **M3 sing-box** | 安装/更新/回滚/启停 + anytls 自签 + 证书 pinning + 订阅与模板 | 从零到"手机能导入订阅并连通"；故意写坏配置能自动回滚 |
| **M4 运维面** | Web 终端（agent 本地 PTY）+ IP/国旗 + 延迟测量 + 告警 | 浏览器里能上探针改配置；离线节点 90s 内告警到 Telegram |
| **M5 AI** | 工具集 + 确认弹窗 + 审计 + Kill Switch + 熔断 | 用自然语言完成"看这台为什么负载高"和"把 sing-box 升到 x.y.z" |
| **M6 打磨** | i18n + 明暗主题 + OpenWrt 实机验证 + 备份恢复 + 文档 | x86 OpenWrt 软路由上完整跑通 M2–M4 |

每阶段可独立验收，M3 结束即具备"最小可用产品"价值。

> 实现修订 2026-09-15：**agent 自更新（§5.5）** 是 M6 之后的运维增强项，不属于任何里程碑的验收前提——它只在"探针换上新 agent 之后"才开始生效（存量探针需人工重装一次，见 §20.8）。

---

## 19. 还需你拍板的默认项

以下我按默认写进了方案，**没有异议就照此实现**：

1. 离线探针的指令入队后 TTL **10 分钟**，超时标失败（避免迟到指令突然生效）。
2. 探针防火墙：agent **尝试自动放行** sing-box 端口（ufw / firewalld / nft / fw4），失败则把需要你手动执行的命令原文返回面板。
3. 备份：每日 `VACUUM INTO` 快照保留 3 份 + 面板导出/导入。（实现修订 2026-09-16：由 14 压到 3，库体量随节点数增长，池子刻意压小；启动即拍 + 迁移前必拍见 §6/§17。）
4. agent 自更新：与 sing-box 同样三道闸门 + 版本显式指定。**（实现修订 2026-09-15：此条已被 §5.5 取代——agent 没有 `check`/观察期可用，实际是"下载 + sha256 + 旁路自检 + 原子替换"，且不保留 `.prev`；"跟随服务端"改为双向（含降级），触发是系统行为、不需确认，Kill Switch on 时冻结。）**
5. anytls 端口默认随机高位端口，面板可改。
6. **接入层完全外部化**：fobe 不碰 nginx、不签发也不续期证书；README 提供可直接复制的 nginx 配置（单上游 + WS 升级 + 真实 IP + 超时 + 上传体量）。
7. 证书 pinning 替代 `insecure`：订阅里内嵌证书 PEM。
8. 日志默认**不进** AI 上下文，需手动开关。
9. 节点级 anytls 密码覆盖字段：**数据模型预留**，v1 UI 不暴露。
10. v1 不做 TOTP（按你的选择），但 `users` 表预留 `totp_secret` 字段。
11. 首次启动密码：未设置 `FOBE_ADMIN_PASSWORD` 时生成一次性初始密码并打印到服务端日志，登录后强制修改；忘记密码用 `fobe-server admin reset-password`。**（实现修订 2026-09-14：`FOBE_ADMIN_PASSWORD` 从"仅首次生效"升级为密码准绳——每次启动都同步为该值，变更时吊销全部旧会话；不设置则不动现有密码。）**
12. 前端形态：**React + TS + Vite**；生产镜像内置编译产物，本地开发用 Vite dev server + `go run`（`FOBE_WEB_DIR` 为空时 server 进 API-only 模式）。
13. **主密钥零输入部署**（实现修订 2026-09-15）：镜像入口脚本在 `FOBE_MASTER_KEY` 与 `FOBE_MASTER_KEY_FILE` 都未设置时，自动生成随机 32 字节（base64）写进数据卷 `/data/.master_key`（0600）并复用——`docker compose up -d` 不再要求先填任何环境变量。要自己掌管密钥就显式设置 env；admin CLI 不经过密钥解析，逃生口不受影响。

---

## 20. 已接受的已知风险（签字区）

1. ⚠ **AI 默认放行 → 间接提示注入可静默执行任意 root 命令**（§12.4）。缓解路径已设计，改一个配置项即可收紧。
2. ⚠ **登录面只有密码 + IP 黑名单**，无第二因子。黑名单依赖你自备的 nginx 正确传递 XFF、且 `FOBE_TRUSTED_PROXIES` 与实际拓扑一致；**接 CDN 后忘记同步回源网段 = 要么封不到人，要么把真实用户全封了**。
3. ⚠ **共享 anytls 密码**：无法按订阅吊销代理访问，泄漏只能全局轮换（会打断所有客户端）。
4. ⚠ **指标只存 7 天**：7 天以外的曲线不可得（月曲线依赖永久日表，可信；但"上月某天下午的 CPU"查不到）。
5. ⚠ **不做自动停服**：配额超标不会自动止损，完全依赖告警通道可达。
6. ⚠ **agent 自更新不留 `.prev`**（§5.5）：没有本地回滚。旁路自检把"坏产物"挡在提交之前，但**自检过、`-run` 起不来**（只有 run 路径才用到的内核特性/配置）这种残余情形只能 SSH 上去跑面板给的重装命令。你选了省下 OpenWrt overlay 上的 10MB，代价就在这里。
7. ⚠ **Kill Switch on 期间探针不跟随**："一直跟着服务端走"有例外——冻结是全局止损闸，优先级高于跟随。恢复跟随要你手动关掉它。
8. ⚠ **存量探针必须人工重装一次**：今天已装的 agent 二进制里没有自更新代码，服务端下发 target 它也不认识（Go 忽略未知字段，照常跑）。面板按能力位把它标成"需人工重装"，不会自动跟上。
9. ⚠ **服务端版本号成了对外契约**：随便打一个版本号（含把 `VERSION` 改成别的时间戳）就等于让**全部探针换一次二进制**，而降级路径是自动化测试里最容易缺的那条。发版前想清楚这个数字。
10. ⚠ **混版窗口**：升级/降级过渡期一定是混版。承诺是"同大版本内双向兼容（新增字段可选、未知帧忽略）"，**不保证行为等价**。
11. ⚠ **DL 目录是产物单点**：`data/` 没挂载成持久卷（被重建容器清空）或换了机器，全部探针会停在原地并告警——fail-closed 不会把探针搞砖，但也绝不会跟上，直到你把产物补齐。
12. ⚠ **主密钥与密文同卷**（实现修订 2026-09-15，§4.4/§19.13）：零输入部署把自动生成的主密钥放在 `/data/.master_key`，与它加密的设置同一个挂载卷——能读卷的人（宿主机 root、备份文件拿到手的人）就能解密 AI key / Bot Token。换来的是不填任何变量即可启动。不接受这个代价：显式设置 `FOBE_MASTER_KEY`（env / secret），入口脚本就完全不碰磁盘。
