# Telegram 群组记账机器人

这是 Telegram 记账机器人 Go/PostgreSQL 主线；当前源码候选版本为 v2.4.10，只有显式发布 workflow 成功后才会产生正式镜像、tag 和 Release。生产部署使用 PostgreSQL、GHCR 镜像和共享 `ledger-chain-watcher` 链上监听服务。

v2.4.10 发布说明见 [docs/releases/v2.4.10.md](docs/releases/v2.4.10.md)，生产升级、回滚限制和验收见 [docs/production-rollout-v2.4.10.md](docs/production-rollout-v2.4.10.md)。

## v2.4.10 发布范围

- 四项记账性能优化：全局权限能力缓存、账期汇总物化与安全核对、账变与关键回执同事务提交、按优先级隔离更新和发送队列。
- Telegram update inbox 和私聊路由状态落 PostgreSQL；执行时权限、当前账期和清账 60 秒票据不再被旧 update 时间冻结。
- 快速回复改为 durable outbox，具备撤权双重验权、租约崩溃恢复、429/5xx/网络重试、400/403 终态和 72 小时终态清理。
- 新增 `2.4.14-telegram-private-route-state` 与 `2.4.15-telegram-quick-reply-outbox` 前向迁移。watcher 和 bot 共用迁移入口，必须先备份并同步升级到 v2.4.10。
- Telegram 已接收请求但响应 ACK 丢失仍是外部系统边界，可能产生无法完全消除的不确定性。

重装系统或换机器前后的当前状态交接见 [docs/reinstall-handoff.md](docs/reinstall-handoff.md)。

## 当前已支持功能

- 每个群默认只保存群名、支持广播和转发回复通知，不会记账；有权限的人在群内发送 `开始` 后，才激活该群记账。
- 非操作人发送 `开始` 会提示 `没有操作权限。请管理员添加操作员。`，不会把首次发送者升为最高权限。
- `停止` 或 `关闭` 暂停记账。
- 日切后当前群记账激活状态自动失效，新业务日需要重新发送 `开始`；汇率、费率、操作员和广播配置继续保留。
- `上课` / `下课` 调整群员发言权限，机器人需要群管理员权限。
- `添加操作员 @user`、`删除操作员 @user`，也支持回复对方消息后发送 `添加操作员`。
- `设置费率3`、`设置入款费率3`、`设置下发费率3`。
- `设置汇率7.1`、`设置入款汇率7.1`、`设置下发汇率7.1`。
- 入款：`+1000`、`+1000/7.1`、`+1000*5`、`+1000*5/7.1`、`+1000*12%`、`+1000U`。
- 下发：`下发5000U`、`下发5000/7.8`、`下发5000*5/7.1`。裸命令 `下发5000` 不入账，避免把人民币误当成 U。
- 群内运算：`1000/6.8`、`1000/7.5-1000/6.8`，机器人直接回复 `原式=结果`，不写入账单。
- `Z0` 查询 OKX OTC 商家所有实时汇率 TOP 10，方向为 USDT 卖 CNY；`设置汇率 Z1 -0.1` 按第 1 档下浮 0.1 设置入款汇率，`设置汇率 Z1 +0.1` 则上浮 0.1。
- 使用 `Z1 -0.1` 或 `设置汇率 Z1 -0.1` 后，账单底部显示为两行：`实时汇率：`、`支付宝1档 下浮0.10`；偏移为 `0`、`+0`、`-0` 时只显示 `支付宝1档`。
- 私聊菜单：开始记账、详细说明、群发广播、分组广播、地址监听、群列表、广播权限、广播替换、后台管理。
- 机器人只有一个宿主，宿主由服务器配置 `BOT_HOST_USER_ID` 指定；宿主是最高权限。
- 默认操作人只由维护人员修改服务器配置 `DEFAULT_OPERATOR_USER_IDS` 添加/删除。默认操作人可以邀请机器人进群、在任意群记账、使用群发广播和分组广播，但不会因为发送 `开始` 或拥有默认权限而变成本群最高权限。
- 宿主、默认操作人或本群最高权限可以在群内发送 `添加操作员 @user` / `删除操作员 @user` 设置单群操作人。
- 机器人被邀请进群或群里有人发言时会按 `chat_id` 保存群组，群名变更后会自动更新；邀请人必须是宿主、默认操作人或 active 一级/下级全局操作人，否则机器人会自动退出。
- 只用于广播、不需要记账的群可以一直不发送 `开始`；未开始的群里普通成员发送 `+100`、`下发100U` 等金额文本时，机器人不会插入记账提示。已经开启过记账的群可由操作人发送 `停止` 或 `关闭` 暂停记账，群仍会保留在群发/分组广播列表中。
- 私聊点击 `📡群发广播` 或 `📣分组广播` 后，可用按钮选择全部群、分组或单群；选择目标后进入连续发送模式，文字、图片、文件或图文都会直接投递，不再二次确认。
- 广播分组、分组成员、单群授权、广播操作人和广播替换都放到后台管理页操作，后台可按群名搜索或多选群组，避免手动查群 ID。
- 广播权限和记账权限分开：宿主/默认操作人拥有全部广播权限；宿主可创建一级或下级全局操作人，一级只能创建和禁用自己的下级。
- 一级操作人只能把自己已有的分组或单群继续分配给自己的下级；下级不能继续创建操作人或授权。禁用后旧广播目标授权会清除，重新启用不会自动恢复。
- 宿主、默认操作人和 active 一级/下级全局操作人可在所在群执行完整记账命令；单群 `operators` 只在对应群内拥有记账权限，不获得私聊后台、广播、邀请或地址监听无限额能力。
- 撤销只允许当前有效账期且必须重新检查当前记账权限；日切封账或开始新账期后，任何身份都不能撤销上一账期记录。
- 私聊广播流程里有 `通知所有人` 按钮开关，开启后会在目标群内 @ 所有有过发言记录的用户。
- 群成员回复机器人投递出去的广播消息时，机器人会按发送任务记录通知原广播操作人和宿主。
- 广播替换在后台管理中维护，可开启/关闭、直接上传替换图片或填写图片 URL/file_id，并设置替换文字；它只作用于单群发送被群成员回复后的原投递消息。
- 群内直接发送单个 TRC20 地址时，首次出现会回复 USDT 防篡改核对图、创建时间、USDT/TRX 余额和验证次数；同一群后续再次发送相同地址，会回复验证次数、上次发送人和本次发送人。
- 地址监听：普通用户私聊最多可监听 2 个 TRC20 地址；只有宿主、`DEFAULT_OPERATOR_USER_IDS` 和 active `global_operators` 不受数量限制，单群 `operators` 不获得私聊全局资格。私聊点击 `🔔地址监听` 打开按钮面板，可设置地址、删除地址、备注、最小提醒金额以及 USDT 收入/支出方向。TRX 地址余额查询仍可用，但当前链源没有真实 TRX 事件通知，因此 UI 不展示 TRX 通知开关。
- TRC20 USDT 交易提醒：收入显示出账地址和入账监听地址，支出显示出账监听地址和入账地址；地址可点按/长按复制，交易哈希可点击跳转 Tronscan。
- 群内地址验证按单群单地址累计次数，不需要后台提前配置。
- `查询Txxxx` 可查询 TRX 地址，返回 TRX/USDT 余额、创建时间、活跃时间、权限和最近 USDT 流水；私聊直接发送 T 地址也可以查询。
- `+0` 查看简洁账单，`显示账单` 查看完整今日账单。
- 加账本人回复自己发送的原始加账消息，发送 `撤销` / `撤销入款` / `撤销下发` 可撤销。
- `删除账单`、`清除今日账单` 会二次确认。
- `简洁模式10` / `显示条数10`、`完整模式`。
- 默认按北京时间 00:00 日切，可用 `设置日切6` 改到 6 点，`设置日切-1` 或 `关闭日切` 可关闭自动日切。
- 修改日切不会立刻拆断已经开始且已有流水的账期，而是从“当前账期结束后遇到的新日切点”开始生效：例如 7 月 3 日 12:00 从 0 点改到 4 点，当前账期延续到 7 月 4 日 04:00；从 4 点改到 0 点，当前账期延续到 7 月 5 日 00:00。如果当前账期没有任何入款/下发流水，则新日切立即生效。
- `开启记账置顶` / `关闭记账置顶`。
- 每个群的 `🌐 完整账单` 按钮会按 `chat_id` 生成独立链接。

## 运行 Go v2.4.10

正式发布后，使用 Go v2.4.10 镜像、PostgreSQL 和共享 watcher：

```powershell
docker pull ghcr.io/dayou0168/telegram-ledger-bot-go:2.4.10
docker pull ghcr.io/dayou0168/telegram-ledger-chain-watcher:2.4.10
```

宝塔 Docker Compose 可以直接使用仓库里的 [docker-compose.ghcr.yml](docker-compose.ghcr.yml)。这个文件默认拉取 `ghcr.io/dayou0168/telegram-ledger-bot-go:2.4.10` 和 `ghcr.io/dayou0168/telegram-ledger-chain-watcher:2.4.10`，在同一个 Compose 项目里用独立 PostgreSQL 容器和独立 watcher 容器组成全家桶。在 Release 可用前不要在生产执行该模板。

如果 PostgreSQL 已经安装在宿主机/宝塔里，使用 [docker-compose.baota-host-pg.yml](docker-compose.baota-host-pg.yml)。这个文件只运行 `ledger-chain-watcher` 和 `ledger-bot`，通过 `host.docker.internal` 连接宿主机 PostgreSQL。

如果希望 `ledger-chain-watcher` 直接跑在宿主机 systemd 里，v2.4.10 正式 Release 会同时发布 `ledger-chain-watcher-v2.4.10-linux-amd64.tar.gz` 宿主机包，里面包含二进制、[deploy/ledger-chain-watcher.env.example](deploy/ledger-chain-watcher.env.example) 和 [deploy/ledger-chain-watcher.service](deploy/ledger-chain-watcher.service)。本轮必须先更新 watcher，再把所有机器人实例同步升级到 v2.4.10；机器人 Compose 保留自己的 `DATABASE_URL`，并把 `CHAIN_WATCHER_URL` 配成 `http://host.docker.internal:8090` 或 Docker 网桥 IP。

部署模板默认限制 Docker `json-file` 日志为 `max-size=20m`、`max-file=5`，单容器约 100MB。host systemd 版 watcher 建议用 journald `MaxRetentionSec=3day` 和 `SystemMaxUse=500M` 控制最近约 72 小时日志，详见 [docs/deployment.md](docs/deployment.md)。

广播和记账是一体能力，不需要为“广播群”单独关闭机器人记账模块。群默认未开始记账；需要记账的群由宿主、默认操作人或本群操作员发送 `开始` 即可开启。

同一个机器人实例只使用一个 PostgreSQL 数据库同时保存广播/转发配置和记账数据；不要为同一个 bot 再拆“广播库”和“记账库”。共享 `ledger-chain-watcher` 仍然使用独立数据库。

### 推荐：多机器人快速安装

宿主机已经安装宝塔 PostgreSQL 和 systemd watcher 时，新机器人不再手写几十项 Compose 环境变量。配置分为三层：

- `deploy/ledger-instance.env.example`：每个机器人只填写实例简称、Bot Token、Bot 用户名、宿主 UID、域名 5 项；
- `deploy/ledger-instance-shared.env.example`：本机只初始化一次的 PostgreSQL、镜像和宝塔路径；
- `deploy/config/`：机器人线程、超时、汇率以及 watcher 扫描、补偿等高级默认参数。

初始化共享配置一次：

```bash
install -d -m 700 /etc/telegram-ledger /etc/telegram-ledger/config
install -m 600 deploy/ledger-instance-shared.env.example /etc/telegram-ledger/shared.env
install -m 644 deploy/config/ledger-bot.defaults.env /etc/telegram-ledger/config/bot-defaults.env
vi /etc/telegram-ledger/shared.env
```

以后每个机器人只执行：

```bash
cp deploy/ledger-instance.env.example /root/new-bot.env
vi /root/new-bot.env
bash deploy/ledger-instance-manager.sh plan /root/new-bot.env
bash deploy/ledger-instance-manager.sh install /root/new-bot.env --apply
```

安装器自动创建独立数据库、生成后台密码和 watcher 密钥、注册 watcher、选择端口、生成精简 Compose/Nginx 并启动验收。任一步失败会回滚本次创建的资源，不影响已有机器人。完整说明见 [docs/deployment.md](docs/deployment.md#多机器人快速安装)。

本地源码构建和更完整的 Go 运行说明见 [go-ledger/README.md](go-ledger/README.md)。

1. 复制配置：

```powershell
Copy-Item .env.example .env
```

2. 编辑 `.env`，填入你的 Bot Token：

```env
TELEGRAM_BOT_TOKEN=123456:replace-me
TELEGRAM_BOT_USERNAME=your_bot_username
TELEGRAM_API_BASE=https://api.telegram.org
DATABASE_URL=postgres://ledger:change_this_strong_password@postgres:5432/ledger_bot?sslmode=disable
CHAIN_WATCHER_URL=http://ledger-chain-watcher:8090
CHAIN_WATCHER_BOT_ID=ledger-main
CHAIN_WATCHER_SECRET=change_this_chain_watcher_secret
BOT_TIMEZONE=Asia/Shanghai
BOT_HOST_USER_ID=123456789
DEFAULT_OPERATOR_USER_IDS=
BOT_WORKER_THREADS=16
BOT_CONTROL_THREADS=6
BOT_CHAIN_THREADS=12
BOT_RATE_THREADS=2
BOT_BROADCAST_THREADS=4
BOT_QUERY_THREADS=4
BOT_NOTIFICATION_THREADS=6
BOT_QUEUE_SIZE=4096
```

先私聊机器人发送 `我的ID` 获取你的 Telegram ID，再填入 `BOT_HOST_USER_ID`。机器人只允许配置一个宿主。默认操作人可在 `.env` 的 `DEFAULT_OPERATOR_USER_IDS` 里用英文逗号分隔，只有维护程序的人员能通过改服务器配置添加或删除。

并发池说明：默认按 4 核 8G 服务器优化。`BOT_WORKER_THREADS` 处理群内记账、撤销、群内设置等实时消息，同一群会按 FIFO 队列串行、不同群可并发；`BOT_CONTROL_THREADS` 处理私聊菜单、后台入口、广播控制台等私聊交互；`BOT_CHAIN_THREADS` 处理 watcher 订阅同步和 matched events 领取；`BOT_RATE_THREADS` 处理 Z0 和实时汇率刷新；`BOT_BROADCAST_THREADS` 处理群发/分组广播，默认最多同时跑 4 个广播任务，单个广播任务内部仍按目标群逐个发送，避免触发 Telegram 限流；`BOT_QUERY_THREADS` 处理 TRX 地址查询等外部查询；`BOT_NOTIFICATION_THREADS` 处理广播发送通知、回复通知和通知素材复制。群内记账、私聊控制、广播、链上监听、汇率刷新、查询、通知互相隔离，避免某一类慢任务拖住其他版块。

如果你用自建 Telegram Bot API Server，`TELEGRAM_API_BASE` 填服务根地址即可，例如：

```env
TELEGRAM_API_BASE=http://telegram-bot-api:8081
```

不要把 `/bot<TOKEN>/sendMessage` 这类完整接口路径填进去，程序会自动拼接。

### 完整账单网页

Go 版镜像内置账单网页和后台服务，默认监听容器 `8080` 端口。配置自己的域名后：

```env
PUBLIC_BILL_BASE_URL=https://bot.your-domain.example
ADMIN_WEB_ENABLED=1
ADMIN_WEB_HOST=0.0.0.0
ADMIN_WEB_PORT=8080
ADMIN_WEB_TOKEN=change-this-admin-token
```

机器人会给每个群生成类似这样的独立链接：

```text
https://bot.your-domain.example/b/-100xxx/20260705
```

宝塔里给域名申请 SSL 后，把站点反向代理到 `http://127.0.0.1:8080`。`ADMIN_WEB_TOKEN` 是 `/admin` 后台登录密码和会话签名密钥，建议设置为一串随机强密码。

后台入口：

```text
https://bot.your-domain.example/admin
```

宿主、`DEFAULT_OPERATOR_USER_IDS` 和 active `global_operators` 可以在私聊菜单点击 `⚙后台管理` 获取 5 分钟有效的后台登录链接。宿主可查看和管理全部后台数据；默认操作人和 active 全局操作人进入后台后只看到自己可管理的内容。单群 `operators` 不获得私聊后台资格。直接打开 `/admin/login` 仍可使用 `ADMIN_WEB_TOKEN` 作为宿主紧急入口。

后台用标签页拆分为已保存群组、广播分组、权限/操作人、地址监听和广播替换，可管理广播操作人、广播分组、分组/单群权限、监听地址开关和广播替换开关。

内置网页同时兼容 `/day_xxb.php` 风格参数。打开：

```text
https://bot.your-domain.example/day_xxb.php?chat_id=-100xxx&created_at=2026-07-05
```

即可查看指定日期历史账单；页面顶部会显示最近历史日期和「下载账单」按钮。下载按钮会生成 `.xlsx` 文件，文件名格式为 `账单_日期_群名_时间戳.xlsx`。也可以直接追加 `download=excel` 下载当前日期或当前时间窗口的账单。

按钮逻辑：配置了 `PUBLIC_BILL_BASE_URL` 时，底部「🌐 完整账单」会打开网页；不配置时只发送 Telegram 内账单摘要。

### TRC20 链上监听

Go v2.4.10 通过共享 `ledger-chain-watcher` 获取链上数据。多个机器人实例只配置内网 URL 和内部密钥，不再各自配置官网 API Key：

```env
CHAIN_WATCHER_URL=http://ledger-chain-watcher:8090
CHAIN_WATCHER_BOT_ID=ledger-main
CHAIN_WATCHER_SECRET=change_this_chain_watcher_secret
CHAIN_WATCHER_POLL_SECONDS=1
CHAIN_WATCHER_BATCH_SIZE=50
BOT_WATCHER_HEALTH_INTERVAL_SECONDS=1
BOT_WATCHER_FAIL_THRESHOLD=3
BOT_WATCHER_CLAIM_TIMEOUT_MS=2000
BOT_FALLBACK_POLL_SECONDS=1
BOT_FALLBACK_SHARED_DATABASE_URL=postgres://chainwatcher:***@chain-postgres:5432/ledger_chain_watcher?sslmode=disable
TRON_USDT_CONTRACT=TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t
# 多 bot 通过共享 PostgreSQL lease 只选一个无 Key leader fallback；仅明确批准持久在线备用源时填写 Key。
TRONGRID_API_KEY=
```

宿主机 watcher 的短配置只保留数据库、管理密钥、API Key 和机器人注册信息：

```env
CHAIN_WATCHER_DATABASE_URL=postgres://chainwatcher:change_this_chain_pg_password@chain-postgres:5432/ledger_chain_watcher?sslmode=disable
CHAIN_WATCHER_ADMIN_TOKEN=change_this_watcher_admin_token
CHAIN_WATCHER_KEY_ENCRYPTION_KEY=base64_encoded_32_byte_key
CHAIN_WATCHER_BOTS=
CHAIN_WATCHER_TRONSCAN_API_KEYS=key1,key2,key3
CHAIN_WATCHER_TRONSCAN_API_KEY=
CHAIN_WATCHER_TRON_API_KEY=
```

扫描并发、动态扩页、缺口补偿和 Key 状态机参数统一位于 [deploy/config/ledger-chain-watcher.defaults.env](deploy/config/ledger-chain-watcher.defaults.env)，不需要在每次新增机器人时重复填写。`CHAIN_WATCHER_BOTS` 由快速安装器自动追加和回滚。

watcher 每秒用同一个 cutoff 扫描，并保存上一轮稳定 event identity 锚点。每个健康 Key 按 `剩余额度/距离 UTC 00:00 的秒数` 产生可持续令牌：完整额度下每 Key 每秒约贡献 1 个基础页，页数不是固定值，也不由旧 `GLOBAL_SCAN_PAGES` 控制。基础页并发返回后立即匹配和持久化；P1 使用独立高优先持久化 lane，P2..PN 进入默认 8 worker 的普通池，不会让慢页形成全局串行瓶颈。找到锚点或非满页后停止继续扩页；未覆盖范围进入精确持久化 gap/catch-up，从 cursor 补到 realtime head 前 2 秒。没有缺口时不请求补偿 API；逐地址扫描默认关闭。

Key 注册表不设固定数量上限，可通过管理接口随时热增删、启停；调度按当时健康状态和可持续 RPS 动态降档或恢复。默认每 Key 日规划额度为 100,000 次，重置点 UTC 00:00（北京时间 08:00）；完整额度下基础主扫约 86,400 次/Key/天，约余 13,600 次进入共享 surplus。10 个完整健康 Key 的基础能力约 10 页/秒，surplus 合计约 136,000 次/天（1.574 RPS）。`CHAIN_WATCHER_CATCHUP_MAX_RPS=0` 表示不再施加额外固定上限，但补洞仍只能消费 surplus token；60 秒突发桶限制空闲后的集中请求。本地计数只用于规划、均衡和观测，不静默硬停实时头部；服务端实际 429/额度响应才是权威。单 Key 200ms 间隔继续保证不超过 5 RPS。Key 使用 AES-256-GCM 加密保存，日志与 status 只显示短指纹。

watcher 有两种部署模式：

- Compose 版：`docker-compose.ghcr.yml` 或 `docker-compose.chain-watcher.yml`，同一个 Compose 项目中包含 `chain-postgres` 独立容器和 `ledger-chain-watcher` 独立容器。这样更利于数据卷持久化、升级、备份、健康检查和故障隔离；宝塔里仍然是一套项目，一起启动/停止。
- 宿主机版：PostgreSQL 在宝塔/宿主机上，`ledger-chain-watcher` 用 systemd 运行，模板见 `deploy/ledger-chain-watcher.env.example` 和 `deploy/ledger-chain-watcher.service`。机器人容器通过 `host.docker.internal` 或 `172.17.0.1` 访问 `8090`。

普通用户私聊点击 `🔔地址监听` 后，最多可添加 2 个监听地址；只有宿主、`DEFAULT_OPERATOR_USER_IDS` 和 active `global_operators` 不受数量限制，单群 `operators` 不获得该资格。面板按钮支持添加监听地址、设置备注和最小提醒金额。最小提醒金额表示小于这个数的 USDT 交易不提醒，设置 `0` 表示不限制。

机器人保存监听地址、event identity 去重和 Telegram outbox；watcher 负责统一链上数据入口、统一解析、按多机器人订阅匹配和短期事件队列。机器人配置了 `CHAIN_WATCHER_URL`、`CHAIN_WATCHER_BOT_ID`、`CHAIN_WATCHER_SECRET` 后，默认停用本机地址监听轮询，改为注册订阅并每秒领取 watcher matched events。事件身份优先使用 `tx_hash + event/log index + contract`，缺少 index 时使用稳定 fingerprint，支持同一交易内多个 Transfer 且不会因 realtime/catch-up/fallback 重叠而重复提醒。

`/healthz` 只表示 watcher 进程存活；`/readyz` 综合链源新鲜度、连续 watermark 和未闭合 gap，cursor 未建立时明确显示 `catchup_lag_unknown=true`。响应同时区分 `source_ready` 与 `continuity_ready`：历史 gap 未闭合时完整 readiness 仍为 503，但链源持续成功不会单独触发新的 bot fallback；claim 或链源真正失败仍会触发。`/status` 受 `CHAIN_WATCHER_ADMIN_TOKEN` 保护，包含 round ID、最多 3 个在途主轮次、分段耗时、429/key 状态、pending/claim lag、gap 和 retention 统计；最近轮次明细有界保存，分钟聚合保留 72 小时。

主扫描固定每秒一轮，实际基础页数由小数 base token 累积产生。`CHAIN_WATCHER_MAIN_SCAN_TIMEOUT_MS=3000` 是整轮独立 API deadline；`CHAIN_WATCHER_MAIN_MAX_INFLIGHT_ROUNDS=3` 允许慢轮次有界流水并发。`HEAD_MAX_CONCURRENCY=32` 只限制 API 页任务，`HEAD_PERSIST_CONCURRENCY=8` 只限制 P2..PN 普通持久化 worker，P1 另有保留 lane。成功页立即落库，失败页形成带 lease generation 的精确 gap。`CHAIN_WATCHER_CATCHUP_MAX_INFLIGHT=8` 是独立 worker 安全天花板，实际并发按健康 Key 和 surplus 自动伸缩。重叠时间窗会合并，公平调度保证旧低优先级 window 在持续高优先级流量下仍被领取。

自动 fallback 使用 `PRIMARY -> FAILOVER_PENDING -> FALLBACK_ACTIVE -> RECOVERING` 状态机；watcher 连续异常 3 秒后，多个 bot 通过 PostgreSQL lease 只选出一个无 Key 公共扫描 leader。leader 使用同 cutoff、共享锚点和精确 continuation 动态扫描，既不是固定 3 页也不是逐地址扫描；429 时按 1/2/3/5/10 秒退避。watcher 恢复并补齐共享 cursor，ready/claim 连续成功且 lag 归零后才释放 lease。未配置共享 DSN 时明确 DEGRADED，不会退回每 bot 逐地址扫描。

部署时每个 bot 还必须配置唯一且重启后保持不变的 `BOT_FALLBACK_INSTANCE_ID`。它与 `BOT_FALLBACK_SHARED_DATABASE_URL` 共同用于 leader lease；任一缺失都只报告降级，不启动旧本地扫描。

### Z0 汇率查询

默认查询 OKX P2P 的 USDT 卖 CNY 支付宝订单簿：

```env
P2P_RATE_API_BASE=https://p2p.army/api/fapi
P2P_RATE_FRONT_API=NextVOF2Ozuh36mW0TCv
P2P_RATE_MARKET=okx
P2P_RATE_FIAT_UNIT=CNY
P2P_RATE_ASSET=USDT
P2P_RATE_TRADE_METHODS=aliPay
P2P_RATE_REFRESH_SECONDS=60
P2P_RATE_CACHE_TTL_SECONDS=180
```

后台每 `P2P_RATE_REFRESH_SECONDS` 秒只请求一次 TOP10，所有开启实时汇率的群共用这份结果，再按各群自己的 Z 档位和偏移写入该群保存汇率。加账时只使用数据库里已经保存的汇率，不等待外部接口；如果某次刷新失败，会继续沿用上一次成功保存的汇率。`P2P_RATE_CACHE_TTL_SECONDS` 只控制手动设置 Z 档位时可复用内存缓存的时间，不是加账汇率有效期。

群里发送 `Z0` 会显示 `OKX OTC商家所有实时汇率 TOP 10`，不显示限额。发送 `设置汇率 Z1` 会按第 1 档价格写入当前群入款汇率；`设置汇率 Z1 -0.1` 会取第 1 档价格减 0.1；`设置汇率 Z1 +0.1` 会取第 1 档价格加 0.1。兼容简写 `Z1 -0.1`。

3. 启动：

```powershell
docker compose -f docker-compose.ghcr.yml up -d
```

## 一键发布入口

Windows 发布线程使用 [scripts/publish-release.ps1](scripts/publish-release.ps1)。脚本要求 clean 的 `codex/` 集成分支、HTTPS origin、仓库局部 OpenSSL、Git Credential Manager 和有效的 `gh` OAuth；它不会读取或输出凭据，也不会连接生产服务器。

先执行只读/dry-run 门禁：

```powershell
pwsh -File scripts/publish-release.ps1 -DryRun
```

获得发布授权并完成本地测试后，单命令执行 push、等待普通 CI、显式 Release 和资产验收：

```powershell
pwsh -File scripts/publish-release.ps1 -Version 2.4.10
```

普通 push 只运行 CI。脚本只有在 PostgreSQL CI、race 和静态构建全部成功后才调用 `workflow_dispatch(version=2.4.10)`；任一步失败都会停止，不会自动部署。

服务器部署见 [docs/deployment.md](docs/deployment.md)。v2.4.10 必须在显式发布 workflow 成功、实际 digest 和校验和可用后，再按 [生产升级、回滚与验收](docs/production-rollout-v2.4.10.md) 先更新 watcher、确认 `source_ready=true`，再同步更新所有 bot；`continuity_ready=false` 仍只表示历史 gap 正在收敛。外层旧工作区部署文件不得回灌，唯一候选基线是本集成仓库的已确认发布提交。
