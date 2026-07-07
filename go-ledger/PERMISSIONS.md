# Go Ledger Bot v2.1 Permissions

本文件是 Go v2.1 权限体系的源头说明。记账核心、链上监听、广播后台、部署运维和总控发布线程都应以这里为准，不在各自模块里临时发明权限规则。

## 当前基线

- `BOT_HOST_USER_ID` 是唯一宿主，代表系统最终所有者。
- `DEFAULT_OPERATOR_USER_IDS` 是维护程序的人在部署配置中写入的默认操作人。
- 为兼容旧行为，当前代码把宿主和默认操作人统一视为全局特权用户。
- 单群记账操作员仍在 `operators` 表，只管理本群记账和本群设置。
- 广播操作员仍在 `broadcast_operators` 表，广播目标授权仍在 `broadcast_operator_permissions` 表。
- 业务模块不得直接读取 `cfg.HostUserID` 或 `cfg.DefaultOperatorIDs` 判断权限；统一调用 `internal/permissions` 暴露的 policy/helper。

## 决策

### 默认操作人不永远等同宿主

当前 v2.1 兼容阶段，默认操作人拥有全局记账、全局广播、地址监听和邀请机器人等能力。

长期规则上，默认操作人不是宿主。宿主是唯一根身份，默认操作人是宿主预配置的全局委托身份。后续一旦进入可配置权限阶段，应允许把默认操作人的能力拆成显式 scope，而不是继续把默认操作人写死为宿主等价身份。

落地要求：

- `Policy.IsHost` 只表示唯一宿主。
- `Policy.IsPrivileged` 只表示当前兼容阶段的全局特权集合。
- 新增能力时，不直接复用 `IsHost` 或散读 env；应新增清晰的能力方法，例如 `CanManageGlobalOperators`、`CanGrantBroadcastPermission`。

### 授权树按域分开存储

记账权限和广播权限继续分离。它们的目标和风险不同：记账权限绑定单群账本，广播权限绑定可投递目标群或广播分组，强行合并会让授权边界变模糊。

统一的是权限判断入口和术语，不是数据库表强合并：

- 记账域：沿用 `operators`，后续增加授权树字段。
- 广播域：沿用 `broadcast_operators` 和 `broadcast_operator_permissions`，后续增加授权树字段。
- 地址监听：当前跟随全局特权用户和广播操作员；后续应拆成单独 scope，避免“能广播”天然等于“能监听地址”。

建议的后续字段：

```sql
ALTER TABLE operators
  ADD COLUMN parent_user_id BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN level INT NOT NULL DEFAULT 1,
  ADD COLUMN can_delegate BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE broadcast_operators
  ADD COLUMN parent_user_id BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN level INT NOT NULL DEFAULT 1,
  ADD COLUMN can_delegate BOOLEAN NOT NULL DEFAULT FALSE;
```

其中 `parent_user_id` 记录直接授权人，`level=1` 表示一级操作人，`level=2` 表示下级操作人。现有数据迁移时默认视为一级操作人，且默认不可再授权，避免旧数据突然扩大权限。

### 下级默认不可再授权

授权链默认只允许两层：

- 宿主和默认操作人可以创建一级操作人。
- 一级操作人只有在 `can_delegate=true` 时，才能创建下级操作人。
- 下级操作人默认不能继续授权。

若以后确实需要多级代理，应通过 `can_delegate` 和最大深度显式打开，并且只能把自己已拥有的目标和能力子集授予下级。

任何授权操作必须满足：

- 授权人当前有效。
- 被授权 scope 属于授权人自己拥有的 scope 子集。
- 被授权目标属于授权人自己拥有的目标子集。
- 不能授权宿主身份。
- 不能通过下级权限覆盖宿主或默认操作人的全局权限。

### 后台需要 UID 角色，但不能立刻替换后台 token

当前后台 `ADMIN_WEB_TOKEN` 继续保留，作为部署级入口和紧急入口。

后续如果后台加入 Telegram UID 登录，UID 角色应来自统一 policy：

- 宿主：后台最高权限，可管理全局权限和所有业务。
- 默认操作人：兼容阶段可管理全局业务，后续按显式 scope 限制。
- 单群记账操作员：只能看和管理授权群内账本能力。
- 广播操作员：只能看和执行已授权的广播目标。

后台写操作必须按 UID 权限判断；只知道 `ADMIN_WEB_TOKEN` 不应自动绕过所有业务权限，除非明确进入“维护模式”。

## 当前代码状态

`internal/permissions.Policy` 已经承接 env 权限，并提供这些兼容阶段能力：

- `CanInviteBot`
- `HasGlobalLedgerAccess`
- `HasGlobalBroadcastAccess`
- `HasGlobalAddressWatchAccess`
- `CanManageAnyGroup`

后续模块接入时，应先在 `internal/permissions` 增加语义化方法，再由 bot、后台或 worker 调用。不要在业务代码里写 `userID == cfg.HostUserID` 或遍历 `cfg.DefaultOperatorIDs`。

## 同步给其他线程的口径

短期同步口径：

宿主和默认操作人在 Go v2.1 兼容阶段都是全局特权用户；单群记账操作员和广播操作员仍按各自表限制范围；所有新判断必须走 `internal/permissions`。

长期同步口径：

宿主是唯一根身份，默认操作人只是预配置的全局委托身份。记账和广播权限不合并表，只合并 policy/helper 入口。一级/下级授权树按域存储，下级默认不可再授权，所有授权只能授予自己已有能力和目标的子集。
