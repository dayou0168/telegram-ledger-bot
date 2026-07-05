from __future__ import annotations

from datetime import timedelta, timezone
from pathlib import Path
from tempfile import TemporaryDirectory

from ledger_bot.bot import LedgerBot
from ledger_bot.config import Config
from ledger_bot.storage import TelegramUser


BEIJING_TZ = timezone(timedelta(hours=8), name="Asia/Shanghai")


class FakeClient:
    def __init__(self) -> None:
        self.messages: list[dict[str, object]] = []

    def send_message(self, chat_id: int, text: str, **kwargs) -> None:
        self.messages.append({"chat_id": chat_id, "text": text, **kwargs})


def make_config(
    db_path: Path,
    *,
    public_bill_base_url: str | None = None,
    admin_web_token: str | None = None,
) -> Config:
    return Config(
        bot_token="123:test",
        telegram_api_base="https://api.telegram.org",
        db_path=db_path,
        timezone_name="Asia/Shanghai",
        bot_host_user_id=10,
        default_operator_user_ids=frozenset({11}),
        public_bill_base_url=public_bill_base_url,
        public_bill_url_template=None,
        public_bill_bot_name="TEST_BOT",
        bill_web_enabled=False,
        bill_web_host="127.0.0.1",
        bill_web_port=8080,
        bill_web_token=None,
        admin_web_token=admin_web_token,
        telegram_bot_username=None,
        trongrid_api_base="https://api.trongrid.io",
        trongrid_api_key=None,
        tron_usdt_contract="TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
        tron_poll_interval_seconds=0,
        tron_initial_lookback_minutes=15,
        p2p_rate_api_base="https://example.invalid",
        p2p_rate_front_api="",
        p2p_rate_market="okx",
        p2p_rate_fiat_unit="CNY",
        p2p_rate_asset="USDT",
        p2p_rate_trade_methods=("aliPay",),
        p2p_rate_refresh_seconds=60,
        p2p_rate_cache_ttl_seconds=180,
        worker_threads=1,
        chain_threads=1,
        rate_threads=1,
        broadcast_threads=1,
        query_threads=1,
        host_check_ttl_seconds=300,
    )


def test_extract_notify_all_option() -> None:
    assert LedgerBot.extract_notify_all_option("通知所有人 广播内容") == (True, "广播内容")
    assert LedgerBot.extract_notify_all_option("广播内容 通知所有人") == (True, "广播内容")
    assert LedgerBot.extract_notify_all_option("广播内容") == (False, "广播内容")


def test_broadcast_preview_for_photo_caption() -> None:
    message = {"message_id": 10, "photo": [{"file_id": "abc"}], "caption": "活动通知"}

    assert LedgerBot.broadcast_message_kind(message) == "photo"
    assert LedgerBot.broadcast_preview(message) == "[图片] 活动通知"
    assert LedgerBot.is_broadcastable_message(message)


def test_private_menu_only_shows_available_features() -> None:
    bot = object.__new__(LedgerBot)
    fake = FakeClient()
    bot.client = fake

    bot.send_private_menu(1001)

    keyboard = fake.messages[0]["reply_markup"]["keyboard"]  # type: ignore[index]
    labels = [button["text"] for row in keyboard for button in row]
    assert labels == [
        "✍开始记账",
        "📃详细说明",
        "📡群发广播",
        "📣分组广播",
        "🔔地址监听",
        "🗂群列表",
        "👥广播权限",
        "🔁广播替换",
        "⚙后台管理",
    ]
    assert "💵自助续费" not in labels
    assert "🛠功能设置" not in labels
    assert "📒账单统计" not in labels


def test_private_admin_button_sends_admin_link_to_root() -> None:
    bot = object.__new__(LedgerBot)
    fake = FakeClient()
    bot.client = fake
    bot.config = make_config(
        Path("unused.db"),
        public_bill_base_url="https://bot.example.com",
        admin_web_token="secret token",
    )

    bot.send_admin_web_link(1001, TelegramUser(10, "root", "Root"), 77)

    message = fake.messages[0]
    assert message["text"] == "后台管理入口：打开后请输入后台管理密码。"
    keyboard = message["reply_markup"]["inline_keyboard"]  # type: ignore[index]
    assert keyboard[0][0]["url"] == "https://bot.example.com/admin"


def test_private_admin_button_rejects_non_root_user() -> None:
    bot = object.__new__(LedgerBot)
    fake = FakeClient()
    bot.client = fake
    bot.config = make_config(
        Path("unused.db"),
        public_bill_base_url="https://bot.example.com",
        admin_web_token="secret",
    )

    bot.send_admin_web_link(1001, TelegramUser(99, "user", "User"), 77)

    assert fake.messages[0]["text"] == "没有后台管理权限。"


def test_private_help_uses_buttons_and_admin_for_non_accounting_features() -> None:
    help_text = LedgerBot.private_help_text(object.__new__(LedgerBot))

    assert "点击 📡群发广播 或 📣分组广播" in help_text
    assert "通知所有人是按钮开关" in help_text
    assert "后台里按群名搜索或多选群组" in help_text
    assert "输入 ADMIN_WEB_TOKEN 设置的后台密码" in help_text
    assert "单群广播 -100111 广播内容" not in help_text
    assert "授权单群 123456 -100111" not in help_text
    assert "开启广播替换" not in help_text
    assert "添加白名单地址 Txxxx 备注" not in help_text
    assert "自助续费" not in help_text
    assert "已预留" not in help_text


def test_update_lock_key_serializes_same_chat_only() -> None:
    first = {"update_id": 1, "message": {"chat": {"id": -1001}}}
    second = {"update_id": 2, "message": {"chat": {"id": -1001}}}
    other = {"update_id": 3, "message": {"chat": {"id": -1002}}}
    callback = {"update_id": 4, "callback_query": {"message": {"chat": {"id": -1001}}}}

    assert LedgerBot.update_lock_key(first) == LedgerBot.update_lock_key(second)
    assert LedgerBot.update_lock_key(first) == LedgerBot.update_lock_key(callback)
    assert LedgerBot.update_lock_key(first) != LedgerBot.update_lock_key(other)


def test_broadcast_permissions_support_group_and_single_chat_acl() -> None:
    with TemporaryDirectory() as tmp:
        bot = LedgerBot(make_config(Path(tmp) / "bot.db"))
        try:
            from datetime import datetime

            current = datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ)
            bot.storage.ensure_group(-1001, "A", current)
            bot.storage.ensure_group(-1002, "B", current)
            bot.storage.create_named_broadcast_group("finance", created_by=10, now=current)
            bot.storage.add_broadcast_group_members("finance", [-1001], now=current)

            root = TelegramUser(10, "root", "Root")
            level1 = TelegramUser(20, "level1", "Level1")
            child = TelegramUser(30, "child", "Child")

            bot.storage.add_broadcast_operator(user_id=20, created_by=10, now=current)
            bot.storage.add_broadcast_operator(user_id=30, created_by=20, now=current)
            bot.storage.grant_broadcast_group_permission("finance", user_id=20, created_by=10, now=current)
            bot.storage.grant_broadcast_chat_permission(chat_id=-1002, user_id=20, created_by=10, now=current)

            assert bot.can_create_broadcast_child_operator(root)
            assert bot.can_create_broadcast_child_operator(level1)
            assert not bot.can_create_broadcast_child_operator(child)
            assert bot.can_manage_broadcast_operator(level1, 30)
            assert bot.can_use_broadcast_group(level1, "finance")
            assert bot.can_use_broadcast_target(level1, -1001)
            assert bot.can_use_broadcast_target(level1, -1002)
            assert not bot.can_use_broadcast_target(child, -1001)
        finally:
            bot.storage.current().conn.close()
