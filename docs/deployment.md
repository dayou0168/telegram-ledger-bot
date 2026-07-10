# Telegram 记账机器人 Go v2.3 部署运维

当前发布目标是 Go v2.3。生产部署使用 GHCR 预构建镜像、PostgreSQL 和共享链上监听服务 `ledger-chain-watcher`。服务器上不需要源码构建作为默认路径。

## 部署基线

- 机器人镜像：`ghcr.io/dayou0168/telegram-ledger-bot-go:2.3`
- watcher 镜像：`ghcr.io/dayou0168/telegram-ledger-chain-watcher:2.3`
- 数据库：每个机器人实例独立 PostgreSQL 16
- 链上监听：多个机器人共享 `ledger-chain-watcher`，watcher 使用独立 PostgreSQL 保存订阅、匹配事件和投递游标
- 推荐入口：宝塔 Docker Compose
- watcher 部署模式：Compose 版是在同一个 Compose 项目里运行 `chain-postgres` 独立容器和 `ledger-chain-watcher` 独立容器；宿主机 systemd 版适合 PostgreSQL 已在宝塔/宿主机安装的服务器
- 推荐配置：4 核 8G 起步；多机器人或链上监听压力高时优先增加 CPU、内存和 NVMe
- 网页账单和后台：每个机器人实例独立域名或独立宿主端口

## 准备 Telegram Bot

在 BotFather 创建机器人，并关闭隐私模式：

```text
/newbot
/setprivacy -> 选择机器人 -> Disable
```

私聊机器人发送 `我的ID` 可以获取 Telegram 数字 ID。第一次部署前也可以先用临时测试 Token 启动，拿到 ID 后再切换正式配置。

## 场景 A：单机器人一体部署

只有一个机器人时，可以直接使用仓库根目录 `docker-compose.yml` 或 `docker-compose.ghcr.yml`。这个 Compose 项目同时包含：

```text
postgres
chain-postgres
ledger-chain-watcher
ledger-bot
```

Compose 版不建议把 PostgreSQL 和 watcher 塞进同一个容器。分成 `chain-postgres` 和 `ledger-chain-watcher` 两个容器，更利于数据卷持久化、升级、备份、健康检查和故障隔离；在宝塔里它们仍然属于同一个 Compose 项目，可以一起启动和停止。

启动前必须改这些值：

```yaml
TELEGRAM_BOT_TOKEN: "123456:replace-me"
TELEGRAM_BOT_USERNAME: "your_bot_username"
BOT_HOST_USER_ID: "123456789"
POSTGRES_PASSWORD: "change_this_strong_password"
DATABASE_URL: "postgres://ledger:change_this_strong_password@postgres:5432/ledger_bot?sslmode=disable"
CHAIN_WATCHER_SECRET: "change_this_chain_watcher_secret"
CHAIN_WATCHER_URL: "http://ledger-chain-watcher:8090"
CHAIN_WATCHER_BOT_ID: "ledger-main"
CHAIN_WATCHER_EMERGENCY_FALLBACK: "false"
TRONGRID_API_KEY: ""
```

`CHAIN_WATCHER_BOT_ID` 和 `CHAIN_WATCHER_SECRET` 必须同步写入 watcher 的 `CHAIN_WATCHER_BOTS`：

```yaml
CHAIN_WATCHER_BOTS: "ledger-main:change_this_chain_watcher_secret"
```

如果第一版 watcher 统一请求 Tronscan/TronGrid，再在 `ledger-chain-watcher` 服务里填写：

```yaml
CHAIN_WATCHER_TRONSCAN_API_BASE: "https://apilist.tronscanapi.com/api"
CHAIN_WATCHER_TRON_API_KEY: "your_real_api_key"
CHAIN_WATCHER_SOURCE_POLL_SECONDS: "1"
CHAIN_WATCHER_GLOBAL_SCAN_PAGES: "1"
CHAIN_WATCHER_ADDRESS_SCAN_PAGES: "3"
CHAIN_WATCHER_ADDRESS_SCAN_CONCURRENCY: "8"
CHAIN_WATCHER_LOOKBACK_SECONDS: "600"
```

`CHAIN_WATCHER_TRON_API_KEY` 只放在 watcher 里，机器人实例不再各自配置官网 API Key。

`CHAIN_WATCHER_EMERGENCY_FALLBACK` 是机器人侧应急兜底开关，默认保持 `false`。临时设为 `true` 后，机器人会在使用 watcher 的同时本机按地址扫链，会增加 Tronscan/TronGrid API 调用量，只建议 watcher 故障排查或短时兜底时使用。本机兜底使用机器人侧 `TRONSCAN_API_BASE` / `TRONGRID_API_KEY`；只修改 watcher 的 `CHAIN_WATCHER_TRON_API_KEY` 不会给机器人本机扫描带 Key。

常用选填：

```yaml
DEFAULT_OPERATOR_USER_IDS: ""
PUBLIC_BILL_BASE_URL: "https://bot.example.com"
ADMIN_WEB_TOKEN: "change_this_admin_password"
ADDRESS_WATCH_FREE_LIMIT: "2"
```

高级并发默认值按 4 核 8G 调整：

```yaml
BOT_WORKER_THREADS: "16"
BOT_CONTROL_THREADS: "6"
BOT_CHAIN_THREADS: "12"
BOT_RATE_THREADS: "2"
BOT_BROADCAST_THREADS: "4"
BOT_QUERY_THREADS: "4"
BOT_NOTIFICATION_THREADS: "6"
BOT_QUEUE_SIZE: "4096"
```

启动后看日志：

```bash
docker compose logs -f ledger-bot
docker compose logs -f ledger-chain-watcher
```

## 场景 A1：宝塔宿主机 PostgreSQL

如果你已经在宝塔里安装 PostgreSQL，并且希望数据库运行在宿主机上，Docker Compose 里只跑机器人和 watcher，请使用仓库根目录：

```text
docker-compose.baota-host-pg.yml
```

这个 Compose 只包含：

```text
ledger-chain-watcher
ledger-bot
```

它不包含 PostgreSQL 容器，也不创建数据库 volume。两个容器都通过：

```yaml
extra_hosts:
  - "host.docker.internal:host-gateway"
```

访问宿主机 PostgreSQL。

宝塔 PostgreSQL 里先创建两个数据库，数据库名不要带下划线、横杠、中文、空格或特殊符号：

```text
ledgerchainwatcher
ledgerbotmain
```

`ledgerchainwatcher` 只给共享 watcher 使用。每个机器人实例再单独使用一个自己的数据库；同一个机器人里的广播/转发配置和记账数据共用这个机器人数据库，不要按功能拆库。`ledgerbotmain` 适合另一个独立主机器人或未来主机器人迁移目标；如果实例是 `tianzezsbot`，建议使用：

```text
ledgerbottianze
```

推荐以后第二个独立机器人使用：

```text
ledgerbotforward
```

Compose 里的数据库连接示例：

```yaml
CHAIN_WATCHER_DATABASE_URL: "postgres://ledger:改成你的PostgreSQL强密码@host.docker.internal:5432/ledgerchainwatcher?sslmode=disable"
DATABASE_URL: "postgres://ledger:改成你的PostgreSQL强密码@host.docker.internal:5432/ledgerbotmain?sslmode=disable"
```

需要确认宝塔 PostgreSQL 允许来自 Docker 容器的连接。如果连接失败，优先检查：

```text
PostgreSQL 监听地址
pg_hba.conf 允许 Docker 网段
宝塔安全/系统防火墙没有拦截 5432
数据库用户名和密码是否正确
```

watcher 和 bot 的凭据仍然必须成对：

```yaml
CHAIN_WATCHER_BOTS: "ledger-main:change_this_chain_watcher_secret"
CHAIN_WATCHER_BOT_ID: "ledger-main"
CHAIN_WATCHER_SECRET: "change_this_chain_watcher_secret"
```

安全边界：

```text
ledger-chain-watcher 只 expose 8090，不映射公网端口。
ledger-bot 映射 8080:8080，用于宝塔/Nginx 反向代理后台和网页账单。
ADMIN_WEB_TOKEN 不要和 CHAIN_WATCHER_SECRET 共用。
多个机器人实例仍然要使用不同数据库、不同端口、不同 PUBLIC_BILL_BASE_URL、不同 ADMIN_WEB_TOKEN。
```

广播和记账是一体能力，不需要为“广播群”单独关闭机器人记账模块。每个群默认未开始记账，只保存群名并支持广播/转发回复通知；需要记账的群，由宿主、默认操作人或本群操作员在群内发送 `开始`。普通成员发送 `开始` 会收到 `没有操作权限。请管理员添加操作员。`

未开始记账的群里，普通成员发送 `+100`、`下发100U` 等金额文本时机器人会静默忽略，避免广播群被记账提示打扰；有权限的人发送金额文本会提示先发送 `开始`。

日切后当前群记账激活状态自动失效，新业务日需要由有权限的人重新发送 `开始`；汇率、费率、操作员和广播配置继续保留。

## 场景 A2：宿主机 systemd 运行 watcher

如果 PostgreSQL 已经通过宝塔安装在宿主机上，也可以把 `ledger-chain-watcher` 直接跑在服务器宿主机上，机器人继续用宝塔 Docker Compose 部署。这种模式适合多个机器人共用同一个宿主机 watcher，且不想把 watcher 放进 Docker。

仓库提供两个模板：

```text
deploy/ledger-chain-watcher.env.example
deploy/ledger-chain-watcher.service
```

服务器上先创建 watcher 数据库，数据库名不要带下划线、横杠、中文、空格或特殊符号：

```text
ledgerchainwatcher
```

复制 env 文件：

```bash
mkdir -p /etc/ledger-chain-watcher
cp deploy/ledger-chain-watcher.env.example /etc/ledger-chain-watcher/env
chmod 600 /etc/ledger-chain-watcher/env
```

编辑 `/etc/ledger-chain-watcher/env`：

```env
CHAIN_WATCHER_DATABASE_URL=postgres://ledger:你的PostgreSQL密码@127.0.0.1:5432/ledgerchainwatcher?sslmode=disable
CHAIN_WATCHER_ADDR=0.0.0.0:8090
CHAIN_WATCHER_BOTS=tianzezsbot:换成随机内部密钥
CHAIN_WATCHER_TRONSCAN_API_BASE=https://apilist.tronscanapi.com/api
CHAIN_WATCHER_TRON_API_KEY=你的Tronscan API Key
CHAIN_WATCHER_SOURCE_POLL_SECONDS=1
CHAIN_WATCHER_GLOBAL_SCAN_PAGES=1
CHAIN_WATCHER_ADDRESS_SCAN_PAGES=3
CHAIN_WATCHER_ADDRESS_SCAN_CONCURRENCY=8
CHAIN_WATCHER_LOOKBACK_SECONDS=600
CHAIN_WATCHER_CLAIM_LEASE_SECONDS=30
CHAIN_WATCHER_DELIVERY_RETRY_SECONDS=2
BOT_TIMEZONE=Asia/Shanghai
BOT_REQUEST_TIMEOUT=70
```

下载 v2.3 发布包并安装二进制到固定路径：

```bash
cd /tmp
wget -O ledger-chain-watcher-v2.3-linux-amd64.tar.gz \
  https://github.com/dayou0168/telegram-ledger-bot/releases/download/v2.3/ledger-chain-watcher-v2.3-linux-amd64.tar.gz
tar -xzf ledger-chain-watcher-v2.3-linux-amd64.tar.gz
install -m 0755 ledger-chain-watcher-v2.3-linux-amd64/ledger-chain-watcher /usr/local/bin/ledger-chain-watcher
/usr/local/bin/ledger-chain-watcher --help
```

发布包里同时包含 `ledger-chain-watcher.env.example` 和 `ledger-chain-watcher.service`，也可以直接从解压目录复制。关键是 `/usr/local/bin/ledger-chain-watcher` 必须存在并可执行。

安装 systemd service：

```bash
cp deploy/ledger-chain-watcher.service /etc/systemd/system/ledger-chain-watcher.service
systemctl daemon-reload
systemctl enable --now ledger-chain-watcher
journalctl -u ledger-chain-watcher -f
```

模板默认不写死 `User=`。如果要用非 root 用户运行，请先创建用户，再确认该用户能读取 `/etc/ledger-chain-watcher/env` 和执行 `/usr/local/bin/ledger-chain-watcher`，然后在 service 里启用 `User=` / `Group=`。

机器人 Compose 接入宿主机 watcher 时，机器人自己的数据库仍然写自己的 bot 库，不要改成 `ledgerchainwatcher`：

```yaml
extra_hosts:
  - "host.docker.internal:host-gateway"
environment:
  DATABASE_URL: "postgres://ledger:密码@host.docker.internal:5432/ledgerbottianze?sslmode=disable"
  CHAIN_WATCHER_URL: "http://host.docker.internal:8090"
  CHAIN_WATCHER_BOT_ID: "tianzezsbot"
  CHAIN_WATCHER_SECRET: "换成随机内部密钥"
  CHAIN_WATCHER_EMERGENCY_FALLBACK: "false"
  TRONGRID_API_KEY: ""
```

Docker 20.10+ 推荐使用：

```yaml
extra_hosts:
  - "host.docker.internal:host-gateway"
```

如果服务器 Docker 不支持 `host-gateway`，可以查 Docker 网桥 IP：

```bash
ip addr show docker0
```

然后把机器人配置成类似：

```env
CHAIN_WATCHER_URL=http://172.17.0.1:8090
```

安全边界：

```text
8090 不开放公网。
宝塔安全组、云安全组和系统防火墙只允许本机/Docker 网段访问 8090。
CHAIN_WATCHER_SECRET、ADMIN_WEB_TOKEN、PostgreSQL 密码三者不要混用。
CHAIN_WATCHER_BOTS 泄露后，要同时更换 watcher env 和所有机器人 Compose 配置。
```

升级宿主机 watcher：

```bash
systemctl stop ledger-chain-watcher
install -m 0755 ledger-chain-watcher /usr/local/bin/ledger-chain-watcher
systemctl restart ledger-chain-watcher
journalctl -u ledger-chain-watcher -f
```

如果只改 `/etc/ledger-chain-watcher/env`，不需要替换二进制，直接重启：

```bash
systemctl restart ledger-chain-watcher
journalctl -u ledger-chain-watcher -f
```

## 场景 B：多机器人共享 watcher

多机器人时，推荐拆成多个宝塔 Compose 项目：

```text
ledger-chain-watcher        共享链上监听服务和 watcher PostgreSQL
ledger-main                 当前记账机器人实例
ledger-ops                  第二个独立 Go v2.3 机器人实例
```

先创建共享 Docker 网络：

```bash
docker network create ledger-chain-net
```

然后单独部署 `docker-compose.chain-watcher.yml`。它是一个独立 watcher Compose 项目，只运行两个服务/两个容器：

```text
chain-postgres
ledger-chain-watcher
```

每个机器人 Compose 保留自己的默认网络连接自己的 PostgreSQL，再额外加入 `ledger-chain-net`：

```yaml
services:
  ledger-bot:
    networks:
      - default
      - ledger-chain-net
    environment:
      CHAIN_WATCHER_URL: "http://ledger-chain-watcher:8090"
      CHAIN_WATCHER_BOT_ID: "ledger-main"
      CHAIN_WATCHER_SECRET: "change_this_chain_watcher_secret"
      CHAIN_WATCHER_EMERGENCY_FALLBACK: "false"

networks:
  ledger-chain-net:
    external: true
```

多机器人可以共享：

```text
ledger-chain-net
ledger-chain-watcher
CHAIN_WATCHER_BOTS 中登记的 bot_id/secret
```

多机器人不能共享：

```text
Bot Token
PostgreSQL 容器
PostgreSQL volume
DATABASE_URL
CHAIN_WATCHER_BOT_ID
container_name
宿主机端口
PUBLIC_BILL_BASE_URL
ADMIN_WEB_TOKEN
```

示例命名：

```text
当前记账机器人：
  Compose 项目: ledger-main
  bot 容器: ledger-main-bot
  PG 容器: ledger-main-postgres
  volume: ledger_main_pg_data
  端口: 8080:8080
  域名: https://bill-a.example.com

第二个机器人实例：
  Compose 项目: ledger-ops
  bot 容器: ledger-ops-bot
  PG 容器: ledger-ops-postgres
  volume: ledger_ops_pg_data
  端口: 8081:8080
  域名: https://bill-b.example.com
```

## chain-watcher 端口和安全

`ledger-chain-watcher` 默认只暴露 Docker 内网端口：

```text
容器端口: 8090
机器人内网访问: http://ledger-chain-watcher:8090
健康检查: /healthz
公网端口: 不开放
```

需要宿主机调试时，只绑定本机：

```yaml
ports:
  - "127.0.0.1:18090:8090"
```

`CHAIN_WATCHER_BOT_ID` 和 `CHAIN_WATCHER_SECRET` 是内部服务凭据：

```text
每个机器人实例使用唯一 CHAIN_WATCHER_BOT_ID
每个 CHAIN_WATCHER_BOT_ID 在 watcher 的 CHAIN_WATCHER_BOTS 中配置一条 bot_id:secret
不要放公网
不要和 ADMIN_WEB_TOKEN 共用
多个机器人可共用 watcher 服务，但不建议共用同一个 bot_id
泄露后应同时更换 watcher 和所有机器人实例配置
```

## 未来接 TRON Lite FullNode + Kafka

v2.3 第一版 watcher 统一请求 Tronscan/TronGrid。未来切换自建节点时，机器人配置不变，只调整 watcher：

```yaml
CHAIN_WATCHER_SOURCE: "kafka"
KAFKA_BROKERS: "tron-kafka:9092"
KAFKA_TOPIC: "tron-events"
CHAIN_WATCHER_LOOKBACK_SECONDS: "600"
```

推荐 Kafka 初始 retention：

```properties
log.retention.ms=600000
log.retention.check.interval.ms=60000
log.cleanup.policy=delete
num.partitions=3
default.replication.factor=1
```

机器人侧继续保存监听地址、tx_hash 去重和 Telegram outbox；watcher 只负责链上数据入口和短期缓存。

## 权限说明

权限以 `go-ledger/internal/permissions` 当前规则为准：

- `BOT_HOST_USER_ID` 是唯一宿主，必填。
- `DEFAULT_OPERATOR_USER_IDS` 是程序默认操作人，选填，多个 ID 用英文逗号、空格或分号分隔。
- 宿主和默认操作人都是全局权限用户。
- 全局权限用户可以邀请机器人进群、在任意群记账、使用群发/分组广播、使用地址监听、管理任意群。
- 普通用户可以在私聊使用地址监听，默认最多 2 个 active 地址；通过 `ADDRESS_WATCH_FREE_LIMIT` 调整。宿主、默认操作人和操作人不受此数量限制。
- 后台入口优先通过私聊机器人生成 5 分钟有效的 Telegram 身份登录链接；`ADMIN_WEB_TOKEN` 仍作为宿主紧急入口和会话签名密钥。
- 普通用户发送 `开始` 会被拒绝，不会激活记账，也不会变成宿主或默认操作人。
- 单群操作员是群级权限，由宿主或默认操作人在群内管理。
- 邀请机器人进群的人如果不是宿主或默认操作人，机器人会自动退出。

## 域名、Cloudflare 和反向代理

如果只用 Telegram 记账，机器人不需要公网入站端口。只有网页账单和后台需要域名。

推荐流程：

1. 在 Cloudflare 添加 `A` 记录，例如 `bot.example.com -> 服务器 IP`。
2. SSL/TLS 模式建议使用 `Full` 或 `Full (strict)`。
3. 宝塔创建站点 `bot.example.com`，申请 SSL。
4. 宝塔反向代理到对应实例端口，例如 `http://127.0.0.1:8080` 或 `http://127.0.0.1:8081`。
5. 对应实例填写独立的 `PUBLIC_BILL_BASE_URL` 和 `ADMIN_WEB_TOKEN`。

每个机器人实例一个独立域名或子域名：

```text
https://bill-a.example.com -> http://127.0.0.1:8080
https://bill-b.example.com -> http://127.0.0.1:8081
```

## 更新镜像

宝塔面板里可以直接重启对应 Compose 项目。命令行更新：

```bash
docker compose pull
docker compose up -d
docker compose logs -f ledger-bot
```

如果只改环境变量，重启对应服务即可：

```bash
docker compose restart ledger-bot
docker compose restart ledger-chain-watcher
```

## PostgreSQL 备份

每个机器人实例都要单独备份自己的 PostgreSQL：

```bash
mkdir -p backups
docker exec ledger-main-postgres pg_dump -U ledger -d ledger_bot > backups/ledger_main_$(date +%F_%H%M%S).sql
docker exec ledger-ops-postgres pg_dump -U ledger -d ledger_bot > backups/ledger_ops_$(date +%F_%H%M%S).sql
```

压缩备份：

```bash
gzip backups/*.sql
```

建议把 `backups/` 同步到服务器外部位置，例如另一台机器、对象存储或网盘。

## 常见检查

查看容器：

```bash
docker ps
```

查看数据库健康：

```bash
docker exec ledger-main-postgres pg_isready -U ledger -d ledger_bot
```

查看机器人和 watcher 日志：

```bash
docker compose logs --tail=200 ledger-bot
docker compose logs --tail=200 ledger-chain-watcher
```

测试反代：

```bash
curl -I https://bot.example.com/admin
```

测试 watcher 内网健康：

```bash
docker exec ledger-bot-go wget -qO- http://ledger-chain-watcher:8090/healthz
```

如果网页打不开，优先检查 `PUBLIC_BILL_BASE_URL` 是否带 `https://`，宝塔反代目标端口是否正确，以及 Cloudflare SSL 模式是否正确。
