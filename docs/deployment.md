# Telegram 记账机器人 Go v2.2 部署运维

当前发布目标是 Go v2.2。生产部署使用 GHCR 预构建镜像、PostgreSQL 和共享链上监听服务 `ledger-chain-watcher`。服务器上不需要源码构建作为默认路径。

## 部署基线

- 机器人镜像：`ghcr.io/dayou0168/telegram-ledger-bot-go:2.2`
- watcher 镜像：`ghcr.io/dayou0168/telegram-ledger-chain-watcher:2.2`
- 数据库：每个机器人实例独立 PostgreSQL 16
- 链上监听：多个机器人共享 `ledger-chain-watcher`，watcher 使用独立 PostgreSQL 保存订阅、匹配事件和投递游标
- 推荐入口：宝塔 Docker Compose
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

只有一个机器人时，可以直接使用仓库根目录 `docker-compose.yml` 或 `docker-compose.ghcr.yml`。这个 Compose 同时包含：

```text
postgres
chain-postgres
ledger-chain-watcher
ledger-bot
```

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
CHAIN_WATCHER_LOOKBACK_SECONDS: "600"
```

`CHAIN_WATCHER_TRON_API_KEY` 只放在 watcher 里，机器人实例不再各自配置官网 API Key。

常用选填：

```yaml
DEFAULT_OPERATOR_USER_IDS: ""
PUBLIC_BILL_BASE_URL: "https://bot.example.com"
ADMIN_WEB_TOKEN: "change_this_admin_password"
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

## 场景 B：多机器人共享 watcher

多机器人时，推荐拆成多个宝塔 Compose 项目：

```text
ledger-chain-watcher        共享链上监听服务和 watcher PostgreSQL
ledger-main                 当前记账机器人实例
ledger-ops                  第二个独立 Go v2.2 机器人实例
```

先创建共享 Docker 网络：

```bash
docker network create ledger-chain-net
```

然后单独部署 `docker-compose.chain-watcher.yml`。它只运行：

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

v2.2 第一版 watcher 统一请求 Tronscan/TronGrid。未来切换自建节点时，机器人配置不变，只调整 watcher：

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
