from datetime import timezone
from decimal import Decimal

from ledger_bot.tron_api import parse_usdt_transfer, safe_header_value


USDT = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"


def test_safe_header_value_ignores_non_latin_placeholder() -> None:
    assert safe_header_value("替换成你的TronGridKey") is None


def test_safe_header_value_trims_ascii_key() -> None:
    assert safe_header_value("  abc123  ") == "abc123"


def test_parse_income_transfer() -> None:
    row = {
        "transaction_id": "abc123",
        "token_info": {"symbol": "USDT", "address": USDT, "decimals": 6},
        "block_timestamp": 1783206840000,
        "from": "TFromAddress111111111111111111111111",
        "to": "TWatchedAddress111111111111111111",
        "type": "Transfer",
        "value": "360000000",
    }
    transfer = parse_usdt_transfer(
        row,
        watched_address="TWatchedAddress111111111111111111",
        watched_label="监控地址",
        timezone=timezone.utc,
        usdt_contract=USDT,
    )
    assert transfer is not None
    assert transfer.direction == "income"
    assert transfer.amount == Decimal("360")
    assert transfer.tx_hash == "abc123"


def test_parse_expense_transfer() -> None:
    row = {
        "transaction_id": "def456",
        "token_info": {"symbol": "USDT", "address": USDT, "decimals": 6},
        "block_timestamp": 1783206840000,
        "from": "TWatchedAddress111111111111111111",
        "to": "TToAddress11111111111111111111111",
        "type": "transfer",
        "value": "518000000",
    }
    transfer = parse_usdt_transfer(
        row,
        watched_address="TWatchedAddress111111111111111111",
        watched_label=None,
        timezone=timezone.utc,
        usdt_contract=USDT,
    )
    assert transfer is not None
    assert transfer.direction == "expense"
    assert transfer.amount == Decimal("518")


def test_ignore_non_usdt_contract() -> None:
    row = {
        "transaction_id": "abc123",
        "token_info": {"symbol": "OTHER", "address": "TOther", "decimals": 6},
        "block_timestamp": 1783206840000,
        "from": "TFromAddress111111111111111111111111",
        "to": "TWatchedAddress111111111111111111",
        "type": "Transfer",
        "value": "1",
    }
    assert parse_usdt_transfer(
        row,
        watched_address="TWatchedAddress111111111111111111",
        watched_label=None,
        timezone=timezone.utc,
        usdt_contract=USDT,
    ) is None
