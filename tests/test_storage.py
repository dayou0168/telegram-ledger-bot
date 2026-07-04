from __future__ import annotations

from datetime import datetime, timedelta, timezone
from pathlib import Path
from tempfile import TemporaryDirectory

from ledger_bot.storage import Storage, business_day_key


BEIJING_TZ = timezone(timedelta(hours=8), name="Asia/Shanghai")


def make_record(chat_id: int, day_key: str, *, amount: str = "100", is_balance: int = 0) -> dict[str, object]:
    return {
        "chat_id": chat_id,
        "kind": "deposit",
        "amount": amount,
        "currency": "CNY",
        "exchange_rate": "1",
        "fee_rate": "0",
        "amount_cny": amount,
        "amount_usdt": amount,
        "commission_cny": "0",
        "net_usdt": amount,
        "actor_user_id": 1,
        "actor_name": "operator",
        "subject_user_id": None,
        "subject_name": None,
        "note": None,
        "is_balance": is_balance,
        "source_message_id": None,
        "day_key": day_key,
        "created_at": datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ).isoformat(),
    }


def test_new_group_defaults_to_midnight_cutoff() -> None:
    with TemporaryDirectory() as tmp:
        storage = Storage(Path(tmp) / "bot.db")
        try:
            now = datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ)
            group = storage.ensure_group(-100, "测试群", now)

            assert group["day_cutoff_hour"] == 0
        finally:
            storage.conn.close()


def test_beijing_midnight_business_day_key() -> None:
    tz = BEIJING_TZ

    assert business_day_key(datetime(2026, 7, 4, 23, 59, tzinfo=tz), 0, tz) == "2026-07-04"
    assert business_day_key(datetime(2026, 7, 5, 0, 0, tzinfo=tz), 0, tz) == "2026-07-05"


def test_cutoff_off_lists_and_clears_current_open_bill() -> None:
    with TemporaryDirectory() as tmp:
        storage = Storage(Path(tmp) / "bot.db")
        try:
            now = datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ)
            storage.ensure_group(-100, "测试群", now)
            storage.insert_record(make_record(-100, "2026-07-03", amount="100"))
            storage.insert_record(make_record(-100, "2026-07-04", amount="200"))
            storage.insert_record(make_record(-100, "2026-07-02", amount="50", is_balance=1))

            ordinary_rows = storage.list_records_for_day(-100, "2026-07-04")
            all_rows = storage.list_records_for_day(-100, "2026-07-04", all_days=True)

            assert [row["amount"] for row in ordinary_rows] == ["200", "50"]
            assert [row["amount"] for row in all_rows] == ["100", "200", "50"]

            deleted = storage.soft_delete_day(-100, "2026-07-04", now, all_days=True)
            remaining = storage.list_records_for_day(-100, "2026-07-04", all_days=True)

            assert deleted == 2
            assert [row["amount"] for row in remaining] == ["50"]
        finally:
            storage.conn.close()
