from __future__ import annotations

from datetime import datetime, timedelta, timezone
from io import BytesIO
from pathlib import Path
from tempfile import TemporaryDirectory
from types import SimpleNamespace
from urllib.parse import parse_qs, urlparse
from zipfile import ZipFile

from ledger_bot.bot import LedgerBot
from ledger_bot.bill_web import (
    day_key_from_legacy_query,
    handle_bill_web_post_response,
    handle_bill_web_request,
    handle_bill_web_response,
    render_bill_page,
)
from ledger_bot.storage import Storage


BEIJING_TZ = timezone(timedelta(hours=8), name="Asia/Shanghai")


def make_bill_record(chat_id: int, day_key: str, *, created_at: datetime | None = None) -> dict[str, object]:
    return {
        "chat_id": chat_id,
        "kind": "deposit",
        "amount": "100",
        "currency": "CNY",
        "exchange_rate": "6.63",
        "fee_rate": "3",
        "amount_cny": "100",
        "amount_usdt": "15.082956",
        "commission_cny": "3",
        "net_usdt": "14.630468",
        "actor_user_id": 1,
        "actor_name": "operator",
        "subject_user_id": None,
        "subject_name": "客户A",
        "note": "测试备注",
        "is_balance": 0,
        "source_message_id": None,
        "day_key": day_key,
        "created_at": (created_at or datetime(2026, 7, 5, 12, tzinfo=BEIJING_TZ)).isoformat(),
    }


def test_render_bill_page_shows_records_and_realtime_rate() -> None:
    with TemporaryDirectory() as tmp:
        storage = Storage(Path(tmp) / "bot.db")
        try:
            now = datetime(2026, 7, 5, 12, tzinfo=BEIJING_TZ)
            storage.ensure_group(-1001, "测试群", now)
            storage.update_group(
                -1001,
                now,
                deposit_exchange_rate="6.63",
                deposit_fee_rate="3",
                realtime_rate=1,
                realtime_rate_rank=1,
                realtime_rate_offset="-0.1",
            )
            storage.insert_record(make_bill_record(-1001, "2026-07-05"))

            html = render_bill_page(
                storage,
                -1001,
                "2026-07-05",
                timezone=BEIJING_TZ,
                trade_methods=("aliPay",),
            )

            assert "测试群" in html
            assert "支付宝1档 下浮0.10" in html
            assert "入款" in html
            assert "历史账单" in html
            assert "history-dropdown" in html
            assert "07-05" in html
            assert "target.showPicker()" in html
            assert "max-width: 1180px" in html
            assert "table-layout: fixed" in html
            assert "text-align: center" in html
            assert "vertical-align: middle" in html
            assert "border: 1px solid var(--line-soft)" in html
            assert "border-left: 1px solid var(--line)" in html
            assert "border-left: 4px solid var(--blue)" not in html
            assert "下载账单" in html
            assert "created_at=2026-07-05" in html
            assert "download=excel" in html
            assert "统计（按标记人）" in html
            assert "统计（按汇率分类）" in html
            assert "客户A" in html
            assert "测试备注" in html
        finally:
            storage.conn.close()


def test_render_bill_page_uses_group_cutoff_window() -> None:
    with TemporaryDirectory() as tmp:
        storage = Storage(Path(tmp) / "bot.db")
        try:
            now = datetime(2026, 7, 5, 12, tzinfo=BEIJING_TZ)
            storage.ensure_group(-1001, "测试群", now)
            storage.update_group(-1001, now, day_cutoff_hour=4)
            storage.insert_record(
                make_bill_record(
                    -1001,
                    "2026-07-04",
                    created_at=datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ),
                )
            )

            html = render_bill_page(
                storage,
                -1001,
                "2026-07-04",
                timezone=BEIJING_TZ,
            )

            assert 'type="datetime-local" step="1" name="begintime" value="2026-07-04T04:00:00"' in html
            assert 'type="datetime-local" step="1" name="endtime" value="2026-07-05T04:00:00"' in html
            assert "客户A" in html
        finally:
            storage.conn.close()


def test_render_bill_page_prefers_linked_bill_window() -> None:
    with TemporaryDirectory() as tmp:
        storage = Storage(Path(tmp) / "bot.db")
        try:
            now = datetime(2026, 7, 5, 12, tzinfo=BEIJING_TZ)
            storage.ensure_group(-1001, "测试群", now)
            storage.update_group(-1001, now, day_cutoff_hour=4)
            storage.insert_record(
                make_bill_record(
                    -1001,
                    "2026-07-03",
                    created_at=datetime(2026, 7, 3, 12, tzinfo=BEIJING_TZ),
                )
            )

            html = render_bill_page(
                storage,
                -1001,
                "2026-07-03",
                timezone=BEIJING_TZ,
                begin_time="2026-07-03 00:00:00",
                end_time="2026-07-04 00:00:00",
            )

            assert 'value="2026-07-03T00:00:00"' in html
            assert 'value="2026-07-04T00:00:00"' in html
            assert 'value="2026-07-03T04:00:00"' not in html
            assert "客户A" in html
        finally:
            storage.conn.close()


def test_render_bill_page_filters_legacy_search() -> None:
    with TemporaryDirectory() as tmp:
        storage = Storage(Path(tmp) / "bot.db")
        try:
            now = datetime(2026, 7, 5, 12, tzinfo=BEIJING_TZ)
            storage.ensure_group(-1001, "测试群", now)
            storage.insert_record(make_bill_record(-1001, "2026-07-05"))
            other = make_bill_record(-1001, "2026-07-05")
            other["subject_name"] = "客户B"
            other["note"] = "其他备注"
            storage.insert_record(other)

            html = render_bill_page(
                storage,
                -1001,
                "2026-07-05",
                timezone=BEIJING_TZ,
                search_text="测试",
                search_type="bz",
            )

            assert "客户A" in html
            assert "客户B" not in html
            assert 'value="测试"' in html
            assert '<option value="bz" selected="selected">按备注</option>' in html
        finally:
            storage.conn.close()


def test_bill_web_token_blocks_missing_token() -> None:
    config = SimpleNamespace(
        bill_web_token="secret",
        db_path=Path("missing.db"),
        timezone=BEIJING_TZ,
        p2p_rate_trade_methods=("aliPay",),
    )

    status, body = handle_bill_web_request(config, "/bill/-1001/2026-07-05")

    assert status == 403
    assert "访问受限" in body


def test_admin_page_requires_admin_token() -> None:
    config = SimpleNamespace(
        admin_web_token="admin-secret",
        bill_web_token=None,
        db_path=Path("missing.db"),
        timezone=BEIJING_TZ,
        p2p_rate_trade_methods=("aliPay",),
    )

    response = handle_bill_web_response(config, "/admin")

    assert response.status == 403
    assert "访问受限" in response.body.decode("utf-8")


def test_admin_page_renders_management_sections() -> None:
    with TemporaryDirectory() as tmp:
        storage = Storage(Path(tmp) / "bot.db")
        try:
            now = datetime(2026, 7, 5, 12, tzinfo=BEIJING_TZ)
            storage.ensure_group(-1001, "测试群", now)
        finally:
            storage.conn.close()

        config = SimpleNamespace(
            admin_web_token="admin-secret",
            bill_web_token="bill-secret",
            db_path=Path(tmp) / "bot.db",
            timezone=BEIJING_TZ,
            p2p_rate_trade_methods=("aliPay",),
        )

        response = handle_bill_web_response(config, "/admin?admin_token=admin-secret")
        body = response.body.decode("utf-8")

        assert response.status == 200
        assert "后台管理" in body
        assert "地址白名单" in body
        assert "广播权限" in body
        assert "广播替换" in body
        assert "已保存群组" in body
        assert "测试群" in body


def test_admin_post_updates_whitelist_broadcast_permissions_and_replacement() -> None:
    with TemporaryDirectory() as tmp:
        db_path = Path(tmp) / "bot.db"
        storage = Storage(db_path)
        try:
            now = datetime(2026, 7, 5, 12, tzinfo=BEIJING_TZ)
            storage.ensure_group(-100111, "测试群", now)
        finally:
            storage.conn.close()

        config = SimpleNamespace(
            admin_web_token="admin-secret",
            bill_web_token=None,
            db_path=db_path,
            timezone=BEIJING_TZ,
            p2p_rate_trade_methods=("aliPay",),
        )

        posts = [
            "action=add_broadcast_operator&user_id=2001&remark=level1",
            "action=create_broadcast_group&group_name=finance",
            "action=add_broadcast_members&group_name=finance&chat_ids=-100111",
            "action=grant_broadcast_permission&user_id=2001&group_name=finance",
            "action=grant_broadcast_chat_permission&user_id=2001&chat_id=-100111",
            (
                "action=add_whitelist&chat_id=-100111"
                "&address=TGhAAySHUUcEGua33pZZ88wP3bA6X5eQuZ"
                "&label=monitor"
            ),
            "action=set_broadcast_replacement&enabled=1&text=hello&photo=file123",
        ]
        for raw_body in posts:
            response = handle_bill_web_post_response(config, "/admin?admin_token=admin-secret", raw_body)
            assert response.status == 303

        storage = Storage(db_path)
        try:
            assert storage.get_broadcast_operator(2001)["remark"] == "level1"
            assert storage.user_can_access_broadcast_group(2001, "finance")
            assert storage.user_can_access_broadcast_chat(2001, -100111)
            assert storage.target_chat_ids_for_broadcast_group("finance") == [-100111]
            whitelist = storage.get_address_whitelist(-100111, "TGhAAySHUUcEGua33pZZ88wP3bA6X5eQuZ")
            assert whitelist is not None
            assert whitelist["label"] == "monitor"
            replacement = storage.get_broadcast_replacement_settings()
            assert replacement["enabled"] == 1
            assert replacement["text"] == "hello"
            assert replacement["photo"] == "file123"
        finally:
            storage.conn.close()


def test_legacy_created_at_uses_day_key_records_without_window() -> None:
    with TemporaryDirectory() as tmp:
        storage = Storage(Path(tmp) / "bot.db")
        try:
            now = datetime(2026, 7, 5, 12, tzinfo=BEIJING_TZ)
            storage.ensure_group(-1001, "测试群", now)
            storage.update_group(-1001, now, day_cutoff_hour=4)
            storage.insert_record(
                make_bill_record(
                    -1001,
                    "2026-07-04",
                    created_at=datetime(2026, 7, 4, 1, tzinfo=BEIJING_TZ),
                )
            )

            config = SimpleNamespace(
                bill_web_token=None,
                db_path=Path(tmp) / "bot.db",
                timezone=BEIJING_TZ,
                p2p_rate_trade_methods=("aliPay",),
            )
            status, body = handle_bill_web_request(config, "/day_xxb.php?chat_id=-1001&created_at=2026-07-04")

            assert status == 200
            assert "客户A" in body
            assert 'name="created_at" value="2026-07-04"' in body
            assert 'name="begintime"' not in body
        finally:
            storage.conn.close()


def test_bill_web_downloads_xlsx() -> None:
    with TemporaryDirectory() as tmp:
        storage = Storage(Path(tmp) / "bot.db")
        try:
            now = datetime(2026, 7, 5, 12, tzinfo=BEIJING_TZ)
            storage.ensure_group(-1001, "测试群", now)
            storage.insert_record(make_bill_record(-1001, "2026-07-05"))
        finally:
            storage.conn.close()

        config = SimpleNamespace(
            bill_web_token=None,
            db_path=Path(tmp) / "bot.db",
            timezone=BEIJING_TZ,
            p2p_rate_trade_methods=("aliPay",),
        )
        response = handle_bill_web_response(config, "/day_xxb.php?chat_id=-1001&created_at=2026-07-05&download=excel")

        assert response.status == 200
        assert response.content_type == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
        assert "filename*=UTF-8''" in response.headers["Content-Disposition"]
        assert "%E8%B4%A6%E5%8D%95_2026-07-05_" in response.headers["Content-Disposition"]
        with ZipFile(BytesIO(response.body)) as zf:
            assert "xl/worksheets/sheet1.xml" in zf.namelist()
            sheet = zf.read("xl/worksheets/sheet1.xml").decode("utf-8")
        assert "入款：1笔" in sheet
        assert "测试群" in sheet
        assert "客户A" in sheet


def test_day_key_from_legacy_query() -> None:
    assert day_key_from_legacy_query({"begintime": ["2026-07-05 00:00:00"]}) == "2026-07-05"
    assert day_key_from_legacy_query({"created_at": ["2026-07-04"]}) == "2026-07-04"
    assert day_key_from_legacy_query({"all": ["1"]}) == "active"
    assert day_key_from_legacy_query({}) == "today"


def test_builtin_bill_url_freezes_business_window() -> None:
    bot = object.__new__(LedgerBot)
    bot.storage = SimpleNamespace(get_group=lambda chat_id: {"day_cutoff_hour": 0})
    bot.config = SimpleNamespace(
        public_bill_url_template=None,
        public_bill_base_url="https://bot.example",
        public_bill_bot_name="LEDGER_BOT",
        bill_web_token="secret",
        timezone=BEIJING_TZ,
    )

    url = bot.build_bill_url(-1001, "2026-07-03")
    parsed = urlparse(url)
    query = parse_qs(parsed.query)

    assert parsed.path == "/bill/-1001/2026-07-03"
    assert query["begintime"] == ["2026-07-03 00:00:00"]
    assert query["endtime"] == ["2026-07-04 00:00:00"]
    assert query["token"] == ["secret"]


def test_builtin_bill_url_freezes_four_oclock_window_before_cutoff_change() -> None:
    bot = object.__new__(LedgerBot)
    bot.storage = SimpleNamespace(get_group=lambda chat_id: {"day_cutoff_hour": 4})
    bot.config = SimpleNamespace(
        public_bill_url_template=None,
        public_bill_base_url="https://bot.example",
        public_bill_bot_name="LEDGER_BOT",
        bill_web_token=None,
        timezone=BEIJING_TZ,
    )

    url = bot.build_bill_url(-1001, "2026-07-03")
    query = parse_qs(urlparse(url).query)

    assert query["begintime"] == ["2026-07-03 04:00:00"]
    assert query["endtime"] == ["2026-07-04 04:00:00"]


def test_builtin_bill_url_uses_extended_open_bill_window() -> None:
    bot = object.__new__(LedgerBot)
    bot.storage = SimpleNamespace(
        get_group=lambda chat_id: {
            "day_cutoff_hour": 4,
            "open_bill_day_key": "2026-07-03",
            "open_bill_begin_at": "2026-07-03T04:00:00+08:00",
            "open_bill_end_at": "2026-07-05T00:00:00+08:00",
        }
    )
    bot.config = SimpleNamespace(
        public_bill_url_template=None,
        public_bill_base_url="https://bot.example",
        public_bill_bot_name="LEDGER_BOT",
        bill_web_token=None,
        timezone=BEIJING_TZ,
    )

    url = bot.build_bill_url(-1001, "2026-07-03")
    query = parse_qs(urlparse(url).query)

    assert query["begintime"] == ["2026-07-03 04:00:00"]
    assert query["endtime"] == ["2026-07-05 00:00:00"]
