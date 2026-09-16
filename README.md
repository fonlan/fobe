<div align="center">

<img src="web/public/logo.svg" alt="fobe logo" width="96" />

# fobe

自托管的**单用户探针管理面板**：一条命令把探针装上，面板里就能看硬件/流量指标、管 sing-box（anytls）服务端生命周期、开 Web 终端、跑延迟测量、出订阅链接，还带一个能直接动手的 AI 助手。

[![Release](https://github.com/fonlan/fobe/actions/workflows/release.yml/badge.svg)](https://github.com/fonlan/fobe/actions/workflows/release.yml)
[![Docker Image](https://img.shields.io/badge/image-ghcr.io%2Ffonlan%2Ffobe-2496ED?logo=docker&logoColor=white)](https://github.com/fonlan/fobe/pkgs/container/fobe)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

</div>

- 单用户 + 单端口：整个系统对外只需要一个 HTTPS 端口；探针只主动外连，**不需要在探针上开放任何管理端口**。
- **零配置启动**：`docker compose up -d` 就能跑；反向代理与 TLS 由你自备的 nginx 负责。
- 组成：`server`（Go 单二进制：API + WebSocket + 指标调度 + 订阅渲染 + AI 代理 + admin CLI，SQLite(WAL) 落在挂载卷）、`agent`（探针上的静态 Go 二进制，采集指标、托管 sing-box、提供终端通道）、`web`（React + TS + Vite，编译产物打进镜像由 server 直出）。镜像内置 agent 产物，探针安装与自更新都从面板 `/dl/` 拉取。
- 完整设计见 [`docs/design.md`](docs/design.md)，仓库结构与代码约定见 [`AGENTS.md`](AGENTS.md)。

```
浏览器 ──HTTPS──► 你的 nginx（TLS + 证书，自备） ──HTTP──► server:8080 ──┐
                                                                         │ WSS（探针主动外连）
                                                             探针 A/B/C ◄┘
```

## 使用

### 起面板

前提：一台装了 Docker 的机器，`docker` 与 `docker compose` v2 可用。

```bash
git clone https://github.com/fonlan/fobe && cd fobe   # 或只下载 docker-compose.yml 这一个文件
docker compose up -d                                  # 自动拉取预构建镜像 ghcr.io/fonlan/fobe
curl -sS http://127.0.0.1:8080/api/health             # 验证服务活着
```

- **管理员密码**：首次启动会在日志打印一次性初始密码：`docker compose logs server | grep -i "initial password"`。想自己指定就设 `FOBE_ADMIN_PASSWORD`（每次启动同步为该值，变更会吊销全部旧会话）。忘了密码：`docker compose exec server fobe-server admin reset-password`。
- **主密钥**：不设 `FOBE_MASTER_KEY` 时，入口脚本自动生成随机密钥落盘 `data/.master_key`（0600）并复用；**换了密钥，已加密的 AI key / Bot Token 全部失效，需要重填**。

### 接 nginx（TLS 自备）

fobe 不含反代/证书逻辑。把下面的配置放到 `/etc/nginx/conf.d/fobe.conf`，改掉域名与证书路径即可：

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

8080 不要裸奔公网：nginx 同机时把 compose 的 ports 改成 `127.0.0.1:8080:8080`，不同机就用防火墙只放行 nginx 来源。若 nginx 不与面板同机、或前面还有 CDN，要把上游网段加进 `FOBE_TRUSTED_PROXIES`，否则黑名单会认错来源 IP。

### 装探针

登录面板，先在 **设置 → 访问配置** 填 `server.public_url`（探针实际能访问的完整 `http(s)://` 地址，不要填 `127.0.0.1`），再添加节点，把生成的安装命令拿到探针上执行：

```sh
curl -fsSL https://panel.example.com/install.sh | bash -s -- --token <REGTOKEN> --server https://panel.example.com
```

默认以 root 安装；常规 systemd 主机可追加 `--unprivileged`，先做一次 root/sudo 引导（建 `fobe-agent` 用户、写 unit），之后日常以该用户运行。

### 日常操作

- **更新面板**：`docker compose pull && docker compose up -d`——探针会按 0–5 分钟错峰自动跟随服务端版本（双向跟随，服务端回退探针也回退），数据都在 `data/` 卷里，重建容器不丢。
- **卸载某台服务器的 sing-box**：设置 → 服务器 → 编辑 → 「sing-box 服务端」卡片点 **卸载 sing-box**（二次确认）。卸载是**期望状态**而不是一次性命令：探针离线时也照样记着，重连后自动停服并删掉服务单元、二进制、配置与自签证书。该节点随即从订阅里消失，之后的「批量更新 sing-box」也会跳过它；想重新启用就再点安装，入站端口沿用原来的（证书会重新签发，客户端需重拉订阅）。
- **端口转发（nftables）**：设置 → 服务器 → 编辑 → 「端口转发（nftables）」卡片里增删改探针上的转发规则。布局与 [nfpf.sh](https://github.com/fonlan/nfpf) 完全相同（`table ip nat` 的 `prerouting` DNAT + `postrouting` masquerade），所以**用那个脚本或手工 `nft` 加过的规则也会出现在这里，并且能在这里改**。列表默认读最近一次上报（探针离线也能看），点「从探针刷新」会立刻问一次探针；改动是在线探针一次往返内生效，离线则入队（10 分钟内探针上线后自动执行）。首次添加会自动建表建链、打开内核 IPv4 转发，并把规则写进 `/etc/nftables.conf`（与 nfpf.sh 相同的持久化文件——**手工维护过该文件的注意，它会被实况覆盖**）。备注（comment）会写进规则本身，位置与 nfpf.sh 相同；双引号在 nft 字符串语法里没有转义写法，因此备注不允许含双引号（反斜杠按原样保存）。**若保存后备注为空**，说明链路里还有旧二进制（面板后端或探针 agent 是加备注字段之前的构建，Go 会静默忽略未知字段）：重启后端（`scripts/dev.sh` 起的话必须重启，后端不热更）、并让探针更新到当前 agent 后重存一次——现在这种情况会在卡片上给出明确提示，不再静默。
- **出订阅**：订阅与模板页创建订阅后得到 `/sub/<token>` 链接，填进客户端即可拉取；链接**随时可在订阅行点「链接」再次查看/复制**，不是只显示一次。模板决定输出结构：`{{nodes}}` 注入节点，`{{rules}}` 注入同页「路由规则」卡片里对应格式的片段（片段自带 `-` 与缩进，模板只管外层的键）。订阅行还能绑定模板（不绑、或模板格式与客户端请求的格式不一致时用内置默认模板）；轮换 Token 会让旧链接立即失效。
- **备份**：整机状态就是 `data/` 一个目录（数据库、主密钥、GeoIP 库、产物缓存、快照），停服后拷走即可。快照默认开启（启动即拍 + 每 24h 一轮 + 迁移前必拍，保留 3 份）；恢复 = 停服后用快照替换 `data/fobe.db`。
- **登录失败自锁自救**：`docker compose exec server fobe-server admin unblock all`。
- **配通知**：设置 → 通知。三个渠道同级并列（Telegram / 飞书 / 通用 Webhook），各带启用开关与「发送测试消息」；飞书点「扫码接入」用飞书 App 扫码并在确认页创建应用即完成绑定，告警由该机器人私聊发给你（面板只出网、不需要公网回调地址），不想建应用就用群自定义机器人 Webhook。下方事件开关按类型决定推不推（探针上线下线、流量阈值、缴费到期、sing-box 异常、更新异常、计数器重置），流量阈值也在这一页。关掉开关只停推送、不清配置，也不补推关闭期间的历史。

## 开发

```bash
scripts/dev.sh              # 一条命令起全栈：后端 API-only + Vite；开发密码 devpass123；Ctrl+C 一起退
scripts/dev.sh --reset      # 先删 data/dev.db 再起
scripts/dev.sh --no-web     # 只起后端
go test ./...               # 全部测试
gofmt -l internal cmd       # 必须无输出
go vet ./...
cd web && npm run build     # 前端 tsc 类型检查 + 构建
```

- `dev.sh` 会交叉编译一份 linux/amd64 agent 产物，并给 server/agent 注入**同一个内容寻址版本号** `dev-<哈希>`，本机就能真跑「探针跟随服务端」：`FOBE_VERSION=<旧号> scripts/dev.sh` 做降级演练，`FOBE_AGENT_UPDATE_STAGGER=1s scripts/dev.sh` 把 0–5 分钟错峰压成立即。**后端不热更新，改了后端必须重启 dev.sh。**
- 不用 dev.sh 时手动各起一个进程：后端 `FOBE_MASTER_KEY=dev-key-not-for-prod FOBE_DB=./data/dev.db go run ./cmd/server`（不设 `FOBE_WEB_DIR` 即 API-only 模式，`/api`、`/ws`、`/sub`、`/install.sh`、`/dl` 行为与生产完全一致）；前端 `cd web && npm install && npm run dev`（Vite 已代理上述路径，其中 `/ws` 的 `ws: true` 不能漏）。
- 本机出镜像：`docker compose up -d --build`；不出容器用 `scripts/build.sh [outdir]` 产三件套（server、linux/amd64 agent + manifest、web dist）。
- 发版：推 `v*` tag，CI 跑完测试自动构建镜像推到 GHCR（[`.github/workflows/release.yml`](.github/workflows/release.yml)）。
- 端口转发的真机验证（需要 root 与可抛弃环境，会清空 `table ip nat`）：`docker run --rm --privileged -v "$PWD":/src -w /src golang:1.25 sh -c 'apt-get update -qq && apt-get install -y -qq nftables >/dev/null && FOBE_NFT_TEST=1 go test ./internal/agent -run TestForwardsNftKernel -v'`。
- 本地装探针时 `--server` 要填**探针能访问到的地址**（局域网 IP 或内网穿透域名），不是 `127.0.0.1`。

## 环境变量

标准部署**无需设置任何变量**；要调整就在 `docker-compose.yml` 的 `environment` 段加一行。

| 变量 | 默认 | 说明 |
|---|---|---|
| `FOBE_ADMIN_PASSWORD` | 无 | 管理员密码准绳：每次启动同步为该值（变更吊销全部旧会话）；未设置则不动现有密码 |
| `FOBE_MASTER_KEY` | 无（自动生成） | 32 字节主密钥，AES-GCM 加密 AI key / Bot Token 等敏感设置；缺省由入口脚本生成到 `data/.master_key`；服务端缺密钥拒绝启动 |
| `FOBE_MASTER_KEY_FILE` | 无 | 从文件读主密钥（Docker secret）；与 `FOBE_MASTER_KEY` 同设时 env 优先 |
| `FOBE_TRUSTED_PROXIES` | `127.0.0.1/32,::1/128,172.16.0.0/12` | 可信上游网段，决定 XFF 是否被采信；nginx 不同机或前面有 CDN 时必须加上游网段 |
| `FOBE_LISTEN` | `0.0.0.0:8080` | 容器内监听地址（对外暴露面由 compose 的端口绑定决定） |
| `FOBE_DB` | `/data/fobe.db` | SQLite 路径（放数据卷内，勿放容器可写层） |
| `FOBE_WEB_DIR` | `/srv/web` | 前端 dist 目录；留空进 API-only 模式 |
| `FOBE_DL_DIR` | `/data/dl` | agent / sing-box 产物目录；必须落在挂载卷内，否则升级/重建容器即丢 |
| `FOBE_AGENT_SEED_DIR` | `/srv/agent-seed` | 镜像内 agent 产物位置，启动时播种进 `FOBE_DL_DIR`；置空关闭播种 |
| `FOBE_VERSION` | `compose` | 仅本地构建生效：注入 server/agent 版本号（镜像内会换成内容寻址的 `compose-<哈希>`）；显式设置可钉住版本做降级演练 |
| `FOBE_BACKUP_DIR` | `/data/backup` | 快照目录；`=off` 关闭快照 |
| `FOBE_SINGBOX_AUTO_DOWNLOAD` | `1` | 启动时若无任何本地版本则自动下载 sing-box 最新稳定版；`0` 关闭（也可手动投放 `data/dl/singbox/<version>/`） |
| `FOBE_GEOIP_MMDB` | `/data/geoip/GeoLite2-Country.mmdb` | 国别库路径（本机直接跑二进制时指到可写目录） |
| `FOBE_GEOIP_AUTO_UPDATE` | `1` | GeoIP 库每日检查、超期自动重下（免密钥镜像，不需要 MaxMind 账号）；`0` 关闭 |
| `FOBE_GEOIP_ONLINE` | `0` | 本地库未命中时用 ip-api.com 在线兜底（会把节点 IP 发给第三方，默认关闭） |

内网镜像/离线环境：`FOBE_SINGBOX_API_BASE`、`FOBE_SINGBOX_DOWNLOAD_BASE`（GitHub 兼容 API 与产物下载根地址）与 `FOBE_GEOIP_URL`（钉死 GeoIP 下载源）可改下载来源。

## 许可

[MIT](LICENSE)
