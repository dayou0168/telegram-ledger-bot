from __future__ import annotations

from datetime import datetime, timedelta, timezone
from pathlib import Path
from tempfile import TemporaryDirectory

from ledger_bot.storage import Storage, TelegramUser, bill_window_for_day, business_day_key, current_business_day_key


BEIJING_TZ = timezone(timedelta(hours=8), name="Asia/Shanghai")


def make_record(
    chat_id: int,
    day_key: str,
    *,
    amount: str = "100",
    is_balance: int = 0,
    created_at: datetime | None = None,
) -> dict[str, object]:
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
        "created_at": (created_at or datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ)).isoformat(),
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


def test_cutoff_change_waits_until_next_new_boundary_after_open_bill() -> None:
    with TemporaryDirectory() as tmp:
        storage = Storage(Path(tmp) / "bot.db")
        try:
            now = datetime(2026, 7, 3, 12, tzinfo=BEIJING_TZ)
            storage.ensure_group(-100, "测试群", now)
            storage.update_group(-100, now, day_cutoff_hour=4)
            storage.insert_record(
                make_record(
                    -100,
                    "2026-07-03",
                    amount="10",
                    created_at=datetime(2026, 7, 3, 5, tzinfo=BEIJING_TZ),
                )
            )

            group = storage.set_day_cutoff_hour(-100, now, 0, BEIJING_TZ)

            assert group["day_cutoff_hour"] == 4
            assert group["pending_day_cutoff_hour"] == 0
            assert group["open_bill_day_key"] == "2026-07-03"
            assert group["open_bill_begin_at"] == "2026-07-03T04:00:00+08:00"
            assert group["open_bill_end_at"] == "2026-07-05T00:00:00+08:00"
            assert current_business_day_key(datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ), group, BEIJING_TZ) == "2026-07-03"

            storage.insert_record(
                make_record(
                    -100,
                    "2026-07-03",
                    amount="100",
                    created_at=datetime(2026, 7, 3, 5, tzinfo=BEIJING_TZ),
                )
            )
            storage.insert_record(
                make_record(
                    -100,
                    "2026-07-03",
                    amount="200",
                    created_at=datetime(2026, 7, 4, 1, tzinfo=BEIJING_TZ),
                )
            )
            storage.insert_record(
                make_record(
                    -100,
                    "2026-07-03",
                    amount="300",
                    created_at=datetime(2026, 7, 4, 23, tzinfo=BEIJING_TZ),
                )
            )
            rows = storage.list_records_for_period(
                -100,
                datetime(2026, 7, 3, 4, tzinfo=BEIJING_TZ),
                datetime(2026, 7, 5, 0, tzinfo=BEIJING_TZ),
            )

            assert [row["amount"] for row in rows] == ["10", "100", "200", "300"]

            group = storage.apply_due_day_cutoff(-100, datetime(2026, 7, 5, 0, tzinfo=BEIJING_TZ), BEIJING_TZ)

            assert group["day_cutoff_hour"] == 0
            assert group["pending_day_cutoff_hour"] is None
            assert current_business_day_key(datetime(2026, 7, 5, 0, tzinfo=BEIJING_TZ), group, BEIJING_TZ) == "2026-07-05"
        finally:
            storage.conn.close()


def test_cutoff_change_from_midnight_to_four_waits_until_next_four_boundary() -> None:
    with TemporaryDirectory() as tmp:
        storage = Storage(Path(tmp) / "bot.db")
        try:
            now = datetime(2026, 7, 3, 12, tzinfo=BEIJING_TZ)
            storage.ensure_group(-100, "测试群", now)
            storage.update_group(-100, now, day_cutoff_hour=0)
            storage.insert_record(
                make_record(
                    -100,
                    "2026-07-03",
                    amount="10",
                    created_at=datetime(2026, 7, 3, 1, tzinfo=BEIJING_TZ),
                )
            )

            group = storage.set_day_cutoff_hour(-100, now, 4, BEIJING_TZ)

            assert group["day_cutoff_hour"] == 0
            assert group["pending_day_cutoff_hour"] == 4
            assert group["open_bill_day_key"] == "2026-07-03"
            assert group["open_bill_begin_at"] == "2026-07-03T00:00:00+08:00"
            assert group["open_bill_end_at"] == "2026-07-04T04:00:00+08:00"
            assert current_business_day_key(datetime(2026, 7, 4, 1, tzinfo=BEIJING_TZ), group, BEIJING_TZ) == "2026-07-03"

            group = storage.apply_due_day_cutoff(-100, datetime(2026, 7, 4, 4, tzinfo=BEIJING_TZ), BEIJING_TZ)

            assert group["day_cutoff_hour"] == 4
            assert group["pending_day_cutoff_hour"] is None
            assert current_business_day_key(datetime(2026, 7, 4, 4, tzinfo=BEIJING_TZ), group, BEIJING_TZ) == "2026-07-04"
        finally:
            storage.conn.close()


def test_cutoff_change_with_empty_current_bill_applies_immediately() -> None:
    with TemporaryDirectory() as tmp:
        storage = Storage(Path(tmp) / "bot.db")
        try:
            now = datetime(2026, 7, 5, 16, 37, tzinfo=BEIJING_TZ)
            storage.ensure_group(-100, "测试群", now)
            storage.update_group(-100, now, day_cutoff_hour=0)
            storage.insert_record(
                make_record(
                    -100,
                    "2026-07-04",
                    amount="999",
                    created_at=datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ),
                )
            )

            group = storage.set_day_cutoff_hour(-100, now, 4, BEIJING_TZ)

            assert group["day_cutoff_hour"] == 4
            assert group["pending_day_cutoff_hour"] is None
            assert group["open_bill_day_key"] is None
            assert current_business_day_key(now, group, BEIJING_TZ) == "2026-07-05"

            begin, end = bill_window_for_day(group, "2026-07-05", BEIJING_TZ)

            assert begin == datetime(2026, 7, 5, 4, tzinfo=BEIJING_TZ)
            assert end == datetime(2026, 7, 6, 4, tzinfo=BEIJING_TZ)
        finally:
            storage.conn.close()


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


def test_group_is_deduped_by_chat_id_and_title_updates() -> None:
    with TemporaryDirectory() as tmp:
        storage = Storage(Path(tmp) / "bot.db")
        try:
            now = datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ)
            storage.ensure_group(-100123, "旧群名", now)
            group = storage.ensure_group(-100123, "新群名", now)
            rows = storage.list_broadcast_groups()

            assert group["chat_title"] == "新群名"
            assert len(rows) == 1
            assert rows[0]["chat_title"] == "新群名"
        finally:
            storage.conn.close()


def test_set_group_owner_replaces_previous_owner_role() -> None:
    with TemporaryDirectory() as tmp:
        storage = Storage(Path(tmp) / "bot.db")
        try:
            now = datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ)
            storage.ensure_group(-100123, "测试群", now)
            old_owner = TelegramUser(1001, "old", "Old")
            host = TelegramUser(2002, "host", "Host")

            storage.set_group_owner(-100123, old_owner, now=now)
            storage.set_group_owner(-100123, host, now=now)
            rows = storage.list_operators(-100123)

            assert storage.get_group(-100123)["owner_user_id"] == 2002
            assert [(row["user_id"], row["role"]) for row in rows] == [(2002, "owner")]
        finally:
            storage.conn.close()


def test_activate_group_does_not_promote_first_starter() -> None:
    with TemporaryDirectory() as tmp:
        storage = Storage(Path(tmp) / "bot.db")
        try:
            now = datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ)
            storage.ensure_group(-100123, "测试群", now)

            group = storage.activate_group(-100123, now)

            assert group["active"] == 1
            assert group["owner_user_id"] is None
            assert storage.list_operators(-100123) == []
        finally:
            storage.conn.close()


def test_named_broadcast_group_bulk_add_remove_and_title_refresh() -> None:
    with TemporaryDirectory() as tmp:
        storage = Storage(Path(tmp) / "bot.db")
        try:
            now = datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ)
            storage.ensure_group(-1001, "A群", now)
            storage.ensure_group(-1002, "B群", now)
            storage.create_named_broadcast_group("财务", created_by=9, now=now)

            added, known_ids, missing_ids = storage.add_broadcast_group_members(
                "财务",
                [-1001, -1002, -1001, -1999],
                now=now,
            )

            assert added == 2
            assert known_ids == [-1002, -1001]
            assert missing_ids == [-1999]
            assert set(storage.target_chat_ids_for_broadcast_group("财务")) == {-1001, -1002}

            storage.ensure_group(-1002, "B群新名字", now)
            members = storage.list_broadcast_group_members("财务")

            assert {row["chat_id"]: row["chat_title"] for row in members} == {
                -1001: "A群",
                -1002: "B群新名字",
            }

            removed = storage.remove_broadcast_group_members("财务", [-1001, -1999], now=now)

            assert removed == 1
            assert storage.target_chat_ids_for_broadcast_group("财务") == [-1002]
        finally:
            storage.conn.close()


def test_broadcast_job_stores_source_message_and_notify_all() -> None:
    with TemporaryDirectory() as tmp:
        storage = Storage(Path(tmp) / "bot.db")
        try:
            now = datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ)
            job = storage.create_broadcast_job(
                creator_user_id=10,
                scope="all",
                target_chat_ids=[-1001, -1002],
                text="[图片] 说明",
                source_chat_id=10,
                source_message_id=55,
                message_kind="photo",
                notify_all=True,
                now=now,
            )

            assert job["source_chat_id"] == 10
            assert job["source_message_id"] == 55
            assert job["message_kind"] == "photo"
            assert job["notify_all"] == 1
            assert job["target_chat_ids"] == "[-1001, -1002]"
            targets = storage.list_broadcast_job_targets(job["id"])
            assert [row["target_chat_id"] for row in targets] == [-1001, -1002]
            assert [row["status"] for row in targets] == ["pending", "pending"]
        finally:
            storage.conn.close()


def test_broadcast_operator_group_and_chat_permissions() -> None:
    with TemporaryDirectory() as tmp:
        storage = Storage(Path(tmp) / "bot.db")
        try:
            now = datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ)
            storage.ensure_group(-1001, "A", now)
            storage.ensure_group(-1002, "B", now)
            storage.create_named_broadcast_group("finance", created_by=10, now=now)
            storage.add_broadcast_group_members("finance", [-1001], now=now)

            operator = storage.add_broadcast_operator(user_id=20, created_by=10, remark="level1", now=now)
            assert operator["status"] == "active"

            assert storage.grant_broadcast_group_permission("finance", user_id=20, created_by=10, now=now)
            assert storage.grant_broadcast_chat_permission(chat_id=-1002, user_id=20, created_by=10, now=now)

            groups = storage.list_named_broadcast_groups_for_user(20)
            assert [row["name"] for row in groups] == ["finance"]
            assert storage.target_chat_ids_for_user_broadcast_groups(20) == [-1001]
            assert storage.target_chat_ids_for_user_chat_permissions(20) == [-1002]
            assert storage.user_has_any_broadcast_permissions(20)
        finally:
            storage.conn.close()


def test_broadcast_target_records_sent_message_lookup() -> None:
    with TemporaryDirectory() as tmp:
        storage = Storage(Path(tmp) / "bot.db")
        try:
            now = datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ)
            job = storage.create_broadcast_job(
                creator_user_id=10,
                scope="chat:-1001",
                target_chat_ids=[-1001],
                text="hello",
                message_kind="text",
                now=now,
            )

            storage.mark_broadcast_job_target(
                job["id"],
                -1001,
                status="sent",
                sent_message_id=88,
                now=now,
            )

            match = storage.find_broadcast_job_by_sent_message(-1001, 88)
            assert match is not None
            assert match["job_id"] == job["id"]
            assert match["creator_user_id"] == 10
            assert match["sent_message_id"] == 88
        finally:
            storage.conn.close()


def test_forget_group_preserves_records_but_removes_broadcast_target() -> None:
    with TemporaryDirectory() as tmp:
        storage = Storage(Path(tmp) / "bot.db")
        try:
            now = datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ)
            storage.ensure_group(-1001, "测试群", now)
            storage.insert_record(make_record(-1001, "2026-07-04", amount="100"))
            storage.create_named_broadcast_group("财务", created_by=9, now=now)
            storage.add_broadcast_group_members("财务", [-1001], now=now)

            storage.forget_group(-1001)

            assert storage.list_broadcast_groups() == []
            assert storage.target_chat_ids_for_broadcast_group("财务") == []
            rows = storage.list_records_for_day(-1001, "2026-07-04")
            assert [row["amount"] for row in rows] == ["100"]
        finally:
            storage.conn.close()


def test_claim_update_is_idempotent_and_persistent() -> None:
    with TemporaryDirectory() as tmp:
        db_path = Path(tmp) / "bot.db"
        storage = Storage(db_path)
        try:
            now = datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ)

            assert storage.claim_update(100, now)
            assert not storage.claim_update(100, now)
            assert storage.last_processed_update_id() == 100
        finally:
            storage.conn.close()

        storage = Storage(db_path)
        try:
            assert storage.last_processed_update_id() == 100
        finally:
            storage.conn.close()


def test_address_watch_settings_min_amount_and_label_update() -> None:
    with TemporaryDirectory() as tmp:
        storage = Storage(Path(tmp) / "bot.db")
        try:
            now = datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ)
            settings = storage.get_address_watch_settings(1001, now)

            assert settings["min_notify_amount"] == "0"

            storage.update_address_watch_settings(1001, now, min_notify_amount="10")
            settings = storage.get_address_watch_settings(1001, now)
            assert settings["min_notify_amount"] == "10"

            storage.add_address_watch(1001, "TRC20", "TGhAAySHUUcEGua33pZZ88wP3bA6XSeQuZ", "旧备注", now)
            changed = storage.update_address_watch_label(
                1001,
                "TGhAAySHUUcEGua33pZZ88wP3bA6XSeQuZ",
                "新备注",
                now,
            )

            assert changed == 1
            row = storage.list_address_watches(1001)[0]
            assert row["label"] == "新备注"
        finally:
            storage.conn.close()


def test_address_verification_counts_per_group_and_address() -> None:
    with TemporaryDirectory() as tmp:
        storage = Storage(Path(tmp) / "bot.db")
        try:
            now = datetime(2026, 7, 4, 12, tzinfo=BEIJING_TZ)
            alice = TelegramUser(1001, "alice", "Alice")
            bob = TelegramUser(1002, "bob", "Bob")
            address = "TM1zJaWxmQmPhkdJpcKUh2H6iuz87rMh5W"

            first = storage.record_address_verification(chat_id=-1001, address=address, sender=alice, now=now)
            second = storage.record_address_verification(chat_id=-1001, address=address, sender=bob, now=now)
            third = storage.record_address_verification(chat_id=-1001, address=address, sender=alice, now=now)
            other_group = storage.record_address_verification(chat_id=-1002, address=address, sender=bob, now=now)

            assert first["count"] == 1
            assert first["previous_sender_name"] is None
            assert first["current_sender_name"] == "@alice"
            assert second["count"] == 2
            assert second["previous_sender_name"] == "@alice"
            assert second["current_sender_name"] == "@bob"
            assert third["count"] == 3
            assert third["previous_sender_name"] == "@bob"
            assert third["current_sender_name"] == "@alice"
            assert other_group["count"] == 1
        finally:
            storage.conn.close()
