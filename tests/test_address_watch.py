from datetime import datetime
from decimal import Decimal

from ledger_bot.address_watch import Trc20Transfer, format_transfer_notice, should_notify_transfer


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
