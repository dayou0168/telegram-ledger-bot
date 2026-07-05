from datetime import datetime, timezone
from decimal import Decimal
from types import SimpleNamespace

from ledger_bot.address_watch import Trc20Transfer, format_transfer_notice, should_notify_transfer
from ledger_bot.bot import LedgerBot


USDT = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"


class FakeGlobalTronClient:
    def __init__(self) -> None:
        self.calls = 0

    def fetch_tronscan_global_trc20_transfers(self, *, contract_address, min_timestamp_ms, pages):  # type: ignore[no-untyped-def]
        self.calls += 1
        assert contract_address == USDT
        assert pages == 1
        return [
            {
                "transaction_id": "abc123",
                "token_info": {"symbol": "USDT", "address": USDT, "decimals": 6},
                "block_timestamp": 1783206840000,
                "from": "TFrom",
                "to": "TWatch",
                "type": "Transfer",
                "value": "360000000",
            },
            {
                "transaction_id": "ignore123",
                "token_info": {"symbol": "USDT", "address": USDT, "decimals": 6},
                "block_timestamp": 1783206840000,
                "from": "TFrom",
                "to": "TOther",
                "type": "Transfer",
                "value": "1",
            },
        ]


class FakeWatchStorage:
    def __init__(self) -> None:
        self.recorded: list[str] = []

    def latest_chain_event_timestamp(self, owner_user_id, address):  # type: ignore[no-untyped-def]
        return None

    def get_address_watch_settings(self, owner_user_id, now):  # type: ignore[no-untyped-def]
        return {"watch_income": 1, "watch_expense": 1, "min_notify_amount": "0"}

    def record_chain_event_notification(self, **kwargs):  # type: ignore[no-untyped-def]
        self.recorded.append(kwargs["tx_hash"])
        return True


class FakeTelegramClient:
    def __init__(self) -> None:
        self.messages: list[tuple[int, str]] = []

    def send_message(self, chat_id, text, **kwargs):  # type: ignore[no-untyped-def]
        self.messages.append((chat_id, text))


def test_income_notice_format() -> None:
    notice = format_transfer_notice(
        Trc20Transfer(
            direction="income",
            amount=Decimal("360"),
            from_address="TUFa599xQPfoSdG68sjaLTb5tFeyQPh7kP",
            to_address="TGhAAySHUUcEGua33pZZ88wP3bA6XSeQuZ",
            tx_time=datetime(2026, 7, 4, 23, 14, 0),
            tx_hash="3f7bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb95ae",
            watched_address="TGhAAySHUUcEGua33pZZ88wP3bA6XSeQuZ",
            watched_label="监控地址",
        )
    )
    assert "交易类型： ⬇️收入" in notice
    assert "交易金额： 360 USDT" in notice
    assert "出账地址： <code>TUFa599xQPfoSdG68sjaLTb5tFeyQPh7kP</code>" in notice
    assert "入账地址： <code>TGhAAySHUUcEGua33pZZ88wP3bA6XSeQuZ</code> ← 监控地址" in notice
    assert '交易哈希： <a href="https://tronscan.org/#/transaction/3f7bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb95ae">3f7b...95ae</a>' in notice


def test_expense_notice_format() -> None:
    notice = format_transfer_notice(
        Trc20Transfer(
            direction="expense",
            amount=Decimal("518"),
            from_address="TGhAAySHUUcEGua33pZZ88wP3bA6XSeQuZ",
            to_address="TTpPrt58mWLhcBq1d9HebDaWxEqmj4tdQU",
            tx_time=datetime(2026, 7, 4, 2, 58, 48),
            tx_hash="818cccccccccccccccccccccccccccccccccccccc0bcf",
            watched_address="TGhAAySHUUcEGua33pZZ88wP3bA6XSeQuZ",
            watched_label="监控地址",
        )
    )
    assert "交易类型： ⬆️支出" in notice
    assert "交易金额： -518 USDT" in notice
    assert "出账地址： <code>TGhAAySHUUcEGua33pZZ88wP3bA6XSeQuZ</code> ← 监控地址" in notice
    assert "入账地址： <code>TTpPrt58mWLhcBq1d9HebDaWxEqmj4tdQU</code>" in notice


def test_should_notify_transfer_honors_min_amount() -> None:
    transfer = Trc20Transfer(
        direction="income",
        amount=Decimal("9.99"),
        from_address="TFrom",
        to_address="TWatch",
        tx_time=datetime(2026, 7, 4, 23, 14, 0),
        tx_hash="abc",
        watched_address="TWatch",
    )
    settings = {"watch_income": 1, "watch_expense": 1, "min_notify_amount": "10"}

    assert not should_notify_transfer(transfer, settings)

    transfer = Trc20Transfer(
        direction="income",
        amount=Decimal("10"),
        from_address="TFrom",
        to_address="TWatch",
        tx_time=datetime(2026, 7, 4, 23, 14, 0),
        tx_hash="abc",
        watched_address="TWatch",
    )

    assert should_notify_transfer(transfer, settings)


def test_address_watch_min_timestamp_uses_latest_event_overlap() -> None:
    bot = object.__new__(LedgerBot)
    bot.storage = SimpleNamespace(latest_chain_event_timestamp=lambda owner_user_id, address: 120_000)

    assert bot.address_watch_min_timestamp_ms({"owner_user_id": 1, "address": "TWatch"}, 10_000) == 90_000


def test_address_watch_min_timestamp_uses_fallback_without_history() -> None:
    bot = object.__new__(LedgerBot)
    bot.storage = SimpleNamespace(latest_chain_event_timestamp=lambda owner_user_id, address: None)

    assert bot.address_watch_min_timestamp_ms({"owner_user_id": 1, "address": "TWatch"}, 10_000) == 10_000


def test_tronscan_global_watch_scan_matches_addresses_locally() -> None:
    bot = object.__new__(LedgerBot)
    bot.config = SimpleNamespace(
        timezone=timezone.utc,
        tron_usdt_contract=USDT,
        tronscan_global_scan_pages=1,
    )
    bot.storage = FakeWatchStorage()
    bot.tron_client = FakeGlobalTronClient()
    bot.client = FakeTelegramClient()

    watches = [
        {"owner_user_id": 1001, "address": "TWatch", "label": "监控地址"},
        {"owner_user_id": 1002, "address": "TNoMatch", "label": None},
    ]

    bot.poll_tronscan_global_address_watches(watches, datetime(2026, 7, 4, tzinfo=timezone.utc), 1)

    assert bot.tron_client.calls == 1
    assert bot.storage.recorded == ["abc123"]
    assert len(bot.client.messages) == 1
    assert bot.client.messages[0][0] == 1001


def test_address_whitelist_reply_includes_chain_info() -> None:
    bot = object.__new__(LedgerBot)
    info = SimpleNamespace(
        created_at=datetime(2026, 7, 6, 2, 18, 20),
        usdt_balance=Decimal("94.85"),
        trx_balance=Decimal("24.360329"),
    )

    lines = bot.address_whitelist_reply(
        "TGhAAySHUUcEGua33pZZ88wP3bA6X5eQuZ",
        {"label": None},
        {"count": 2, "previous_sender_name": None, "current_sender_name": "阿泽"},
        info,
    )

    text = "\n".join(lines)
    assert "💎 <code>TGhAAySHUUcEGua33pZZ88wP3bA6X5eQuZ</code>" in text
    assert "🌐 创建： 2026-07-06 02:18:20" in text
    assert "├ ▣ USDT： 94.85" in text
    assert "├ ▣ TRX： 24.360329" in text
    assert "└✅ 状态： 白名单" in text
