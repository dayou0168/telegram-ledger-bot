from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from decimal import Decimal
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
        lines.append(f"出账地址： {transfer.from_address}")
        lines.append(f"入账地址： {format_watched_address(transfer.to_address, transfer)}")
    else:
        lines.append(f"出账地址： {format_watched_address(transfer.from_address, transfer)}")
        lines.append(f"入账地址： {transfer.to_address}")
    lines.extend(
        [
            f"交易时间： {transfer.tx_time:%Y-%m-%d %H:%M:%S}",
            f'交易哈希： <a href="{tronscan_tx_url(transfer.tx_hash)}">{short_hash(transfer.tx_hash)}</a>',
        ]
    )
    return "\n".join(lines)


def should_notify_transfer(direction: str, settings) -> bool:
    if direction == "income":
        return bool(settings["watch_income"])
    if direction == "expense":
        return bool(settings["watch_expense"])
    return False


def format_watched_address(address: str, transfer: Trc20Transfer) -> str:
    suffix = f" ← {transfer.watched_label}" if transfer.watched_label else ""
    return f"{address}{suffix}"


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
