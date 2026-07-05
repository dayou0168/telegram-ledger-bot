from datetime import timezone
from decimal import Decimal

from ledger_bot.tron_api import (
    format_tron_address_query,
    parse_tron_address_info,
    parse_usdt_transfer,
    safe_header_value,
)


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


def test_parse_tron_address_info_balances_and_permissions() -> None:
    row = {
        "balance": 123456789,
        "trc20": [{USDT: "360000000"}],
        "create_time": 1783200000000,
        "latest_opration_time": 1783206840000,
        "owner_permission": {"threshold": 2, "keys": [{"address": "a"}, {"address": "b"}]},
    }

    info = parse_tron_address_info(
        row,
        address="TGhAAySHUUcEGua33pZZ88wP3bA6XSeQuZ",
        timezone=timezone.utc,
        usdt_contract=USDT,
    )

    assert info.trx_balance == Decimal("123.456789")
    assert info.usdt_balance == Decimal("360")
    assert info.created_at is not None
    assert info.latest_active_at is not None
    assert info.permission_summary == "Owner多签"


def test_format_tron_address_query_links_hash_and_codes_address() -> None:
    info = parse_tron_address_info(
        {"balance": 1000000, "trc20": [{USDT: "2500000"}]},
        address="TGhAAySHUUcEGua33pZZ88wP3bA6XSeQuZ",
        timezone=timezone.utc,
        usdt_contract=USDT,
    )
    transfer = parse_usdt_transfer(
        {
            "transaction_id": "3f7bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb95ae",
            "token_info": {"symbol": "USDT", "address": USDT, "decimals": 6},
            "block_timestamp": 1783206840000,
            "from": "TUFa599xQPfoSdG68sjaLTb5tFeyQPh7kP",
            "to": "TGhAAySHUUcEGua33pZZ88wP3bA6XSeQuZ",
            "type": "Transfer",
            "value": "360000000",
        },
        watched_address="TGhAAySHUUcEGua33pZZ88wP3bA6XSeQuZ",
        watched_label=None,
        timezone=timezone.utc,
        usdt_contract=USDT,
    )

    assert transfer is not None
    text = format_tron_address_query(info, [transfer])

    assert "<b>TRX 地址查询</b>" in text
    assert "<code>TGhAAySHUUcEGua33pZZ88wP3bA6XSeQuZ</code>" in text
    assert "TRX余额： 1 TRX" in text
    assert "USDT余额： 2.5 USDT" in text
    assert 'https://tronscan.org/#/transaction/3f7bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb95ae' in text
