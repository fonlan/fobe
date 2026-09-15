# fobe

自托管的**单用户探针管理面板**：一条命令把探针装上，面板里就能看硬件/流量指标、管 sing-box（anytls）服务端生命周期、开 Web 终端、跑延迟测量、出订阅链接，还带一个能直接动手的 AI 助手。

- 单用户 + 单端口：整个系统对外只需要一个 HTTPS 端口。
- 探针只主动外连，**不需要在探针上开放任何管理端口**。
- **接入层不在本仓库内**：反向代理与证书由你自备的 nginx 负责。本文档第 [外部 nginx 配置](#外部-nginx-配置) 节给出可直接复制的配置。
- 完整设计（架构、数据模型、协议、里程碑、已接受风险）见 [`docs/design.md`](docs/design.md)。

## 组成

| 组件 | 说明 |
|---|---|
| `server` | Go 单二进制：API + WebSocket + 指标调度 + 订阅渲染 + AI 代理 + admin CLI。SQLite(WAL) 落在容器外的挂载目录。 |
| `agent` | Go 静态二进制（`CGO_ENABLED=0`，linux/amd64，musl 兼容）。装在探针上跑，root 权限，托管 sing-box、采集指标、跑延迟测量、提供终端通道。 |
| `web` | React + TypeScript + Vite 前端。**编译产物在镜像构建阶段打进镜像**（`/srv/web`），由 `server` 直出；不嵌入 Go 二进制。 |

```
浏览器 ──HTTPS──► 你的 nginx（TLS + 证书，自备） ──HTTP──► server:8080 ──┐
                                                                        │ WSS（探针主动外连）
                                                            探针 A/B/C ◄┘
```

## 快速开始

```bash
# 1. 主密钥（加密 AI key、Bot Token 等敏感设置用；丢了就得重填这些凭据）
mkdir -p data/sqlite data/dl data/backup
echo "FOBE_MASTER_KEY=$(openssl rand -base64 32)" > .env

# 2. 起服务（--build 会在镜像内编译 React 前端 + server + agent；server 只绑定回环，等你的 nginx 来接）
docker compose -f deploy/docker-compose.yml --env-file .env up -d --build

# 3. 直接验证服务活着（此时还没有 nginx）
curl -sS http://127.0.0.1:8080/api/health
```

首次启动若未设置管理员密码，`server` 会在日志里打印一个一次性初始密码：

```bash
docker compose -f deploy/docker-compose.yml --env-file .env logs server | grep -i "initial password"
```

想自己指定，就在 `.env` 里加 `FOBE_ADMIN_PASSWORD=...`。**该变量是管理员密码的准绳**：每次启动都会把密码同步成它的值（没变则不动；变了会更新密码并吊销所有旧会话）。不设置则保持现状；首次启动没有用户时若也未设置，会生成一次性初始密码打印到日志。忘了密码进不去：

```bash
docker compose -f deploy/docker-compose.yml --env-file .env exec server fobe-server admin reset-password
```

`sudo` 不需要也不应写在安装命令里：root VPS 直接执行即可；非 root 用户且机器安装了 sudo 时，安装脚本会自行提权。

## 外部 nginx 配置

### 前提

- 一个域名（例如 `panel.example.com`）解析到这台机器，证书你自己签发与续期（certbot / acme.sh / Caddy 都行，fobe 不参与）。
- `fobe-server` 监听在 `127.0.0.1:8080`（compose 默认绑定，见 `deploy/docker-compose.yml`）。
- **单端口**：全部路径都走同一个 `server` 块，不需要任何路径分发规则，也不需要 nginx `stream` 模块。

### 可直接复制的配置

放到 `/etc/nginx/conf.d/fobe.conf`（`conf.d` 在 `http` 上下文内，所以 `map` 放这里合法）：

```nginx
# WebSocket 升级映射：没有它，探针长连接与浏览器终端会在握手阶段被拒
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

server {
    listen      443 ssl;
    listen      [::]:443 ssl;
    http2       on;                 # nginx < 1.25.1 请改用: listen 443 ssl http2;
    server_name panel.example.com;

    ssl_certificate     /etc/letsencrypt/live/panel.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/panel.example.com/privkey.pem;
    ssl_protocols       TLSv1.2 TLSv1.3;

    client_max_body_size 32m;       # 模板 / GeoIP 库上传

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;

        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        proxy_set_header Upgrade    $http_upgrade;      # WSS / 终端
        proxy_set_header Connection $connection_upgrade;

        proxy_read_timeout 3600s;   # agent 心跳与终端会长时间静默
        proxy_send_timeout 3600s;
        proxy_buffering    off;     # 实时推送不被缓冲

        proxy_redirect off;
    }

    # 订阅：禁止任何中间缓存（换了节点却拿到旧配置，八成是这里）
    location /sub/ {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        add_header Cache-Control "no-store" always;
    }
}
```

### 每一条为什么必须有

| 指令 | 作用 | 配错的症状 |
|---|---|---|
| `Host $host` | 服务端据此生成安装命令与订阅里的域名 | 安装命令里出现 `127.0.0.1` 或容器名，探针装完连不上 |
| `X-Real-IP` + `X-Forwarded-For` | 登录失败黑名单判定真实来源 | 所有人被看成同一个 IP；一封就封掉全世界（或永远封不到人） |
| `X-Forwarded-Proto` | 生成 `https://` 链接、Cookie 安全标记 | 订阅链接变成 `http://`，部分客户端拒绝导入 |
| `Upgrade` + `Connection` + `proxy_http_version 1.1` | WebSocket 升级 | **能登录，但节点永远离线**，终端打不开 |
| `proxy_read_timeout 3600s` | 长连接静默期不被掐 | 节点每 60s 抖动掉线；终端开着不动几分钟自动断 |
| `proxy_buffering off` | 实时推送立即到浏览器 | 指标卡片好几秒才动一次 |
| `client_max_body_size 32m` | 模板/GeoIP 上传 | 上传报 413 |
| `/sub/` 的 `no-store` | 订阅不被缓存 | 面板改了节点，客户端拉到的还是旧的 |

### 三种拓扑

**A. nginx 在宿主机（推荐，就是上面的配置）—— 默认可用，无需改动。**

**B. nginx 在另一台机器或另一个 compose 网络**

```yaml
# deploy/docker-compose.yml
ports:
  - "0.0.0.0:8080:8080"        # 放开绑定
```

```bash
# 并且用防火墙只放行 nginx 的来源
ufw allow from <nginx-ip> to any port 8080 proto tcp
```

同时把 nginx 的 IP（或网段）加进服务端环境变量，否则黑名单会认错 IP：

```yaml
environment:
  FOBE_TRUSTED_PROXIES: "127.0.0.1/32,::1/128,172.16.0.0/12,<nginx-ip>/32"
```

**C. 前面再叠一层 CDN（Cloudflare 等）**

必须把 CDN 的**回源网段**加进 `FOBE_TRUSTED_PROXIES`，并让 nginx 用 `real_ip` 模块还原真实客户端地址。漏了这一步的典型后果：要么黑名单形同虚设，要么某个 CDN 节点 IP 被拉黑、整片用户一起被拒。

> 为什么默认值里会有 `172.16.0.0/12`：即使你从宿主机 `127.0.0.1` 发起请求，容器内看到的源地址通常是 Docker 网关（如 `172.17.0.1`）。服务端只信任 `FOBE_TRUSTED_PROXIES` 范围内上游传来的 `X-Forwarded-For`，范围外一律改用 socket 源地址。

### 可选：让 nginx 直接吐静态文件

如果你不想让 Go 进程转发静态资源，需要**自己产出前端文件**（`cd web && npm run build`，或从镜像里 `docker cp <容器>:/srv/web ./dist`），然后把 `location /` 换成 `root /path/to/dist; try_files $uri /index.html;`，但**必须保留**下面这些路径转发到 `http://127.0.0.1:8080`：

`/api/`、`/ws/`、`/sub/`、`/install.sh`、`/dl/`

## 对外路径一览

| 路径 | 用途 |
|---|---|
| `/` | 面板前端（SPA） |
| `/api/*` | 管理 API（会话 Cookie 鉴权） |
| `/ws/agent` | 探针长连接（WSS，节点凭证鉴权） |
| `/ws/terminal` | 浏览器终端（xterm.js） |
| `/ws/events` | 面板实时推送 |
| `/sub/<token>` | 订阅输出（`?format=singbox\|clash`，缺省按 UA 嗅探） |
| `/install.sh` | 动态渲染的探针安装脚本（内含注册 token 与 server 地址） |
| `/dl/*` | agent 与 sing-box 产物下载（探针拉取） |

## 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `FOBE_MASTER_KEY` | 无（**必填**） | 32 字节，AES-GCM 加密 AI key / Bot Token 等敏感设置。未设置时服务端拒绝启动 |
| `FOBE_ADMIN_PASSWORD` | 无 | 管理员密码准绳：设置后每次启动同步为该值（变更会吊销全部旧会话）；未设置则不动现有密码 |
| `FOBE_DB` | `/data/fobe.db` | SQLite 路径（放挂载卷，勿放容器层） |
| `FOBE_WEB_DIR` | `/srv/web` | 前端 `dist` 目录 |
| `FOBE_DL_DIR` | `/srv/dl` | agent 与 sing-box 产物目录（**必须是挂载卷/绑定挂载**，见「数据与备份」） |
| `FOBE_AGENT_SEED_DIR` | `/srv/agent-seed` | 镜像内 agent 产物所在目录（**卷外路径**）。服务端启动时把它复制进 `FOBE_DL_DIR/agent/<版本>/`，并把 `agent/latest` 指向自己这一版——compose 的 `/srv/dl` 绑定挂载会让镜像里放在该目录下的产物运行时读不到。置空即关闭播种 |
| `FOBE_SINGBOX_AUTO_DOWNLOAD` | `1` | 启动时若 `<FOBE_DL_DIR>/singbox` 里没有任何有效版本，后台自动下载当时的最新**稳定版**；已有缓存则完全不联网；置 `0` 关闭。失败不阻塞启动，只记 WARN 并把状态与原因写进设置页 |
| `FOBE_SINGBOX_API_BASE` | `https://api.github.com` | sing-box release 列表来源（GitHub 兼容 API）；镜像源/离线环境改这里 |
| `FOBE_SINGBOX_DOWNLOAD_BASE` | `https://github.com` | sing-box 产物下载根地址（asset 没带下载 URL 时用它拼路径） |
| `FOBE_GEOIP_MMDB` | `/data/geoip/GeoLite2-Country.mmdb` | 国别数据库路径。自动下载、手动上传与国别判定写/读的都是这个文件（放挂载卷，见「数据与备份」）。**默认值是容器内路径**——本机直接跑二进制时要指到可写目录（`scripts/dev.sh` 已设为 `./data/geoip/...`），否则自动更新会报 `mkdir /data: read-only file system` |
| `FOBE_GEOIP_AUTO_UPDATE` | `1` | GeoIP 库的自动更新总闸：每天检查一次，文件超过「最长使用天数」就从免密钥镜像重新下载；置 `0` 连启动补缺都不做（设置页的开关随之失效，但「立即更新」与上传仍可用） |
| `FOBE_GEOIP_URL` | 内置镜像链 | 钉死一个下载源（内网镜像/离线环境）。设置后不再回退到内置的三个 GitHub 镜像；设置页「自定义下载源」等价，优先级低于本变量 |
| `FOBE_GEOIP_ONLINE` | `0` | 本地库未命中时用 ip-api.com 在线兜底（会把节点 IP 发给第三方，默认关闭） |
| `FOBE_TRUSTED_PROXIES` | `127.0.0.1/32,::1/128,172.16.0.0/12` | 可信上游网段（决定 XFF 是否被采信，见上文拓扑 C） |
| `FOBE_LISTEN` | `0.0.0.0:8080` | 容器内监听地址（对外由 compose 的端口绑定决定） |

### 服务端访问网址（生成安装命令）

在面板 **设置 → 访问配置** 中填写 `server.public_url`，例如：

```text
https://panel.example.com
```

这个网址必须是探针实际能够访问的完整 `http://` 或 `https://` 地址。添加节点时生成的安装命令，以及直接访问 `/install.sh` 得到的脚本，都会优先使用这个地址。不要填写 `127.0.0.1`、`localhost` 或仅供浏览器本机访问的地址；否则探针会尝试连接自己所在的机器。

如果尚未设置，服务端会回退使用当前请求的 `Host` 和协议。在本地 Vite 开发代理或未正确配置 nginx `Host` 转发时，这个回退值可能是 `127.0.0.1:8080`，因此生产部署应先配置 `server.public_url`。

## 数据与备份

```
data/
├─ sqlite/            # 挂到容器 /data
│  ├─ fobe.db         # 全部状态；WAL 模式，勿多实例同时挂载
│  └─ geoip/          # 国别数据库 GeoLite2-Country.mmdb（自动下载 + 手动上传）
├─ dl/                # agent / sing-box 产物（容器内是 /srv/dl）
│  ├─ agent/<version>/    # agent 二进制 + manifest.json（首次启动从镜像的 /srv/agent-seed 复制进来）
│  └─ singbox/<version>/  # 面板可选的 sing-box 版本（服务端自动下载，或你手动投放）
└─ backup/            # 每日 VACUUM INTO 快照，保留 14 份
```

- `data/dl/`（容器内的 `/srv/dl`）**必须挂载**：它存放 agent 产物与自动下载的 sing-box 版本。跑在容器里且没有独立挂载点时，服务端会打一条 WARN 并在设置页标红（`singbox.dl_mount_ok=false`）——升级/重建容器会把这些版本全部丢掉。
- **sing-box 版本从哪来**：默认由服务端自己在启动时下载（仅当 `data/dl/singbox/` 里一个有效版本都没有；已有缓存则完全不联网）。也可以不联网：按上面的布局手动放一份 `singbox/<version>/{linux-amd64,linux-amd64.sha256,manifest.json}` 进去即可。想主动补一个版本，用设置页的「下载新版本」下拉（列的是上游 release，已缓存的那几条是灰的），或点「刷新版本列表」重新拉一次；下载中出现的那一行会就地显示阶段、字节与速度，失败的那一行保留原因并给重试图标。
- **发布与删除**：设置页 sing-box 区块一行一个本地版本，行尾两个图标按钮——「发布」把这一行的版本下发到所有已启用 sing-box 的节点（会先列出受影响节点并要求二次确认；15 分钟后仍未生效的节点汇总发一条告警），「删除」把该版本从服务端磁盘删掉（仍被某节点 `desired_version` 引用时需要确认）。旧版本不会自动删，列表里能看到每个版本的占用、下载时间与被多少节点引用。**同一个产物只会下载一次**：下载进行中「发布」置灰，另一个版本的下载请求会返回 `download_in_progress`。
- **GeoIP 国别库从哪来**：`data/sqlite/geoip/GeoLite2-Country.mmdb`（容器内 `/data/geoip/`，跟 `fobe.db` 同一个挂载卷，随备份一起走）。服务端每天检查一次，文件超过「最长使用天数」（默认 7 天）就从免密钥镜像自动重新下载——**不需要 MaxMind 账号或 License Key**；设置页 GeoIP 区块能看状态与数据日期，也能点「立即更新」或直接「上传 MMDB」。下载与上传装的是同一个文件，**新库解析失败就拒绝替换**，旧库继续用；更新成功后立即生效，无需重启。
- 恢复：停服 → 用快照替换 `data/sqlite/fobe.db` → 起服。
- 探针节点无需重建：agent 用落盘的 machine-id 重连即复用原节点。
- 换了 `FOBE_MASTER_KEY` = 已加密的 AI key / Bot Token 等敏感设置全部失效，需要重填（节点与指标数据不受影响）。

## 探针 agent 自动更新

探针**一直跟着服务端走**：服务端版本变了，探针就把自己的二进制换成与之匹配的那一版——**不看高低**，服务端回退时探针也跟着回退。不需要（也不能）逐台点确认。

- **前提是「服务端有个像版本号的版本」**：镜像构建时注入 `$VERSION`（compose 里的 `FOBE_VERSION`）。`scripts/dev.sh` 会自己造一个（内容寻址的 `dev-<哈希>`，见「本地开发」一节），所以本机 dev 就能试；手工 `go run ./cmd/server` 没有 `-ldflags`、版本是 `dev`，本地 compose 不设 `FOBE_VERSION` 时是 `compose`——这两个**不是发布形态**，服务端不会下发目标（面板会写明原因）。要让 compose 构建也参与，用 `FOBE_VERSION=20260101.000000 docker compose … build` 这类带数字的版本号。
- **版本号就是"该配哪一版 agent"**：两端由构建时同一个 `$VERSION` 注入。判据是**两端版本号是否相等**（不是谁更新），所以"随便打个号"或"两个不同的二进制用同一个号"都会直接改变全网探针的行为——发版前想清楚这个数字。
- **什么时候会动**：探针每次连上服务端握手时，服务端告诉它「该跑哪一版」，不一致就切换。服务端一重启，全部探针会同时确认一次，所以服务端会给每台排一个 **0–5 分钟内的错峰时刻**（否则就是几十台一起拉同一个 10MB 文件）；面板上能看到每台的「计划时刻」。
- **换的过程**：下载到二进制所在目录的临时文件 → 校验 sha256 → 先用 `-selfcheck` 跑一次新二进制（连上服务端完成握手、且**自己报告的版本号**与目标一致才算通过）→ 原子替换 → 主动退出，由 systemd / procd 拉起新版。**自检不通过就完全不碰线上二进制。**
- **不保留旧版本**（省下 OpenWrt overlay 上的十几 MB）：因此**没有本地回滚**。自检把坏产物挡在提交之前，但"自检能过、正式启动却起不来"这种残余情形只能 SSH 上去用面板给的重装命令重装。
- **什么时候不会动**：① 服务端版本不是发布形态（`go run ./cmd/server` 的版本是 `dev`，未设 `FOBE_VERSION` 的本地 compose 构建是 `compose`）；② `data/dl/agent/<版本>/` 里没有二进制或 `.sha256`（容器首次启动会从 `/srv/agent-seed` 播种，`scripts/dev.sh` 也会产，所以正常部署第 ① ② 条都不成立）；③ 面板里关掉了自动更新；④ **Kill Switch 打开时**（冻结优先于跟随）；⑤ 探针上既没有 systemd 也没有 procd（`nohup` 兜底模式没有 supervisor，"重启自己"无处落地），面板会把这类节点标出来。
- **失败怎么办**：产物类失败（sha256 不符 / 跑不起来 / 版本号对不上）让这台探针**停止重试**并立即告警；环境类失败（连不上服务端、`/dl` 404、磁盘空间不足）退避重试（1 分钟 → 1 小时）。同一个目标版本连续失败 3 次即熔断，在面板上点**重试**解锁。分发 15 分钟后仍未生效的节点会汇总成一条告警。
- **老探针要人工重装一次**：这条功能上线前装的 agent 二进制里没有自更新代码。面板会把它们标成「**需人工重装（不支持自更新）**」并给出一条重装命令（**设置 → 服务器 → 编辑 → Agent 更新**，点「重装命令」生成）——在那台机器上重跑一次即可，它会重绑到同一个节点（凭据与 machine-id 都还在）。这是正常的，不是故障。
- **看与操作的地方**：「**设置 → 基础设置 → Agent 更新**」是总开关与全局状态（服务端版本、几台待更新、未生效时的原因）；「**设置 → 服务器 → 编辑 → Agent 更新**」是单台视图（当前/期望版本、计划时刻、状态、失败次数与原因）+「重试更新」（熔断或人工干预后解锁，会给在线探针立刻推送一次）。服务器列表里版本号后面出现 `→ x.y.z` 就表示这台还没跟上。
- **开关**：面板「设置 → 基础设置 → Agent 更新」（默认开）。AI 助手**没有**任何与 agent 更新相关的工具：状态随节点查询一起返回，但触发、冻结、解锁、重试都只能由你在面板上操作。

## 构建与运行

```bash
# 生产：一条命令把 React 前端、server、agent 全部编进镜像
docker compose -f deploy/docker-compose.yml --env-file .env up -d --build

# 只改了前端源码：重建镜像即可（本机不需要装 Node）
docker compose -f deploy/docker-compose.yml --env-file .env build
```

镜像构建分三段（`deploy/Dockerfile.server`）：`node` 阶段编译 React 前端 → `golang` 阶段编译 `server` 与 `agent`（agent 交叉编译 `linux/amd64`，`CGO_ENABLED=0`）→ 运行镜像内含 server 二进制、`/srv/web`（前端产物）、`/srv/dl/agent/<version>/`（agent 产物 + sha256 清单，安装脚本与面板版本选择都读它）。

前端**始终以编译产物形态存在于镜像里**，不做宿主机挂载——避免"改了源码忘了重新构建"和"挂载路径写错"这两类事故。

## 本地开发（不走容器）

最快方式——一条命令起全栈（后端 API-only + Vite 前端，Ctrl+C 同时退出）：

```bash
scripts/dev.sh              # 后端 + 前端;开发密码 devpass123
scripts/dev.sh --reset      # 先清空开发数据库再起
scripts/dev.sh --no-web     # 只起后端,前端自己跑
```

`scripts/dev.sh` 除了 server，还会交叉编译一份 linux/amd64 的 agent 产物，并给两端注入**同一个内容寻址的版本号** `dev-<哈希>`（见「探针 agent 自动更新」）：先用固定占位版本编一份、对它取 sha256 前 12 位当版本号，再用这个号正式编一次。于是**本机 dev 也能真跑「探针跟随服务端」**：

- **只有 agent 编出来的二进制变了才换号**：改后端代码后重启 `dev.sh` 不会产生新版本，局域网里的探针不会白下一次 10MB；**只改注释/格式也不换号**（二进制没变，探针本来就不该动——这是内容寻址而不是"源码清单"的意义）。真正改了 `internal/agent`、`internal/protocol`、`cmd/agent` 的语义才会换号。
- **降级演练**：`FOBE_VERSION=<上一个号> scripts/dev.sh` —— 旧产物还在 `data/dl/agent/` 里（播种从不覆盖已有版本），探针会跟着降下去。
- **错峰**：默认 0–5 分钟；只有一两台探针时用 `FOBE_AGENT_UPDATE_STAGGER=1s scripts/dev.sh`，否则"还没到计划时刻"看起来就像"功能没生效"。
- **一次性**：`data/dl/agent/latest` 若是**真实目录**（以前手工放进去的产物），server 不会重指它（只认自己建的符号链接布局），`/install.sh` 与面板的「重装命令」会一直装那份旧二进制 —— 删掉它，下次启动 `dev.sh` 让 server 重建。
- **状态确认**：`设置 → 基础设置 → Agent 更新` 应显示已启用与 `dev-<哈希>` 版本号；显示 `not_released` / `artifact_missing` 就是上面某一条没成立。

也可以手动各起一个进程，改代码即热重载：

```bash
# 终端 1：后端（不设置 FOBE_WEB_DIR → API-only 模式）
FOBE_MASTER_KEY=dev-key-not-for-prod \
FOBE_DB=./data/dev.db \
FOBE_DL_DIR=./data/dl \
FOBE_GEOIP_MMDB=./data/geoip/GeoLite2-Country.mmdb \
go run ./cmd/server

# 终端 2：前端
cd web && npm install && npm run dev
```

> `dev.sh` 里的 `FOBE_MASTER_KEY` 是固定派生值，重启后开发库里加密过的设置仍能解开；生产环境必须换成 `openssl rand -base64 32`。

- `FOBE_WEB_DIR` 留空时 server 进 **API-only 模式**：`/` 返回一句提示（告诉你去访问 Vite dev server），而 `/api`、`/ws/*`、`/sub/*`、`/install.sh`、`/dl/*` 行为与生产**完全一致**。所以前端开发不需要 nginx，也不需要 Docker。
- 需要一个"看起来像生产"的整体时，仍然可以用 `docker compose up --build`；两种方式共用同一套接口契约。

`web/vite.config.ts` 的代理（**`ws: true` 不能漏**，否则终端与实时推送在开发环境连不上）：

```ts
export default defineConfig({
  server: {
    proxy: {
      '/api':        { target: 'http://127.0.0.1:8080', changeOrigin: true },
      '/ws':         { target: 'ws://127.0.0.1:8080',   ws: true },
      '/sub':        { target: 'http://127.0.0.1:8080', changeOrigin: true },
      '/dl':         { target: 'http://127.0.0.1:8080', changeOrigin: true },
      '/install.sh': { target: 'http://127.0.0.1:8080', changeOrigin: true },
    },
  },
});
```

> 本地装探针时，`--server` 要填**探针能访问到的地址**（局域网 IP 或内网穿透域名），不能填 `127.0.0.1`。

## 排障速查

| 症状 | 先查这里 |
|---|---|
| 能登录，节点永远离线 | nginx 的 `Upgrade`/`Connection` 头与 `proxy_read_timeout` |
| 终端打开几秒就断 | 同上 + `proxy_buffering off` |
| 本地开发时终端/实时推送连不上 | Vite 代理 `/ws` 没开 `ws: true` |
| 登录失败几次后自己也进不去 | 黑名单取错 IP；先 `docker compose exec server fobe-server admin unblock all` 自救，再核对 `FOBE_TRUSTED_PROXIES` |
| 改了节点，订阅还是旧的 | `/sub/` 被缓存（检查 `no-store`，以及客户端自身的缓存） |
| 模板/GeoIP 上传 413 | `client_max_body_size` |
| 点新按钮提示**「接口不存在(服务端可能是旧版本)」**或「服务端返回了非 JSON 响应」 | 前端是新的、后端是旧的：`scripts/dev.sh` 只在启动时 `go build` 一次（生产是镜像里的二进制），改了后端必须**重启 dev.sh**（生产则重建镜像/重启容器）。旧后端不认识新接口，会以 SPA 兜底页应答 |
| 安装命令里域名不对 | nginx 没传 `Host` |
| 装完 agent 连不上、反复重连 | 探针能否解析并连通你的域名（DNS 污染 / 出网限制）；`journalctl -u fobe-agent` 或 OpenWrt 上 `logread` |
| **升级容器后 sing-box 没了**（设置页版本列表空、节点更新失败） | `/srv/dl` 没挂成独立卷，重建容器把已下载版本留在了旧容器层。核对 `docker compose config` 里的 `./data/dl:/srv/dl`，以及设置页/日志里的 `singbox.dl_mount_ok=false` 警告；恢复做法是重新下载（设置页「重试」）或手动把产物放回 `data/dl/singbox/<version>/` |
| **探针版本一直没跟上服务端**（节点页显示期望版本 ≠ 当前版本） | ① 看期望版本旁边的原因：没下发（服务端版本是 `dev`、或 `data/dl/agent/<版本>/` 里缺二进制/`.sha256`）/ 已关闭开关 / Kill Switch 开着；② 看是否熔断（连续失败 3 次会停手，点「重试」解锁）；③ 产物缺失时确认容器里 `ls /srv/dl/agent/` 有该版本目录——挂载卷会遮蔽镜像内置产物，服务端启动时会补进卷，补不上就会有日志 |
| **节点页标着「需人工重装」** | 该探针的 agent 是这条功能上线前装的，二进制里没有自更新代码。用面板给的重装命令在那台机器上重装一次即可（之后它才会自动跟随） |
| **重装过了、版本还是旧的**（箭头 `旧 → 新` 不消失，但 `strings /usr/local/bin/fobe-agent \| grep -c selfcheck` 已经是新二进制） | 装的是文件、跑的还是旧进程：`systemctl status fobe-agent` 看 `Active ... since` 是不是刚才（不是 = 服务没被重启）。旧版 `install.sh` 的 `enable --now` 对已运行的服务是 no-op（已修：改用 `restart`）。立刻恢复：`sudo systemctl restart fobe-agent`；OpenWrt 用 `/etc/init.d/fobe-agent restart` |
| **某台探针 agent 反复重启、版本没变** | 属于熔断保护的对象：先看 `journalctl -u fobe-agent`（OpenWrt 用 `logread`）里的自检/替换错误，确认二进制落点与权限；连续失败到 3 次会停手，不再反复重启 |
| **拉不到 GitHub / 自动下载失败**（设置页显示失败原因） | 服务端出网受限。三选一：① 配镜像源 `FOBE_SINGBOX_API_BASE` + `FOBE_SINGBOX_DOWNLOAD_BASE`（GitHub 兼容即可）后点「重试」；② 手动把 `linux-amd64` 与 `linux-amd64.sha256` 放进 `data/dl/singbox/<version>/`；③ 用 `FOBE_SINGBOX_AUTO_DOWNLOAD=0` 关掉自动下载，完全手动管理。注意**校验失败会拒绝安装**（fail-closed），不会留半成品 |
| **GeoIP 库自动更新失败 / 国别显示为空** | 三个免密钥镜像都不通（内网出网受限）。做法：设置 → GeoIP 看失败原因，① 用 `FOBE_GEOIP_URL` 或设置页「自定义下载源」钉一个可达的镜像；② 手动下载 `GeoLite2-Country.mmdb` 后点「上传 MMDB」；③ 用 `FOBE_GEOIP_AUTO_UPDATE=0` 关掉自动更新，只留手动。上传/下载的都是 `FOBE_GEOIP_MMDB` 指向的同一个文件，校验失败会拒绝替换（旧库继续用） |
| **节点详情磁盘占用率恒 0 / 每日流量为空** | 两个旧版 bug（磁盘的挂载点/文件系统字段解析错位、没有 `node_network` 行时整条流量上报被丢弃），已修复。**服务端与探针 agent 都要升级**：磁盘在 agent 侧采集，旧 agent 上报的磁盘恒空；升级后新样本才有值（历史样本仍是 0） |
| 一键更新后个别节点没生效 | 离线节点要等重连后由 `hello_ack` 自动收敛（结果表里是"离线待生效"）；15 分钟后仍未收敛会发一条 `singbox_update_stale` 告警，逐台查 `journalctl -u fobe-agent` / agent 侧的 sing-box 日志 |
| **探针上 sing-box 装在哪 / 升级后路径变了** | agent 侧统一用 `/etc/one-sing/`：`sing-box`（二进制）、`config.json`、`cert/{cert.crt,private.key}`（与 one-sing.sh 同一套路径，便于互相接管）。从旧版本升级的探针会在 agent 启动时自动搬迁旧路径（`/usr/local/bin/sing-box`、`/etc/sing-box/…`）并删掉空目录，日志里是 `sing-box layout migrated`。机器上原本有 one-sing.sh 的 `one-sing.service` 时，agent 首次收敛前会**停掉并 disable** 它（两个 supervisor 抢同一个进程只会互相重启）。注意 `config.json` 由面板独占：继续用 one-sing.sh 加协议会互相覆盖，要共存请改路径 |
