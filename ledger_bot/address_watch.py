from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from decimal import Decimal, InvalidOperation
from html import escape


@dataclass(frozen=True)
class Trc20Transfer:
    direction: str
    amount: Decimal
    from_address: str
    to_address: str
    tx_time: datetime
    tx_hash: str
    watched_address: str
    watched_label: str | None = None


def format_transfer_notice(transfer: Trc20Transfer) -> str:
    if transfer.direction not in {"income", "expense"}:
        raise ValueError("direction must be income or expense")

    is_income = transfer.direction == "income"
    direction_label = "⬇️收入" if is_income else "⬆️支出"
    amount = transfer.amount if is_income else -abs(transfer.amount)
    lines = [
        f"交易类型： {direction_label}",
        f"交易金额： {format_amount(amount)} USDT",
    ]
    if is_income:
        lines.append(f"出账地址： {format_address(transfer.from_address)}")
        lines.append(f"入账地址： {format_watched_address(transfer.to_address, transfer)}")
    else:
        lines.append(f"出账地址： {format_watched_address(transfer.from_address, transfer)}")
        lines.append(f"入账地址： {format_address(transfer.to_address)}")
    lines.extend(
        [
            f"交易时间： {transfer.tx_time:%Y-%m-%d %H:%M:%S}",
            f'交易哈希： <a href="{tronscan_tx_url(transfer.tx_hash)}">{escape(short_hash(transfer.tx_hash))}</a>',
        ]
    )
    return "\n".join(lines)


def should_notify_transfer(transfer: Trc20Transfer, settings) -> bool:
    if transfer.direction == "income":
        enabled = bool(settings["watch_income"])
    elif transfer.direction == "expense":
        enabled = bool(settings["watch_expense"])
    else:
        return False
    return enabled and abs(transfer.amount) >= min_notify_amount(settings)


def min_notify_amount(settings) -> Decimal:
    try:
        raw = settings["min_notify_amount"]
    except (KeyError, IndexError, TypeError):
        raw = "0"
    try:
        amount = Decimal(str(raw or "0"))
    except InvalidOperation:
        return Decimal("0")
    if amount < 0:
        return Decimal("0")
    return amount


def format_watched_address(address: str, transfer: Trc20Transfer) -> str:
    suffix = f" ← {escape(transfer.watched_label)}" if transfer.watched_label else ""
    return f"{format_address(address)}{suffix}"


def format_address(address: str) -> str:
    return f"<code>{escape(address)}</code>"


def short_address(address: str) -> str:
    if len(address) <= 16:
        return address
    return f"{address[:6]}...{address[-6:]}"


def short_hash(tx_hash: str) -> str:
    if len(tx_hash) <= 12:
        return tx_hash
    return f"{tx_hash[:4]}...{tx_hash[-4:]}"


def tronscan_tx_url(tx_hash: str) -> str:
    return f"https://tronscan.org/#/transaction/{escape(tx_hash, quote=True)}"


def format_amount(amount: Decimal) -> str:
    normalized = amount.normalize()
    text = f"{normalized:f}"
    if "." in text:
        text = text.rstrip("0").rstrip(".")
    return text or "0"
