# Telegram Ledger Bot Go Runtime v2.4.12

这是机器人 Go 版 v2.4.12 发布候选主线，目标是把同步、异步、队列、并发、缓存、数据库和共享链上监听从架构层面重新设计。当前已具备：

候选版发布说明见 [../docs/releases/v2.4.12.md](../docs/releases/v2.4.12.md)。

v2.4.12 增加可靠的群成员发现与操作员目标解析，并把广播回复通知改成独立、分层授权的后台设置。新增 `2.4.16-chat-member-discovery` 和 `2.4.17-broadcast-reply-preferences` 数据库迁移；watcher 协议不变。

- Telegram long polling。
- 按 `chat_id` / `user_id` 串行分发。
- 独立弹性功能池：记账、私聊控制、链上监听、汇率、广播、TRX 查询、通知。
- PostgreSQL 主存储，启动时自动迁移 schema 和索引。
- 金额保存统一四舍五入到小数点后 2 位；汇率保留必要精度。
- 基础群保存、用户 touch、操作员权限表、账本记录表。
- `开始` / `停止`，日切后当前群记账激活状态失效，新业务日需要重新发送 `开始`；费率、汇率、操作员和广播配置继续沿用。
- `上课` / `下课` 调用 Telegram 群权限开启或关闭全员发言，机器人需要群管理权限。
- `设置费率3`、`设置汇率7.1`、`设置日切04`；`设置日切-1` 与 `关闭日切` 等价，都会写入明确的关闭值 `cutoff_hour=-1`。
- `+100`、`+100/7`、`+100*5/7.1`、`+100*3%`、`+100U`、`下发100U`、`下发5000/7.8`、`下发5000*5/7.1`；`下发100` 已禁用，避免误把人民币当 U。
- 群内运算：`1000/6.8`、`1000/7.5-1000/6.8`、`(2+3)*4` 直接回复计算结果，不写入账单。
- `+0`、`显示账单`、`账单` 生成当前业务日账单摘要；未开始的新账期里普通成员静默，有权限的人会收到先发送 `开始` 的提示。
- `通知所有人` 会在群内 @ 已发言成员，并按 Telegram 消息长度自动分段。
- `清除当前账期` 带二次确认并显示账期起止、记录数和不可恢复提示；只清当前 active period，不重置群配置、汇率或费率。历史 `cutoff_hour=0` 与真实午夜日切无法区分，启动和迁移都不会自动改成关闭日切；只能由确认过的旧群重新发送关闭命令，或由运维按确认白名单定向修正。
- 回复原始加账消息或机器人账单回执发送 `撤销` / `撤销入款` / `撤销下发`。
- 回复用户、选择 Telegram 可点击昵称（`text_mention`）或输入机器人已记录的 `@用户名` 添加/删除本群操作员；普通手打昵称和数字 UID 不作为目标。
- 加账写库和 Telegram 账单回执发送拆开：同群写库串行，回执异步有序发送，并保存机器人回执消息 ID 供撤销定位。
- 完整账单按钮使用短链接：`/b/{chat_id}/{yyyymmdd}`，不再拼接冗长的 `begintime/endtime` 参数。
- 内置后台管理和短账单网页。后台只接受 Telegram 一次性身份 ticket；`ADMIN_SESSION_SECRET` 独立签名 cookie，`ADMIN_WEB_TOKEN` 仅作为可选第二因素，不能单独登录或提升身份。账单页支持历史日期、上一天/下一天、高级筛选和下载 `.xlsx`。
- 私聊按钮式广播入口：群发、分组广播、单群发送；选定目标后可连续发送文字、图片、图片+文字或文件，不再逐条确认。
- 广播目标选择后可切换“通知所有人”，开启后广播投递到目标群会自动追加 @ 已发言成员。
- 私聊菜单已接入详细说明、UID 查询和后台管理入口；后台入口会生成 5 分钟有效的 `PUBLIC_BILL_BASE_URL/admin/login?ticket=...` 登录链接。
- 广播回复通知：发送者强制接收；后台独立“回复通知”标签允许宿主、默认操作人和 active 全局操作人在权限范围内逐人控制额外接收人，一级只能管理自己的 active 下级。
- 广播替换：后台可开关，仅对单群发送生效；群成员回复投递消息时，可把原投递消息替换成固定图片/文字。
- `Z0` 查询 OKX OTC 商家所有实时汇率 TOP10，`Z1 -0.1` / `设置汇率 Z1 -0.1` 可按档位偏移设置群汇率。
- 实时汇率全局定时刷新，默认 60 秒刷新一次，所有群共用缓存。
- 群内发送 TRC20 地址会自动记录验证次数；首次出现回复防篡改核对图，重复出现显示上次发送人和本次发送人。
- `查询T...` / `查询TRX地址 T...` 查询 TRON 地址余额、创建/活跃时间和最近 USDT 流水，走独立查询池。
- 地址监听权限：普通用户最多 2 个监听地址；只有宿主、`DEFAULT_OPERATOR_USER_IDS` 和 active `global_operators` 不受数量限制，单群 `operators` 不获得该私聊全局资格。私聊按钮面板支持添加/删除地址、收入/支出/TRX 通知开关、最小提醒金额。
- v2.4.12 链上监听继续通过共享 `ledger-chain-watcher` 获取链上数据；现有 v2.4.11 watcher 与本次机器人功能兼容，无需重启。机器人侧继续保存监听地址、event identity 去重和 Telegram outbox。

## 构建

推荐直接使用 GitHub Actions 构建发布的镜像：

```bash
docker pull ghcr.io/dayou0168/telegram-ledger-bot-go:2.4.12
docker pull ghcr.io/dayou0168/telegram-ledger-chain-watcher:2.4.12
```

本目录也保留独立 Dockerfile，方便本地构建：

```bash
docker build -t telegram-ledger-bot-go:dev .
docker build --build-arg APP=chain-watcher -t telegram-ledger-chain-watcher:dev .
```

正式发布后，推荐直接用仓库根目录的 `docker-compose.yml` 或 `docker-compose.ghcr.yml` 启动，同一个 Compose 项目里包含 PostgreSQL 独立容器和 `ledger-chain-watcher` 独立容器，默认拉取 `ghcr.io/dayou0168/telegram-ledger-bot-go:2.4.12` 与 `ghcr.io/dayou0168/telegram-ledger-chain-watcher:2.4.12`：

```bash
docker compose -f ../docker-compose.yml up -d
```

Compose 模板默认限制 Docker `json-file` 日志为 `max-size=20m`、`max-file=5`，单容器约 100MB，用大小上限近似保留最近日志并防止长期运行撑爆磁盘。host systemd 版 watcher 建议用 journald `MaxRetentionSec=3day` 和 `SystemMaxUse=500M` 控制最近约 72 小时日志。

如果 `ledger-chain-watcher` 要跑在宿主机 systemd 里，仓库根目录提供：

```text
deploy/ledger-chain-watcher.env.example
deploy/ledger-chain-watcher.service
```

GitHub Release `v2.4.12` 同时发布配套 watcher 宿主机包，但本次修复只需要把机器人升级到 v2.4.12，生产 watcher 保持当前版本和进程不动。

机器人仍然用自己的 `DATABASE_URL` 连接自己的 PostgreSQL 数据库，并通过 `CHAIN_WATCHER_URL=http://host.docker.internal:8090` 或 Docker 网桥 IP 访问宿主机 watcher。

核心环境变量：

```env
TELEGRAM_BOT_TOKEN=123456:replace-me
TELEGRAM_BOT_USERNAME=your_bot_username
BOT_HOST_USER_ID=123456789
DATABASE_URL=postgres://ledger:change_this_strong_password@postgres:5432/ledger_bot?sslmode=disable
PUBLIC_BILL_BASE_URL=https://bot.example.com
ADMIN_SESSION_SECRET=change_this_independent_session_secret
ADMIN_WEB_TOKEN=
ADDRESS_WATCH_FREE_LIMIT=2
CHAIN_WATCHER_URL=http://ledger-chain-watcher:8090
CHAIN_WATCHER_BOT_ID=ledger-main
CHAIN_WATCHER_SECRET=change_this_chain_watcher_secret
TRONGRID_API_KEY=
BOT_WATCHER_HEALTH_INTERVAL_SECONDS=1
BOT_WATCHER_FAIL_THRESHOLD=3
BOT_WATCHER_CLAIM_TIMEOUT_MS=2000
BOT_FALLBACK_POLL_SECONDS=1
BOT_FALLBACK_SHARED_DATABASE_URL=postgres://chainwatcher:***@chain-postgres:5432/ledger_chain_watcher?sslmode=disable
```

正常模式下 bot 只消费 watcher 事件。watcher 连续异常 3 秒后，所有 bot 通过共享 PostgreSQL lease 只选一个无 Key fallback leader；它持续工作到 watcher 补齐 cursor 并恢复，不设 600 秒强停。未配置共享 DSN 时明确降级，不启动逐地址 fallback。

每个 bot 的 `BOT_FALLBACK_INSTANCE_ID` 必须唯一且稳定，并与 `BOT_FALLBACK_SHARED_DATABASE_URL` 一起配置；缺少任一项时 fallback 只报告 DEGRADED。

watcher 的动态 Tronscan Key 注册表不设固定数量上限。主扫每秒按健康 Key 的可持续小数 base token 动态生成页数，完整额度下约为 1 页/Key/秒，但不会按 Key 数机械发请求。P1 使用独立持久化 lane，P2..PN 进入有界普通 worker 池。每 Key 默认日规划额度 100,000 次，重置点 UTC 00:00；基础约 86,400 次/天，剩余约 13,600 次进入共享 surplus。数据库计数不硬停实时头部，服务端 429 才触发对应 Key 冷却。Key 用 AES-256-GCM 加密保存，`/status` 只显示短指纹和脱敏状态。

## 推荐默认并发

4 核 8G：

```env
BOT_WORKER_THREADS=16
BOT_CONTROL_THREADS=6
BOT_CHAIN_THREADS=12
BOT_RATE_THREADS=2
BOT_BROADCAST_THREADS=4
BOT_QUERY_THREADS=4
BOT_NOTIFICATION_THREADS=6
BOT_QUEUE_SIZE=4096
BOT_BROADCAST_DELIVERY_RETENTION_HOURS=168
```

这些值是最大并发上限，不是长期占用。Go 版功能池会按队列压力自动扩容，空闲 30 秒自动缩回；少量群活跃时，空闲资源会用于异步回执、缓存预热、链上索引和账单摘要预计算，而不是让账本队列等待慢 HTTP。

## 数据库原则

- 生产主库使用 PostgreSQL。
- Telegram 更新去重、账本、权限、广播任务、链上通知都落 PostgreSQL。
- 表设计从第一版就使用 `BIGINT` Telegram ID、`TIMESTAMPTZ` 时间、`BOOLEAN` 开关和高频组合索引。
- 金额类字段在写库前统一格式化为两位小数，减少长尾小数导致的账单阅读问题。
- PostgreSQL 是 v2.4.12 的唯一主库目标，避免后续再次迁移。

## 启动原则

Go v2.4.12 直接按 PostgreSQL 空库启动。等账单、广播、监听三块经过真实群测试后，再切换生产 Bot Token。多机器人部署时，每个机器人实例独立 PostgreSQL；只有 `ledger-chain-watcher` 共享。
