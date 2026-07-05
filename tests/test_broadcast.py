from __future__ import annotations

from datetime import datetime, timedelta, timezone
from pathlib import Path
from tempfile import TemporaryDirectory
import time

from ledger_bot.bot import LedgerBot, MessageContext
from ledger_bot.config import Config
from ledger_bot.storage import TelegramUser


BEIJING_TZ = timezone(timedelta(hours=8), name="Asia/Shanghai")


class FakeClient:
    def __init__(self) -> None:
        self.messages: list[dict[str, object]] = []
        self.edits: list[dict[str, object]] = []
        self.copies: list[dict[str, object]] = []
        self.answers: list[dict[str, object]] = []

    def send_message(self, chat_id: int, text: str, **kwargs) -> None:
        self.messages.append({"chat_id": chat_id, "text": text, **kwargs})

    def copy_message(self, chat_id: int, from_chat_id: int, message_id: int, **kwargs) -> dict[str, object]:
        payload = {
            "chat_id": chat_id,
            "from_chat_id": from_chat_id,
            "message_id": message_id,
            **kwargs,
        }
        self.copies.append(payload)
        return {"message_id": 900 + len(self.copies)}

    def answer_callback_query(self, callback_query_id: str, text: str | None = None) -> None:
        self.answers.append({"callback_query_id": callback_query_id, "text": text})

    def edit_message_text(self, chat_id: int, message_id: int, text: str, **kwargs) -> dict[str, object]:
        payload = {"method": "edit_text", "chat_id": chat_id, "message_id": message_id, "text": text, **kwargs}
        self.edits.append(payload)
        return payload

    def edit_message_caption(self, chat_id: int, message_id: int, caption: str, **kwargs) -> dict[str, object]:
        payload = {
            "method": "edit_caption",
            "chat_id": chat_id,
            "message_id": message_id,
            "caption": caption,
            **kwargs,
        }
        self.edits.append(payload)
        return payload

    def edit_message_media(self, chat_id: int, message_id: int, media: dict[str, object], **kwargs) -> dict[str, object]:
        payload = {"method": "edit_media", "chat_id": chat_id, "message_id": message_id, "media": media, **kwargs}
        self.edits.append(payload)
        return payload


class InlineExecutor:
    def submit(self, fn, *args, **kwargs):  # type: ignore[no-untyped-def]
        return fn(*args, **kwargs)


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
        notification_threads=1,
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
        "🔎查询UID",
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


def test_uid_lookup_formats_forward_origin_user() -> None:
    text = LedgerBot.format_uid_lookup(
        {
            "message_id": 9,
            "from": {"id": 100, "first_name": "Root"},
            "forward_origin": {
                "type": "user",
                "sender_user": {"id": 200, "first_name": "Alice", "username": "alice"},
            },
        },
        TelegramUser(100, "root", "Root"),
    )

    assert "消息发送人UID：100" in text
    assert "转发来源用户UID：200" in text
    assert "转发来源用户用户名：@alice" in text


def test_user_picker_keyboard_and_shared_uid_flow() -> None:
    bot = object.__new__(LedgerBot)
    fake = FakeClient()
    bot.client = fake
    bot.config = make_config(Path("unused.db"))
    bot.storage = object()

    bot.send_user_picker(1001, TelegramUser(10, "root", "Root"), 77)

    message = fake.messages[0]
    assert message["text"] == "点击下方「选择用户」按钮，选择后机器人会显示对方 UID。"
    keyboard = message["reply_markup"]["keyboard"]  # type: ignore[index]
    request_users = keyboard[0][0]["request_users"]
    assert request_users["user_is_bot"] is False
    assert request_users["request_name"] is True
    assert request_users["request_username"] is True

    fake.messages.clear()
    handled = bot.handle_user_shared_lookup(
        1001,
        TelegramUser(10, "root", "Root"),
        {
            "message_id": 88,
            "users_shared": {
                "users": [{"user_id": 200, "first_name": "Alice", "username": "alice"}]
            },
        },
        88,
    )

    assert handled
    assert "已选择用户" in fake.messages[0]["text"]
    assert "UID：200" in fake.messages[0]["text"]
    assert "用户名：@alice" in fake.messages[0]["text"]
    assert fake.messages[0]["reply_markup"] == {"remove_keyboard": True}


def test_private_help_uses_buttons_and_admin_for_non_accounting_features() -> None:
    help_text = LedgerBot.private_help_text(object.__new__(LedgerBot))

    assert "点击 📡群发广播 或 📣分组广播" in help_text
    assert "通知所有人是按钮开关" in help_text
    assert "后台里按群名搜索或多选群组" in help_text
    assert "点击 🔎查询UID" in help_text
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
            assert not bot.can_use_direct_broadcast_target(level1, -1001)
            assert bot.can_use_direct_broadcast_target(level1, -1002)
            assert bot.direct_broadcast_chat_ids_for_user(level1) == [-1002]
            assert not bot.can_use_broadcast_target(child, -1001)
            assert not bot.can_use_direct_broadcast_target(child, -1001)

            bot.storage.update_broadcast_operator_features(
                user_id=20,
                now=current,
                allow_group_broadcast=0,
                allow_direct_send=1,
                allow_manage_operators=0,
                receive_sent_notifications=1,
                receive_reply_notifications=1,
            )

            assert not bot.can_create_broadcast_child_operator(level1)
            assert not bot.can_use_broadcast_group(level1, "finance")
            assert bot.can_use_direct_broadcast_target(level1, -1002)
            assert bot.storage.list_broadcast_operator_ids_with_feature("sent_notifications") == {20}
            assert bot.storage.list_broadcast_operator_ids_with_feature("reply_notifications") == {20}
        finally:
            bot.storage.current().conn.close()


def test_broadcast_replacement_does_not_override_job_payload() -> None:
    with TemporaryDirectory() as tmp:
        bot = LedgerBot(make_config(Path(tmp) / "bot.db"))
        fake = FakeClient()
        bot.client = fake
        try:
            current = datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ)
            bot.storage.update_broadcast_replacement_settings(
                now=current,
                enabled=1,
                text="固定文字",
                photo="replacement-file-id",
                updated_by=10,
            )

            bot.create_broadcast_job_reply(
                1001,
                TelegramUser(10, "root", "Root"),
                "chat:-1001",
                [-1001],
                "",
                {"message_id": 55, "photo": [{"file_id": "original-file-id"}], "caption": "原图说明"},
                False,
                current,
                99,
            )

            confirmation = fake.messages[0]["text"]
            assert "类型：photo" in confirmation
            assert "[图片] 原图说明" in confirmation
            assert "覆盖" not in confirmation

            job = bot.storage.current().conn.execute("SELECT * FROM broadcast_jobs ORDER BY id DESC LIMIT 1").fetchone()
            assert job is not None
            assert job["scope"] == "chat:-1001"
            assert job["source_chat_id"] == 1001
            assert job["source_message_id"] == 55
            assert job["message_kind"] == "photo"
            assert job["photo"] is None
            assert job["text"] == "[图片] 原图说明"
        finally:
            bot.storage.current().conn.close()


def test_direct_broadcast_reply_replaces_original_photo() -> None:
    with TemporaryDirectory() as tmp:
        bot = LedgerBot(make_config(Path(tmp) / "bot.db"))
        fake = FakeClient()
        bot.client = fake
        try:
            current = datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ)
            bot.storage.update_broadcast_replacement_settings(
                now=current,
                enabled=1,
                photo="replacement-file-id",
                updated_by=10,
            )
            job = bot.storage.create_broadcast_job(
                creator_user_id=10,
                scope="chat:-1001",
                target_chat_ids=[-1001],
                text="[图片] 原图说明",
                source_chat_id=1001,
                source_message_id=55,
                message_kind="photo",
                now=current,
            )
            bot.storage.mark_broadcast_job_target(job["id"], -1001, status="sent", sent_message_id=300, now=current)
            match = bot.storage.find_broadcast_job_by_sent_message(-1001, 300)
            assert match is not None

            ctx = MessageContext(
                message={
                    "message_id": 301,
                    "reply_to_message": {
                        "message_id": 300,
                        "photo": [{"file_id": "original-file-id"}],
                        "caption": "原图说明",
                    },
                },
                chat_id=-1001,
                chat_title="A",
                user=TelegramUser(200, "bob", "Bob"),
                text="收到",
                now=current,
            )

            assert bot.replace_direct_broadcast_original_if_needed(ctx, match)
            assert fake.edits == [
                {
                    "method": "edit_media",
                    "chat_id": -1001,
                    "message_id": 300,
                    "media": {"type": "photo", "media": "replacement-file-id", "caption": "原图说明"},
                }
            ]
        finally:
            bot.storage.current().conn.close()


def test_group_broadcast_reply_does_not_replace_original() -> None:
    with TemporaryDirectory() as tmp:
        bot = LedgerBot(make_config(Path(tmp) / "bot.db"))
        fake = FakeClient()
        bot.client = fake
        try:
            current = datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ)
            bot.storage.update_broadcast_replacement_settings(
                now=current,
                enabled=1,
                text="固定文字",
                photo="replacement-file-id",
                updated_by=10,
            )
            job = bot.storage.create_broadcast_job(
                creator_user_id=10,
                scope="group:finance",
                target_chat_ids=[-1001],
                text="[图片] 原图说明",
                source_chat_id=1001,
                source_message_id=55,
                message_kind="photo",
                now=current,
            )
            bot.storage.mark_broadcast_job_target(job["id"], -1001, status="sent", sent_message_id=300, now=current)
            match = bot.storage.find_broadcast_job_by_sent_message(-1001, 300)
            assert match is not None

            ctx = MessageContext(
                message={
                    "message_id": 301,
                    "reply_to_message": {
                        "message_id": 300,
                        "photo": [{"file_id": "original-file-id"}],
                        "caption": "原图说明",
                    },
                },
                chat_id=-1001,
                chat_title="A",
                user=TelegramUser(200, "bob", "Bob"),
                text="收到",
                now=current,
            )

            assert not bot.replace_direct_broadcast_original_if_needed(ctx, match)
            assert fake.edits == []
        finally:
            bot.storage.current().conn.close()


def test_pending_broadcast_input_auto_sends_and_keeps_target() -> None:
    with TemporaryDirectory() as tmp:
        bot = LedgerBot(make_config(Path(tmp) / "bot.db"))
        fake = FakeClient()
        bot.client = fake
        bot.broadcast_executor = InlineExecutor()  # type: ignore[assignment]
        bot.notification_executor = InlineExecutor()  # type: ignore[assignment]
        try:
            user = TelegramUser(10, "root", "Root")
            current = datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ)
            bot.storage.ensure_group(-1001, "A", current)
            bot.broadcast_pending[user.user_id] = {
                "label": "单群：A",
                "scope": "chat:-1001",
                "target_ids": [-1001],
                "notify_all": False,
                "created_at": time.monotonic(),
            }

            handled = bot.handle_pending_broadcast_input(
                1001,
                user,
                {"message_id": 77, "text": "哈哈"},
                "哈哈",
                current,
                77,
            )

            assert handled
            assert user.user_id in bot.broadcast_pending
            assert any("已提交广播" in str(item["text"]) for item in fake.messages)
            assert not any("确认发送广播" in str(item["text"]) for item in fake.messages)
        finally:
            bot.storage.current().conn.close()


def test_broadcast_reply_notice_hides_job_and_has_actions() -> None:
    with TemporaryDirectory() as tmp:
        bot = LedgerBot(make_config(Path(tmp) / "bot.db"))
        fake = FakeClient()
        bot.client = fake
        bot.notification_executor = InlineExecutor()  # type: ignore[assignment]
        try:
            current = datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ)
            bot.storage.ensure_group(-1001, "11", current)
            job = bot.storage.create_broadcast_job(
                creator_user_id=10,
                scope="chat:-1001",
                target_chat_ids=[-1001],
                text="原广播",
                message_kind="text",
                now=current,
            )
            bot.storage.mark_broadcast_job_target(job["id"], -1001, status="sent", sent_message_id=300, now=current)
            ctx = MessageContext(
                message={
                    "message_id": 301,
                    "text": "嘿嘿",
                    "reply_to_message": {"message_id": 300, "text": "原广播"},
                },
                chat_id=-1001,
                chat_title="11",
                user=TelegramUser(200, "aze89", "阿泽"),
                text="嘿嘿",
                now=current,
            )

            bot.handle_broadcast_reply_notification(ctx)

            notice = fake.messages[0]
            assert "任务" not in str(notice["text"])
            assert "群：" in str(notice["text"])
            assert "人：" in str(notice["text"])
            assert "内容：" in str(notice["text"])
            keyboard = notice["reply_markup"]["inline_keyboard"]  # type: ignore[index]
            assert keyboard[0][0]["text"] == "快速回复"
            assert keyboard[1][0]["text"] == "定位回复消息"
            assert keyboard[2][0]["text"] == "定位原投递消息"
        finally:
            bot.storage.current().conn.close()


def test_broadcast_reply_quick_reply_copies_next_private_message() -> None:
    with TemporaryDirectory() as tmp:
        bot = LedgerBot(make_config(Path(tmp) / "bot.db"))
        fake = FakeClient()
        bot.client = fake
        try:
            user = TelegramUser(10, "root", "Root")
            bot.handle_broadcast_reply_start_callback(1001, user, "cb1", "reply:start:-1001:301")
            assert user.user_id in bot.broadcast_reply_pending

            handled = bot.handle_broadcast_reply_pending_input(
                1001,
                user,
                {"message_id": 88, "text": "收到"},
                "收到",
                datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ),
                88,
            )

            assert handled
            assert user.user_id not in bot.broadcast_reply_pending
            assert fake.copies[-1] == {
                "chat_id": -1001,
                "from_chat_id": 1001,
                "message_id": 88,
                "reply_to_message_id": 301,
            }
        finally:
            bot.storage.current().conn.close()
