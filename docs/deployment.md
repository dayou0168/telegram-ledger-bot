# Telegram 记账机器人 Go v2.1 部署运维

当前发布主线是 Go v2.1。生产部署使用 GHCR 预构建镜像和 PostgreSQL，不再使用 Python 版、SQLite 或服务器源码构建作为默认路径。

## 部署基线

- 镜像：`ghcr.io/dayou0168/telegram-ledger-bot-go:2.1`
- 数据库：PostgreSQL 16
- 推荐入口：宝塔 Docker Compose
- 推荐配置：4 核 8G
- 网页账单和后台：容器 `8080` 端口，宝塔/Nginx/Cloudflare 走 HTTPS 反代

## 准备 Telegram Bot

在 BotFather 创建机器人，并关闭隐私模式：

```text
/newbot
/setprivacy -> 选择机器人 -> Disable
```

私聊机器人发送 `我的ID` 可以获取 Telegram 数字 ID。第一次部署前也可以先用临时测试 Token 启动，拿到 ID 后再切换正式配置。

## 宝塔 Docker Compose

宝塔里进入 Docker -> Compose，新建项目后粘贴仓库根目录的 `docker-compose.ghcr.yml` 或 `docker-compose.yml`。两个文件都默认拉取 GHCR 镜像，并自带 PostgreSQL。

启动前必须改这些值：

```yaml
TELEGRAM_BOT_TOKEN: "123456:replace-me"
TELEGRAM_BOT_USERNAME: "your_bot_username"
BOT_HOST_USER_ID: "123456789"
DATABASE_URL: "postgres://ledger:change_this_strong_password@postgres:5432/ledger_bot?sslmode=disable"
POSTGRES_PASSWORD: "change_this_strong_password"
```

`POSTGRES_PASSWORD` 和 `DATABASE_URL` 里的密码必须一致。

常用选填：

```yaml
DEFAULT_OPERATOR_USER_IDS: ""
PUBLIC_BILL_BASE_URL: "https://bot.example.com"
ADMIN_WEB_TOKEN: "change_this_admin_password"
TRONGRID_API_KEY: ""
```

高级配置已经在 Compose 文件里分区注释。默认值按 4 核 8G 调整：

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
```

看到机器人开始 long polling，且没有 PostgreSQL 连接错误，即为基础启动成功。

## 权限说明

权限以 `go-ledger/internal/permissions` 当前规则为准：

- `BOT_HOST_USER_ID` 是唯一宿主，必填。
- `DEFAULT_OPERATOR_USER_IDS` 是程序默认操作人，选填，多个 ID 用英文逗号、空格或分号分隔。
- 宿主和默认操作人都是全局权限用户。
- 全局权限用户可以邀请机器人进群、在任意群记账、使用群发/分组广播、使用地址监听、管理任意群。
- 普通用户发送 `开始` 只会激活记账，不会变成宿主或默认操作人。
- 单群操作员是群级权限，由宿主或默认操作人在群内管理。
- 邀请机器人进群的人如果不是宿主或默认操作人，机器人会自动退出。

## 域名、Cloudflare 和反向代理

如果只用 Telegram 记账，机器人不需要公网入站端口。只有网页账单和后台需要域名。

推荐流程：

1. 在 Cloudflare 添加 `A` 记录，例如 `bot.example.com -> 服务器 IP`。
2. SSL/TLS 模式建议使用 `Full` 或 `Full (strict)`。
3. 宝塔创建站点 `bot.example.com`，申请 SSL。
4. 宝塔反向代理到：

```text
http://127.0.0.1:8080
```

5. Compose 里填写：

```yaml
PUBLIC_BILL_BASE_URL: "https://bot.example.com"
ADMIN_WEB_ENABLED: "1"
ADMIN_WEB_HOST: "0.0.0.0"
ADMIN_WEB_PORT: "8080"
ADMIN_WEB_TOKEN: "change_this_admin_password"
```

账单短链接格式：

```text
https://bot.example.com/b/-1001234567890/20260705
```

后台入口：

```text
https://bot.example.com/admin
```

宿主或默认操作人可以在私聊菜单点击 `⚙后台管理` 打开后台。公网部署时必须设置强 `ADMIN_WEB_TOKEN`。

## 更新镜像

宝塔面板里可以直接重启 Compose 项目。命令行更新：

```bash
docker compose pull
docker compose up -d
docker compose logs -f ledger-bot
```

如果只改环境变量，重启即可：

```bash
docker compose restart ledger-bot
```

## PostgreSQL 备份

Compose 使用 Docker 卷 `ledger_pg_data` 保存 PostgreSQL 数据。建议用 `pg_dump` 做可迁移备份：

```bash
mkdir -p backups
docker exec ledger-postgres pg_dump -U ledger -d ledger_bot > backups/ledger_bot_$(date +%F_%H%M%S).sql
```

压缩备份：

```bash
gzip backups/ledger_bot_*.sql
```

建议把 `backups/` 同步到服务器外部位置，例如另一台机器、对象存储或网盘。

## 恢复

先启动 PostgreSQL，再把 SQL 导入空库：

```bash
docker compose up -d postgres
cat backups/ledger_bot.sql | docker exec -i ledger-postgres psql -U ledger -d ledger_bot
docker compose up -d ledger-bot
```

恢复后验证：

```bash
docker compose logs -f ledger-bot
```

再在 Telegram 测试：

```text
/start
我的ID
开始
+0
```

## 常见检查

查看容器：

```bash
docker ps
```

查看数据库健康：

```bash
docker exec ledger-postgres pg_isready -U ledger -d ledger_bot
```

查看机器人日志：

```bash
docker compose logs --tail=200 ledger-bot
```

测试反代：

```bash
curl -I https://bot.example.com/admin
```

如果网页打不开，优先检查 `PUBLIC_BILL_BASE_URL` 是否带 `https://`，宝塔反代目标是否是 `http://127.0.0.1:8080`，以及 Cloudflare SSL 模式是否正确。
