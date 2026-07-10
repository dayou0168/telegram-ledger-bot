# Telegram 群组记账机器人

这是 Telegram 记账机器人 Go v2.3 主线，按群组使用，生产部署使用 PostgreSQL、GHCR 镜像和共享 `ledger-chain-watcher` 链上监听服务。

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
- 宿主或默认操作人可以在群内发送 `添加操作员 @user` / `删除操作员 @user` 设置单群操作人。
- 机器人被邀请进群或群里有人发言时会按 `chat_id` 保存群组，群名变更后会自动更新；邀请人必须是宿主或默认操作人，否则机器人会自动退出。
- 只用于广播、不需要记账的群可以一直不发送 `开始`；未开始的群里普通成员发送 `+100`、`下发100U` 等金额文本时，机器人不会插入记账提示。已经开启过记账的群可由操作人发送 `停止` 或 `关闭` 暂停记账，群仍会保留在群发/分组广播列表中。
- 私聊点击 `📡群发广播` 或 `📣分组广播` 后，可用按钮选择全部群、分组或单群；选择目标后进入连续发送模式，文字、图片、文件或图文都会直接投递，不再二次确认。
- 广播分组、分组成员、单群授权、广播操作人和广播替换都放到后台管理页操作，后台可按群名搜索或多选群组，避免手动查群 ID。
- 广播权限和记账权限分开：宿主/默认操作人拥有全部广播权限；一级广播操作人可以继续创建一层下级，并把自己已有的分组或单群权限分配给下级。
- 一级操作人只能把自己已有的分组或单群继续分配给下级；下级不能继续创建操作人。操作人创建的分组会自动授权给自己。
- 私聊广播流程里有 `通知所有人` 按钮开关，开启后会在目标群内 @ 所有有过发言记录的用户。
- 群成员回复机器人投递出去的广播消息时，机器人会按发送任务记录通知原广播操作人和宿主。
- 广播替换在后台管理中维护，可开启/关闭、直接上传替换图片或填写图片 URL/file_id，并设置替换文字；它只作用于单群发送被群成员回复后的原投递消息。
- 群内直接发送单个 TRC20 地址时，首次出现会回复 USDT 防篡改核对图、创建时间、USDT/TRX 余额和验证次数；同一群后续再次发送相同地址，会回复验证次数、上次发送人和本次发送人。
- 地址监听：普通用户私聊最多可监听 2 个 TRC20 地址；宿主、默认操作人、一级操作人和下级操作人不受这个数量限制。私聊点击 `🔔地址监听` 打开按钮面板，可按钮式设置地址、删除地址、设置备注、设置最小提醒金额；USDT 监听只支持 TRC20，收入/支出/TRX 通知可单独开关。
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

## 运行 Go v2.3

推荐优先使用 Go v2.3 镜像、PostgreSQL 和共享 watcher：

```powershell
docker pull ghcr.io/dayou0168/telegram-ledger-bot-go:2.3
docker pull ghcr.io/dayou0168/telegram-ledger-chain-watcher:2.3
```

宝塔 Docker Compose 可以直接使用仓库里的 [docker-compose.ghcr.yml](docker-compose.ghcr.yml)。这个文件默认拉取 `ghcr.io/dayou0168/telegram-ledger-bot-go:2.3` 和 `ghcr.io/dayou0168/telegram-ledger-chain-watcher:2.3`，在同一个 Compose 项目里用独立 PostgreSQL 容器和独立 watcher 容器组成全家桶，适合快速部署。

如果 PostgreSQL 已经安装在宿主机/宝塔里，使用 [docker-compose.baota-host-pg.yml](docker-compose.baota-host-pg.yml)。这个文件只运行 `ledger-chain-watcher` 和 `ledger-bot`，通过 `host.docker.internal` 连接宿主机 PostgreSQL。

如果希望 `ledger-chain-watcher` 直接跑在宿主机 systemd 里，GitHub Release `v2.3` 会同时发布 `ledger-chain-watcher-v2.3-linux-amd64.tar.gz` 宿主机包，里面包含二进制、[deploy/ledger-chain-watcher.env.example](deploy/ledger-chain-watcher.env.example) 和 [deploy/ledger-chain-watcher.service](deploy/ledger-chain-watcher.service)。机器人 Compose 保留自己的 `DATABASE_URL`，并把 `CHAIN_WATCHER_URL` 配成 `http://host.docker.internal:8090` 或 Docker 网桥 IP。

广播和记账是一体能力，不需要为“广播群”单独关闭机器人记账模块。群默认未开始记账；需要记账的群由宿主、默认操作人或本群操作员发送 `开始` 即可开启。

同一个机器人实例只使用一个 PostgreSQL 数据库同时保存广播/转发配置和记账数据；不要为同一个 bot 再拆“广播库”和“记账库”。共享 `ledger-chain-watcher` 仍然使用独立数据库。

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

宿主、默认操作人和已启用的广播操作人可以在私聊菜单点击 `⚙后台管理` 获取 5 分钟有效的后台登录链接。宿主可查看和管理全部后台数据；默认操作人和普通操作人进入后台后只看到自己可管理的内容，例如自己的地址监听。直接打开 `/admin/login` 仍可使用 `ADMIN_WEB_TOKEN` 作为宿主紧急入口。

后台用标签页拆分为已保存群组、广播分组、权限/操作人、地址监听和广播替换，可管理广播操作人、广播分组、分组/单群权限、监听地址开关和广播替换开关。

内置网页同时兼容 `/day_xxb.php` 风格参数。打开：

```text
https://bot.your-domain.example/day_xxb.php?chat_id=-100xxx&created_at=2026-07-05
```

即可查看指定日期历史账单；页面顶部会显示最近历史日期和「下载账单」按钮。下载按钮会生成 `.xlsx` 文件，文件名格式为 `账单_日期_群名_时间戳.xlsx`。也可以直接追加 `download=excel` 下载当前日期或当前时间窗口的账单。

按钮逻辑：配置了 `PUBLIC_BILL_BASE_URL` 时，底部「🌐 完整账单」会打开网页；不配置时只发送 Telegram 内账单摘要。

### TRC20 链上监听

Go v2.3 通过共享 `ledger-chain-watcher` 获取链上数据。多个机器人实例只配置内网 URL 和内部密钥，不再各自配置官网 API Key：

```env
CHAIN_WATCHER_URL=http://ledger-chain-watcher:8090
CHAIN_WATCHER_BOT_ID=ledger-main
CHAIN_WATCHER_SECRET=change_this_chain_watcher_secret
CHAIN_WATCHER_POLL_SECONDS=1
CHAIN_WATCHER_BATCH_SIZE=50
CHAIN_WATCHER_EMERGENCY_FALLBACK=false
TRON_USDT_CONTRACT=TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t
# 仅 CHAIN_WATCHER_EMERGENCY_FALLBACK=true 时给机器人本机扫链使用。
TRONGRID_API_KEY=
```

`ledger-chain-watcher` 第一版统一请求 Tronscan/TronGrid：

```env
CHAIN_WATCHER_DATABASE_URL=postgres://chainwatcher:change_this_chain_pg_password@chain-postgres:5432/ledger_chain_watcher?sslmode=disable
CHAIN_WATCHER_ADDR=:8090
CHAIN_WATCHER_BOTS=ledger-main:change_this_chain_watcher_secret
CHAIN_WATCHER_TRONSCAN_API_BASE=https://apilist.tronscanapi.com/api
CHAIN_WATCHER_TRON_API_KEY=your_real_api_key
CHAIN_WATCHER_SOURCE_POLL_SECONDS=1
CHAIN_WATCHER_GLOBAL_SCAN_PAGES=1
CHAIN_WATCHER_ADDRESS_SCAN_PAGES=3
CHAIN_WATCHER_ADDRESS_SCAN_CONCURRENCY=8
CHAIN_WATCHER_LOOKBACK_SECONDS=600
CHAIN_WATCHER_CLAIM_LEASE_SECONDS=30
```

未来接 TRON Lite FullNode + Event Plugin V2 + Kafka 时，机器人配置保持不变，只切换 watcher 的数据源。

watcher 有两种部署模式：

- Compose 版：`docker-compose.ghcr.yml` 或 `docker-compose.chain-watcher.yml`，同一个 Compose 项目中包含 `chain-postgres` 独立容器和 `ledger-chain-watcher` 独立容器。这样更利于数据卷持久化、升级、备份、健康检查和故障隔离；宝塔里仍然是一套项目，一起启动/停止。
- 宿主机版：PostgreSQL 在宝塔/宿主机上，`ledger-chain-watcher` 用 systemd 运行，模板见 `deploy/ledger-chain-watcher.env.example` 和 `deploy/ledger-chain-watcher.service`。机器人容器通过 `host.docker.internal` 或 `172.17.0.1` 访问 `8090`。

普通用户私聊点击 `🔔地址监听` 后，最多可添加 2 个监听地址；宿主、默认操作人、一级操作人和下级操作人不受数量限制。面板按钮支持添加监听地址、设置备注和最小提醒金额。最小提醒金额表示小于这个数的 USDT 交易不提醒，设置 `0` 表示不限制。

机器人保存监听地址、tx_hash 去重和 Telegram outbox；watcher 负责统一链上数据入口、统一解析、按多机器人订阅匹配和短期事件队列。机器人配置了 `CHAIN_WATCHER_URL`、`CHAIN_WATCHER_BOT_ID`、`CHAIN_WATCHER_SECRET` 后，默认停用本机地址监听轮询，改为注册订阅并每秒领取 watcher matched events。同一笔交易仍按 `owner + address + tx_hash + direction` 去重，不会重复提醒。

`CHAIN_WATCHER_EMERGENCY_FALLBACK=false` 是应急开关，默认关闭。临时设为 `true` 后，机器人会在使用 watcher 的同时启用本机按地址扫链，适合 watcher 故障排查或短时兜底；它会让每个机器人重新产生 Tronscan/TronGrid API 调用，不建议长期打开。这个兜底走机器人侧 `TRONSCAN_API_BASE` / `TRONGRID_API_KEY`，不是 watcher 服务端的 `CHAIN_WATCHER_TRON_API_KEY`。

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

服务器部署见 [docs/deployment.md](docs/deployment.md)，当前发布目标优先用 Go v2.3 镜像、PostgreSQL、共享 `ledger-chain-watcher` 和 Docker Compose。
