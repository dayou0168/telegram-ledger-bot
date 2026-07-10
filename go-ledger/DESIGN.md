# Go Ledger Bot v2.3 Architecture

这个目录是 Telegram 记账机器人的 Go v2.3 主线。当前主线按 PostgreSQL-first 设计，生产部署、测试和发布都以 Go 运行时为准。

## 目标

- 上百个群同时使用时，群内加账、撤销、设置仍能秒响应。
- 广播、链上监听、TRX 查询、汇率刷新互相隔离，慢任务不拖住实时消息。
- 同一群账本严格串行，避免并发写入导致账本乱序。
- 不把所有逻辑堆在一个 handler 中，按同步路径、异步任务、后台调度拆分。
- PostgreSQL 作为生产主库；多个机器人实例各自独立 PostgreSQL，共享链上监听服务 `ledger-chain-watcher`。

## 同步路径

同步路径只处理用户正在等待即时反馈的操作：

- Telegram 更新接收和 update 去重。
- 群内记账、撤销、开始/停止、设置费率/汇率。
- 私聊菜单、地址监听配置、广播控制台选择。
- 轻量权限判断和缓存读取。

同步路径必须遵守：

- 不直接做慢 HTTP 查询。
- 不直接跑大广播。
- 不直接做大量数据库扫描。
- 需要慢操作时，写入任务队列并立即回复状态。

## 异步路径

异步路径处理可能慢或会阻塞的任务：

- 群发/分组广播。
- 通知所有人。
- 链上监听订阅同步和 watcher 事件领取。
- TRX 地址查询。
- Z0/实时汇率刷新。
- 广播回复通知。
- Web 后台导出账单。

异步任务要带任务 ID、创建人、目标范围、状态和失败原因，方便后台追踪。

## 队列模型

Go 版使用两层队列：

1. Telegram 更新分发队列
   - 按 `chat_id` 建立串行队列。
   - 同一群同一时间只处理一个账本更新。
   - 不同群可以并发。
   - 私聊按 `user_id` 串行。

2. 功能任务队列
   - `ledger_pool`: 群内记账和群设置。
   - `control_pool`: 私聊菜单、按钮回调、后台入口。
   - `chain_pool`: watcher 订阅同步、matched events 领取和本地入 outbox。
   - `rate_pool`: 汇率刷新和 Z0 查询。
   - `broadcast_pool`: 群发/分组广播。
   - `notify_pool`: 广播回复通知、链上提醒发送。
   - `query_pool`: TRX 地址查询。

每个功能池都是弹性池：默认保留少量常驻 goroutine，队列堆积时自动扩容到配置上限，空闲 30 秒后自动缩回。配置项里的线程数表示最大并发，不表示长期占用。

## 并发策略

- 群内账本：按群串行，跨群并发。
- 热群加速：同一群账本写入仍串行，但回执发送、账单快照重算、通知、TRX 查询、缓存预热等旁路任务进入独立池。
- 私聊控制台：按用户串行，跨用户并发。
- 链上监听：机器人同步监听地址到 `ledger-chain-watcher`，watcher 统一拉取链上流水并按 bot_id 投递匹配事件。
- 广播：任务级并发，单个任务内部按目标群限速发送。
- 通知：独立池，避免 Telegram 发送慢拖住监听或记账。
- 少量群活跃时，空闲 CPU/内存优先服务这些热群：提前刷新权限/群配置缓存，异步发送回执，预计算账单摘要，维持地址监听索引，减少下一笔操作等待。

## 缓存设计

缓存只缓存热读和非关键维护数据，不缓存账本流水本身。

- `group_cache`: 群基础信息、群名、记账状态，TTL 60 秒，写操作主动失效。
- `user_touch_cache`: 群成员最近触达，TTL 180 秒，减少高频 touch 写入。
- `operator_cache`: 权限判断，TTL 10 秒，权限变更主动失效。
- `watch_target_cache`: 地址监听目标，TTL 3 秒，增删监听地址主动失效。
- `watch_settings_cache`: 监听开关和最小金额，TTL 3 秒，设置变更主动失效。
- `rate_cache`: Z0 TOP10 和实时汇率，全局缓存，默认 60 秒刷新。

## 数据库策略

v2.3 起继续直接使用 PostgreSQL：

- `pgxpool` 连接池，默认 `MaxConns=32`、`MinConns=4`。
- Telegram ID 使用 `BIGINT`。
- 时间使用 `TIMESTAMPTZ`，业务统一按北京时间展示。
- 开关使用 `BOOLEAN`，不再用 0/1 混写。
- 账本写入、权限变更、广播任务状态更新使用事务。
- 幂等表记录 Telegram update 和链上 tx hash，避免重复处理。
- 高频路径从第一版就建立组合索引：权限、账单日切窗口、撤销消息定位、地址监听目标、链上去重、广播回执定位。

测试阶段直接使用 PostgreSQL 空库验证 Go v2.3 的账本、广播、监听和后台逻辑。

## 链上监听策略

- 机器人侧保存监听地址、监听开关、最小金额、tx_hash 去重和 Telegram outbox。
- 机器人通过 `CHAIN_WATCHER_URL`、`CHAIN_WATCHER_BOT_ID`、`CHAIN_WATCHER_SECRET` 接入共享 watcher。
- watcher 使用独立 PostgreSQL 保存 bot 凭据、订阅、链上事件、匹配事件和投递游标。
- watcher 第一版统一请求 Tronscan/TronGrid，后续可切换 TRON Lite FullNode + Event Plugin V2 + Kafka。
- watcher 按 `bot_id` 隔离订阅和投递；机器人实例之间不共享业务数据库。
- 通知发送仍进入机器人 `notify_pool`，watcher 不直接发送 Telegram。

## 发布策略

Go v2.3 是当前唯一发布主线。每次发布前按模块测试账本、广播、监听、后台、chain-watcher 和部署配置，确认后再推送镜像。
