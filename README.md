# Telegram 群组记账机器人

这是一个按群组使用的 Telegram 记账机器人原型，参考了你提供的两套记账助手教程和截图里的账单样式。

## 已支持的第一版功能

- 群内发送 `开始` 激活记账，首次激活用户成为最高权限。
- `停止` 或 `关闭` 暂停记账。
- `上课` / `下课` 调整群员发言权限，机器人需要群管理员权限。
- `添加操作员 @user`、`删除操作员 @user`，也支持回复对方消息后发送 `添加操作员`。
- `设置费率3`、`设置入款费率3`、`设置下发费率3`。
- `设置汇率7.1`、`设置入款汇率7.1`、`设置下发汇率7.1`。
- 入款：`+1000`、`+1000/7.1`、`+1000*5`、`+1000*5/7.1`、`+1000*12%`、`+1000U`。
- 下发：`下发5000`、`下发5000/7.8`、`下发5000*5/7.1`、`下发5000U`。
- 群内运算：`1000/6.8`、`1000/7.5-1000/6.8`，机器人直接回复 `原式=结果`，不写入账单。
- 私聊菜单：开始记账、详细说明、分组广播、自助续费、地址监听、群发广播、功能设置、账单统计。
- 地址监听：私聊发送 `添加监听地址 Txxxx 备注`、`删除监听地址 Txxxx`、`地址监听`。USDT 监听只支持 TRC20，收入/支出/TRX 通知可单独开关。
- TRC20 USDT 交易提醒格式已预留：收入显示出账地址和入账监听地址，支出显示出账监听地址和入账地址，交易哈希可点击跳转 Tronscan。
- `+0` 查看简洁账单，`显示账单` 查看完整今日账单。
- 加账本人回复错误的机器人记账回执，发送 `撤销` / `撤销入款` / `撤销下发` 可撤销。
- `删除账单`、`清除今日账单` 会二次确认。
- `简洁模式10` / `显示条数10`、`完整模式`。
- `设置日切6`、`设置日切-1` 或 `关闭日切`。
- `开启记账置顶` / `关闭记账置顶`。
- 每个群的 `🌏完整账单` 按钮会按 `chat_id` 生成独立链接。

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
```

如果你用自建 Telegram Bot API Server，`TELEGRAM_API_BASE` 填服务根地址即可，例如：

```env
TELEGRAM_API_BASE=http://telegram-bot-api:8081
```

不要把 `/bot<TOKEN>/sendMessage` 这类完整接口路径填进去，程序会自动拼接。

### 完整账单链接

如果你有自己的账单网页，配置：

```env
PUBLIC_BILL_BASE_URL=https://your-domain.example/day_xxb.php
PUBLIC_BILL_BOT_NAME=YOUR_BOT_CODE
```

机器人会给每个群生成类似这样的独立链接：

```text
https://your-domain.example/day_xxb.php?firstname=&chat_id=-100xxx&up_page=1&down_page=1&created_at=&begintime=2026-07-04+06%3A00%3A00&endtime=2026-07-05+06%3A00%3A00&all=&phpname=YOUR_BOT_CODE&type=bjr
```

核心隔离字段是 `chat_id`，不同群会打开不同账单。

### TRC20 链上监听

使用 TronGrid 只读接口，不需要私钥。配置：

```env
TRONGRID_API_BASE=https://api.trongrid.io
TRONGRID_API_KEY=你的TronGridKey
TRON_USDT_CONTRACT=TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t
TRON_POLL_INTERVAL_SECONDS=5
TRON_INITIAL_LOOKBACK_MINUTES=15
```

私聊添加监听地址：

```text
添加监听地址 TGhAAySHUUcEGua33pZZ88wP3bA6XSeQuZ 监控地址
```

机器人会定时请求 TronGrid 的 TRC20 交易接口，发现新的 USDT 收入/支出后私聊推送。`TRON_POLL_INTERVAL_SECONDS=5` 是接近秒级提醒；如果要更快可以调到 `2` 或 `3`，但主网强烈建议配置 `TRONGRID_API_KEY`。`TRON_INITIAL_LOOKBACK_MINUTES` 控制每次轮询回看的时间窗口，去重表会避免重复提醒。

3. 启动：

```powershell
python -m ledger_bot
```

服务器部署见 [docs/deployment.md](docs/deployment.md)，推荐优先用 Docker Compose。

如果使用预构建镜像，宝塔 Docker Compose 可以直接使用：

```yaml
services:
  ledger-bot:
    image: ghcr.io/dayou0168/telegram-ledger-bot:latest
    container_name: ledger-bot
    restart: unless-stopped
    environment:
      TELEGRAM_BOT_TOKEN: "replace-with-your-token"
      TELEGRAM_BOT_USERNAME: "replace-with-your-bot-username"
      TELEGRAM_API_BASE: "https://api.telegram.org"
      BOT_DB_PATH: "data/ledger_bot.db"
      BOT_TIMEZONE: "Asia/Shanghai"
      TRONGRID_API_BASE: "https://api.trongrid.io"
      TRONGRID_API_KEY: "replace-with-your-trongrid-key"
      TRON_USDT_CONTRACT: "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
      TRON_POLL_INTERVAL_SECONDS: "5"
      TRON_INITIAL_LOOKBACK_MINUTES: "15"
    volumes:
      - ledger_bot_data:/app/data

volumes:
  ledger_bot_data:
```

## 还没接入的外部功能

这些功能已经预留指令入口，但需要第三方 API 或后台页面再接：

- OTC/欧易/火币/越南盾/马币/金价查询。
- 手机号、银行卡、身份证、TRX 地址查询。
- 网页版完整账单。
- 群发后台。
- 地址白名单和 USDT 交易提醒。
