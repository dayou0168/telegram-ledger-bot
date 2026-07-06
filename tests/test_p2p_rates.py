from __future__ import annotations

from datetime import datetime, timedelta, timezone
from decimal import Decimal
from pathlib import Path
from tempfile import TemporaryDirectory
import threading
import time
from types import SimpleNamespace

from ledger_bot.bot import LedgerBot
from ledger_bot.p2p_rates import P2POrderBookEntry, build_order_book_monitoring_body, parse_order_book_top
from ledger_bot.storage import Storage


BEIJING_TZ = timezone(timedelta(hours=8), name="Asia/Shanghai")


def test_order_book_body_uses_sell_usdt_direction() -> None:
    body = build_order_book_monitoring_body(
        market="okx",
        fiat_unit="CNY",
        asset="USDT",
        trade_methods=["aliPay"],
        limit=10,
    )

    assert body["tradeType1"] == "BUY"
    assert body["tradeType2"] == "BUY"
    assert body["tradeMethods1"] == ["aliPay"]


def test_parse_order_book_top_sell_usdt_entries() -> None:
    payload = {
        "success": True,
        "data": [
            {
                "pos": 1,
                "buy": {
                    "price": 6.73,
                    "limit_min": "99999.00",
                    "limit_max": "161520.00",
                    "surplus_amount": 24000,
                    "updated_at_ts": 1783181279,
                    "trade_methods": ["aliPay"],
                    "user": {
                        "nickname": "商家A",
                        "orders": 385,
                        "good_reviews_percent": 96.25,
                    },
                },
            },
            {
                "pos": 2,
                "buy": {
                    "price": "6.72",
                    "limit_min": "30000.00",
                    "limit_max": "2016000.00",
                    "surplus_amount": "300000",
                    "trade_methods": ["aliPay", "bank"],
                    "user": {
                        "nickname": "商家B",
                        "orders": 243793,
                        "good_reviews_percent": "99.98",
                    },
                },
            },
        ],
    }

    entries = parse_order_book_top(payload, limit=10)

    assert len(entries) == 2
    assert entries[0].rank == 1
    assert entries[0].price == Decimal("6.73")
    assert entries[0].merchant_name == "商家A"
    assert entries[0].methods == ("aliPay",)
    assert entries[0].limit_min == Decimal("99999.00")
    assert entries[1].price == Decimal("6.72")
    assert entries[1].methods == ("aliPay", "bank")


def test_refresh_realtime_rates_updates_groups_from_one_shared_fetch() -> None:
    class FakeP2PClient:
        def __init__(self) -> None:
            self.calls: list[dict[str, object]] = []

        def fetch_order_book_top(self, **kwargs: object) -> list[P2POrderBookEntry]:
            self.calls.append(kwargs)
            return [
                _order_book_entry(1, "6.70"),
                _order_book_entry(2, "6.80"),
                _order_book_entry(3, "6.90"),
            ]

    with TemporaryDirectory() as temp_dir:
        storage = Storage(Path(temp_dir) / "ledger.db")
        now = datetime(2026, 7, 5, 12, 0, tzinfo=BEIJING_TZ)
        storage.ensure_group(-1001, "rank 1", now)
        storage.update_group(
            -1001,
            now,
            deposit_exchange_rate="1",
            realtime_rate=1,
            realtime_rate_rank=1,
            realtime_rate_offset="-0.1",
        )
        storage.ensure_group(-1002, "rank 3", now)
        storage.update_group(
            -1002,
            now,
            deposit_exchange_rate="1",
            realtime_rate=1,
            realtime_rate_rank=3,
            realtime_rate_offset="0.5",
        )
        storage.ensure_group(-1003, "fixed", now)
        storage.update_group(
            -1003,
            now,
            deposit_exchange_rate="1",
            realtime_rate=0,
            realtime_rate_rank=1,
            realtime_rate_offset="0",
        )

        try:
            fake_client = FakeP2PClient()
            bot = object.__new__(LedgerBot)
            bot.storage = storage
            bot.p2p_rate_client = fake_client
            bot.cached_otc_top_entries = []
            bot.cached_otc_top_at = 0.0
            bot.p2p_cache_lock = threading.Lock()
            bot.config = SimpleNamespace(
                timezone=BEIJING_TZ,
                p2p_rate_market="okx",
                p2p_rate_fiat_unit="CNY",
                p2p_rate_asset="USDT",
                p2p_rate_trade_methods=("aliPay",),
                p2p_rate_cache_ttl_seconds=180,
            )

            updated = bot.refresh_realtime_rates()

            assert updated == 2
            assert len(fake_client.calls) == 1
            assert fake_client.calls[0]["limit"] == 10
            assert storage.get_group(-1001)["deposit_exchange_rate"] == "6.60"
            assert storage.get_group(-1002)["deposit_exchange_rate"] == "7.40"
            assert storage.get_group(-1003)["deposit_exchange_rate"] == "1"
        finally:
            storage.conn.close()


def test_cached_rate_is_used_for_ledger_entries_without_fetching() -> None:
    bot = object.__new__(LedgerBot)
    entry = SimpleNamespace(exchange_rate=None, kind="deposit")

    rate = bot.effective_rate({"deposit_exchange_rate": "6.60", "payout_exchange_rate": "1"}, entry)

    assert rate == Decimal("6.60")


def test_otc_top_entries_use_cache_within_ttl() -> None:
    class FailingP2PClient:
        def fetch_order_book_top(self, **kwargs: object) -> list[P2POrderBookEntry]:
            raise AssertionError("cache should avoid fetching")

    entries = [_order_book_entry(rank, f"6.7{rank}") for rank in range(1, 11)]
    bot = object.__new__(LedgerBot)
    bot.p2p_rate_client = FailingP2PClient()
    bot.cached_otc_top_entries = entries
    bot.cached_otc_top_at = time.monotonic()
    bot.p2p_cache_lock = threading.Lock()
    bot.config = SimpleNamespace(p2p_rate_cache_ttl_seconds=180)

    assert bot.get_otc_top_entries(10) == entries


def test_telegram_poll_timeout_is_not_limited_by_background_intervals() -> None:
    bot = object.__new__(LedgerBot)
    bot.config = SimpleNamespace(
        tron_poll_interval_seconds=0,
        p2p_rate_refresh_seconds=12,
        poll_timeout=50,
    )

    assert bot.telegram_poll_timeout() == 50


def _order_book_entry(rank: int, price: str) -> P2POrderBookEntry:
    return P2POrderBookEntry(
        rank=rank,
        price=Decimal(price),
        merchant_name=f"merchant {rank}",
        methods=("aliPay",),
        limit_min=None,
        limit_max=None,
        surplus_amount=None,
        orders=None,
        good_reviews_percent=None,
        updated_at_ts=None,
    )
