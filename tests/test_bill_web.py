from __future__ import annotations

from datetime import datetime, timedelta, timezone
from pathlib import Path
from tempfile import TemporaryDirectory
from types import SimpleNamespace

from ledger_bot.bill_web import handle_bill_web_request, render_bill_page
from ledger_bot.storage import Storage


BEIJING_TZ = timezone(timedelta(hours=8), name="Asia/Shanghai")


def make_bill_record(chat_id: int, day_key: str) -> dict[str, object]:
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
        "created_at": datetime(2026, 7, 5, 12, tzinfo=BEIJING_TZ).isoformat(),
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
            assert "今日入款" in html
            assert "客户A" in html
            assert "测试备注" in html
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
