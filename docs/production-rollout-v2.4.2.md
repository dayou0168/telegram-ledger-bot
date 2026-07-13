# v2.4.2 生产升级、回滚与验收

本文只描述尚未发布的 v2.4.2 候选版本。当前正式生产仍使用 bot v2.4.1 和上一版宿主机 watcher；取得正式 Release、镜像 digest、宿主机二进制 SHA256 和部署授权前，不执行升级。

唯一代码基线是 `codex/v2.4.2-integration` 的已确认提交。外层旧工作区中的部署文件、旧权限文案和未跟踪 TRON 安装脚本不得直接合并或投产。

## 变量名基线

v2.4.2 二进制实际读取以下变量：

```env
CHAIN_WATCHER_MAIN_SCAN_TIMEOUT_MS=3000
CHAIN_WATCHER_MAIN_MAX_INFLIGHT_ROUNDS=3
CHAIN_WATCHER_ADMIN_TOKEN=换成独立随机管理密钥
```

`CHAIN_WATCHER_MAIN_SCAN_MAX_INFLIGHT` 不是当前二进制支持的变量，不要配置。所有 Compose 与 env 模板必须使用 `CHAIN_WATCHER_MAIN_MAX_INFLIGHT_ROUNDS`。

正常模式下 bot 只消费 watcher 事件。watcher 持续异常后，多个 bot 通过 watcher PostgreSQL lease 只选出一个共享无 Key leader fallback；这不是每个 bot 独立运行的 emergency scanner。

## 使用升级脚本

[production-rollout.sh](../deploy/production-rollout.sh) 默认执行只读 `preflight`。脚本不保存生产 IP、密码、Token 或完整 DSN；连接信息来自现有容器/env，或者由维护人员在当前 shell 环境中临时提供。

```bash
sudo bash deploy/production-rollout.sh
```

真正升级必须显式提供 `--apply`，并提前准备已经固定正式镜像标签或 digest 的新 Compose：

```bash
export BOT_CONTAINER='实际bot容器名'
export BOT_SERVICE='Compose中的bot服务名'
export NEW_WATCHER_BINARY=/root/release/ledger-chain-watcher
export NEW_WATCHER_SHA256='发布页给出的完整SHA256'
export NEW_BOT_IMAGE='ghcr.io/dayou0168/telegram-ledger-bot-go:正式版本或digest'
export NEW_BOT_COMPOSE_FILE=/root/release/docker-compose.yaml
sudo -E bash deploy/production-rollout.sh upgrade --apply
```

脚本顺序为：只读预检、备份并校验两个数据库及运行配置、校验 watcher SHA256、先升级 watcher 并等待 migration/health/ready/status、再 pull/recreate bot。任何一半失败都会尝试恢复旧二进制、旧 Compose、旧 env 和旧 unit。

PostgreSQL migration 只前向执行。恢复旧镜像或旧二进制不会自动降 schema；旧程序若不兼容新 schema，应重新启用新程序。只有在明确接受升级后新增数据丢失、并已停止所有写入时，才考虑用升级前两个数据库 dump 做整库恢复。

显式运行时回滚：

```bash
export ROLLBACK_DIR=/root/ledger-upgrade-backups/v2.4.2-YYYYmmdd-HHMMSS
sudo -E bash deploy/production-rollout.sh rollback --apply
```

## 备份与恢复演练

升级备份目录权限必须是 0700，并至少包含：bot custom dump、watcher custom dump、Compose、watcher env、systemd unit、旧二进制、容器 inspect 和 `SHA256SUMS`。`pg_restore -l` 和文件非空检查通过后才能开始升级。

日常本机备份不等于异地备份。[offsite-backup.sh](../deploy/offsite-backup.sh) 默认只验证本地 `.sql.gz`；显式 `push --apply` 才会通过已配置 SSH key/known_hosts 的 rsync 复制到私网备份机，且不会自动删除远端历史：

```bash
sudo bash deploy/offsite-backup.sh audit
export SSH_KEY_FILE=/root/.ssh/ledger-backup
export OFFSITE_TARGET='backup-user@private-backup-host:/srv/ledger-postgres'
sudo -E bash deploy/offsite-backup.sh push --apply
```

建议本机至少保留 7 天，异地保留 30 天，并为上传失败配置独立监控。每季度执行一次恢复演练：

1. 在隔离 PostgreSQL 实例创建临时 bot 库和 watcher 库。
2. 对异地下载文件执行 `gzip -t` 或 `pg_restore -l`。
3. 将两个 dump 分别恢复到临时库，禁止覆盖生产数据库。
4. 检查 `schema_migrations`、群组/账单/outbox 数量，以及 watcher key registry、subscriptions、events、gap/metric 表。
5. 用临时只读配置启动对应版本程序，检查 migration 和健康接口后销毁临时库。

## 验收门槛

```bash
export EXPECTED_BOT_REPO_DIGEST='ghcr.io/dayou0168/telegram-ledger-bot-go@sha256:发布页digest'
export EXPECTED_WATCHER_SHA256='发布页给出的完整SHA256'
sudo bash deploy/production-rollout.sh acceptance
```

验收默认持续观察 10 分钟，可通过 `OBSERVE_SECONDS=900` 延长到 15 分钟。必须记录：

- `/healthz` 和 `/readyz` 持续成功；无 Token 的 `/status` 返回 401，带 Bearer Token 返回 200。
- watcher systemd `MainPID` 稳定、`NRestarts` 不增长，实际二进制 SHA256 与 Release 一致。
- bot 容器实际 image ID/digest 正确、running、restart count 不增长，Docker 日志仍为 `20m × 5`。
- bot DB 出现 v2.4.2 migrations 和 `groups.active_period_started_at`；watcher DB 出现 migration `2.4.2`、`chain_watcher_gap_tasks`、`chain_watcher_metric_minutes`。
- 10–15 分钟内无 migration、panic、decrypt、401/403、持续 429、持续 deadline、Telegram polling 409。
- 由用户实际执行一笔 USDT 收入和一笔支出，确认秒级提醒、方向/金额/tx hash 正确且无重复；这项不能用健康检查代替。

## PostgreSQL 网络收窄

不要直接把现有 `172.16.0.0/12` HBA 规则替换成猜测网段。变更前先执行只读预检：

```bash
docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{.Gateway}}{{end}}' BOT_CONTAINER
docker network inspect BOT_NETWORK
ss -lntp | grep ':5432'
sudo -u postgres psql -Atqc 'show listen_addresses; show hba_file;'
```

记录每个生产 bot Docker subnet、宝塔管理来源和 watcher 的连接方式后，先备份 `postgresql.conf`、`pg_hba.conf`、UFW 状态，再把 HBA 收窄到实际网段。使用 `pg_reload_conf()` 后立即验证 bot DB、watcher DB 和宝塔管理连接；验证失败时恢复备份并 reload。只有确认不再需要其他接口后才收窄 `listen_addresses`。文档中的这些步骤是待实施方案，不代表生产已经完成纵深收紧。

## 自建 TRON 事件服务器网络清单

本轮不合并外层旧 `install-tron-event-server.sh`。未来接入 Lite FullNode + Event Plugin V2 + Kafka 时必须满足：

- 事件服务器与 watcher 通过私网、WireGuard/Tailscale 等 VPN 互通，使用固定私网源地址。
- Kafka 9092 禁止公网开放；安全组和主机防火墙只允许 watcher 固定私网地址。
- Kafka 启用 SASL/SCRAM 或 mTLS，证书、账号和 watcher secret 分开管理。
- topic retention 初始设为 10 分钟，同时设置大小上限；它不是交易历史或长期重放备份。
- watcher 使用唯一稳定 consumer group，明确 offset reset、重平衡和 10 分钟中断后允许丢弃的策略。
- 验收覆盖 FullNode -> Event Plugin -> Kafka -> watcher -> watcher DB -> bot outbox -> Telegram 的端到端延迟、去重和重启恢复。

## 日志保留

Compose 模板继续使用 Docker `json-file` 的 `max-size=20m`、`max-file=5`，单容器约 100MB 上限；这只能近似保留近期日志，不能承诺严格 72 小时。现有生产 bot 已在上次 recreate 后应用该 LogConfig。宿主机 watcher 继续使用 journald `MaxRetentionSec=3day` 和总量上限，新部署仍需按部署文档显式安装 journald drop-in。
