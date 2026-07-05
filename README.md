# Telegram 群组记账机器人

这是一个按群组使用的 Telegram 记账机器人原型，参考了你提供的两套记账助手教程和截图里的账单样式。

## 当前已支持功能

- 群内发送 `开始` 激活记账；`开始` 只开启记账，不会把首次发送者升为最高权限。
- `停止` 或 `关闭` 暂停记账。
- `上课` / `下课` 调整群员发言权限，机器人需要群管理员权限。
- `添加操作员 @user`、`删除操作员 @user`，也支持回复对方消息后发送 `添加操作员`。
- `设置费率3`、`设置入款费率3`、`设置下发费率3`。
- `设置汇率7.1`、`设置入款汇率7.1`、`设置下发汇率7.1`。
- 入款：`+1000`、`+1000/7.1`、`+1000*5`、`+1000*5/7.1`、`+1000*12%`、`+1000U`。
- 下发：`下发5000`、`下发5000/7.8`、`下发5000*5/7.1`、`下发5000U`。
- 群内运算：`1000/6.8`、`1000/7.5-1000/6.8`，机器人直接回复 `原式=结果`，不写入账单。
- `Z0` 查询 OKX OTC 商家所有实时汇率 TOP 10，方向为 USDT 卖 CNY；`设置汇率 Z1 -0.1` 按第 1 档下浮 0.1 设置入款汇率，`设置汇率 Z1 +0.1` 则上浮 0.1。
- 使用 `Z1 -0.1` 或 `设置汇率 Z1 -0.1` 后，账单底部显示为两行：`实时汇率：`、`支付宝1档 下浮0.10`；偏移为 `0`、`+0`、`-0` 时只显示 `支付宝1档`。
- 私聊菜单：开始记账、详细说明、分组广播、自助续费、地址监听、群发广播、功能设置、账单统计。
- 机器人只有一个宿主，宿主由服务器配置 `BOT_HOST_USER_ID` 指定；宿主是最高权限。
- 默认操作人只由维护人员修改服务器配置 `DEFAULT_OPERATOR_USER_IDS` 添加/删除。默认操作人可以邀请机器人进群、在任意群记账、使用群发广播和分组广播，但不会因为发送 `开始` 或拥有默认权限而变成本群最高权限。
- 宿主可以在群内发送 `添加操作员 @user` / `删除操作员 @user` 设置单群操作人。
- 机器人被邀请进群或群里有人发言时会按 `chat_id` 保存群组，群名变更后会自动更新；唯一宿主必须在群内，否则机器人自动退群，即使默认操作人在群内也不会保留。
- 只用于广播、不需要记账的群可以不发送 `开始`；如果已经开启过记账，发送 `停止` 或 `关闭` 即可暂停记账，群仍会保留在群发/分组广播列表中。
- 私聊 `群发广播 广播内容` 可发给全部已保存群；图片/图片+文字先发给机器人，再回复该素材发送 `群发广播`。
- 私聊 `新建分组 财务`、`分组添加 财务 -100111 -100222`、`分组移除 财务 -100111` 管理分组，`分组广播 财务 广播内容` 发送到该分组。
- 群发广播和分组广播都支持 `通知所有人`，会在目标群内 @ 所有有过发言记录的用户，例如 `群发广播 通知所有人 广播内容`。
- 地址监听：私聊发送 `地址监听` 打开按钮面板，可按钮式设置地址、删除地址、设置备注、设置最小提醒金额；文字命令也支持 `设置地址 Txxxx 备注`、`删除地址 Txxxx`、`设置备注 Txxxx 备注`、`设置监听金额10`。USDT 监听只支持 TRC20，收入/支出/TRX 通知可单独开关。
- TRC20 USDT 交易提醒：收入显示出账地址和入账监听地址，支出显示出账监听地址和入账地址；地址可点按/长按复制，交易哈希可点击跳转 Tronscan。
- 群内直接发送单个 TRC20 地址时，机器人会回复原消息并显示验证地址、该群累计验证次数、上次发送人和本次发送人。
- `查询Txxxx` 可查询 TRX 地址，返回 TRX/USDT 余额、创建时间、活跃时间、权限和最近 USDT 流水；私聊直接发送 T 地址也可以查询。
- `+0` 查看简洁账单，`显示账单` 查看完整今日账单。
- 加账本人回复自己发送的原始加账消息，发送 `撤销` / `撤销入款` / `撤销下发` 可撤销。
- `删除账单`、`清除今日账单` 会二次确认。
- `简洁模式10` / `显示条数10`、`完整模式`。
- 默认按北京时间 00:00 日切，可用 `设置日切6` 改到 6 点，`设置日切-1` 或 `关闭日切` 可关闭自动日切。
- 修改日切不会立刻拆断已经开始且已有流水的账期，而是从“当前账期结束后遇到的新日切点”开始生效：例如 7 月 3 日 12:00 从 0 点改到 4 点，当前账期延续到 7 月 4 日 04:00；从 4 点改到 0 点，当前账期延续到 7 月 5 日 00:00。如果当前账期没有任何入款/下发流水，则新日切立即生效。
- `开启记账置顶` / `关闭记账置顶`。
- 每个群的 `🌐 完整账单` 按钮会按 `chat_id` 生成独立链接。

## 运行

1. 复制配置：

```powershell
Copy-Item .env.example .env
```

2. 编辑 `.env`，填入你的 Bot Token：

```env
TELEGRAM_BOT_TOKEN=123456:replace-me
TELEGRAM_BOT_USERNAME=your_bot_username
TELEGRAM_API_BASE=https://api.telegram.org
BOT_DB_PATH=data/ledger_bot.db
BOT_TIMEZONE=Asia/Shanghai
BOT_HOST_USER_ID=123456789
DEFAULT_OPERATOR_USER_IDS=
BOT_WORKER_THREADS=8
BOT_CHAIN_THREADS=8
BOT_RATE_THREADS=1
BOT_BROADCAST_THREADS=4
BOT_QUERY_THREADS=2
BOT_HOST_CHECK_TTL_SECONDS=300
```

先私聊机器人发送 `我的ID` 获取你的 Telegram ID，再填入 `BOT_HOST_USER_ID`。机器人只允许配置一个宿主。宿主必须在机器人所在群内，否则机器人会自动退群。默认操作人可在 `.env` 的 `DEFAULT_OPERATOR_USER_IDS` 里用英文逗号分隔，只有维护程序的人员能通过改服务器配置添加或删除。

并发池说明：`BOT_WORKER_THREADS` 处理 Telegram 实时消息，同一群会按 FIFO 队列串行、不同群可并发；`BOT_CHAIN_THREADS` 处理 USDT/TRX 链上监听并并行扫描监听地址；`BOT_RATE_THREADS` 处理 Z0 和实时汇率刷新；`BOT_BROADCAST_THREADS` 处理群发/分组广播，默认最多同时跑 4 个广播任务，单个广播任务内部仍按目标群逐个发送，避免触发 Telegram 限流；`BOT_QUERY_THREADS` 处理 TRX 地址查询等外部查询。广播、链上监听、汇率刷新、查询互相隔离，避免某一类慢任务拖住其他版块。`BOT_HOST_CHECK_TTL_SECONDS` 控制每个群宿主在群检测的缓存时间，减少每条消息都调用 Telegram 管理接口。

如果你用自建 Telegram Bot API Server，`TELEGRAM_API_BASE` 填服务根地址即可，例如：

```env
TELEGRAM_API_BASE=http://telegram-bot-api:8081
```

不要把 `/bot<TOKEN>/sendMessage` 这类完整接口路径填进去，程序会自动拼接。

### 完整账单网页

镜像内置账单网页服务，默认监听容器 `8080` 端口。配置自己的域名后：

```env
PUBLIC_BILL_BASE_URL=https://bot.your-domain.example
PUBLIC_BILL_URL_TEMPLATE=
BILL_WEB_ENABLED=1
BILL_WEB_HOST=0.0.0.0
BILL_WEB_PORT=8080
BILL_WEB_TOKEN=change-this-random-token
```

机器人会给每个群生成类似这样的独立链接：

```text
https://bot.your-domain.example/bill/-100xxx/2026-07-05?begintime=2026-07-05+00%3A00%3A00&endtime=2026-07-06+00%3A00%3A00&token=change-this-random-token
```

宝塔里给域名申请 SSL 后，把站点反向代理到 `http://127.0.0.1:8080`。`BILL_WEB_TOKEN` 可留空，留空时网页是公开链接；建议正式使用时设置一串随机字符。

如果你已经有自己的 PHP 账单系统，也可以继续用 `.php` 地址：

```env
PUBLIC_BILL_BASE_URL=https://your-domain.example/day_xxb.php
PUBLIC_BILL_BOT_NAME=YOUR_BOT_CODE
```

如果你的网页参数和示例机器人完全一致，也可以直接配置完整模板：

```env
PUBLIC_BILL_URL_TEMPLATE=https://your-domain.example/day_xxb.php?firstname=&chat_id={chat_id}&up_page=1&down_page=1&created_at=&begintime={begin_time}&endtime={end_time}&all={all}&phpname={bot_name}&type=bjr
```

按钮逻辑：配置了 `PUBLIC_BILL_URL_TEMPLATE` 或 `PUBLIC_BILL_BASE_URL` 时，底部「🌐 完整账单」会打开网页；不配置时，按钮会在 Telegram 内显示完整账单。

### TRC20 链上监听

使用 TronGrid 只读接口，不需要私钥。配置：

```env
TRONGRID_API_BASE=https://api.trongrid.io
TRONGRID_API_KEY=
TRON_USDT_CONTRACT=TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t
TRON_POLL_INTERVAL_SECONDS=5
TRON_INITIAL_LOOKBACK_MINUTES=15
```

私聊添加监听地址：

```text
设置地址 TGhAAySHUUcEGua33pZZ88wP3bA6XSeQuZ 监控地址
设置备注 TGhAAySHUUcEGua33pZZ88wP3bA6XSeQuZ 监控地址
设置监听金额10
```

机器人会定时请求 TronGrid 的 TRC20 交易接口，发现新的 USDT 收入/支出后私聊推送。`设置监听金额10` 表示小于 10 USDT 的交易不提醒，设置 `0` 表示不限制。`TRON_POLL_INTERVAL_SECONDS=5` 是接近秒级提醒；如果要更快可以调到 `2` 或 `3`，但主网强烈建议配置真实的 `TRONGRID_API_KEY`。没有 key 时保持空值，不要填中文占位符或 key 名称。`TRON_INITIAL_LOOKBACK_MINUTES` 控制每次轮询回看的时间窗口，去重表会避免重复提醒。

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
python -m ledger_bot
```

服务器部署见 [docs/deployment.md](docs/deployment.md)，推荐优先用 Docker Compose。

如果使用预构建镜像，宝塔 Docker Compose 可以直接使用仓库里的 [docker-compose.ghcr.yml](docker-compose.ghcr.yml)。这个文件已经把每个配置项都用中文批注出来，并把必填内容的示例放进去了。

## 还没接入的外部功能

这些功能已经预留指令入口，但需要第三方 API 或后台页面再接：

- 火币/越南盾/马币/金价查询。
- 手机号、银行卡、身份证、TRX 地址查询。
- 地址白名单和更完整的后台管理页。
