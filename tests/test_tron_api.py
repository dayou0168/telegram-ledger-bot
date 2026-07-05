from datetime import timezone
from decimal import Decimal
from io import BytesIO
import urllib.error

import pytest

from ledger_bot.tron_api import TronGridClient, TronGridError
from ledger_bot.tron_api import (
    format_tron_address_query,
    parse_tron_address_info,
    parse_usdt_transfer,
    safe_header_value,
)


USDT = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"


class CapturingTronGridClient(TronGridClient):
    def open_json(self, url: str, headers: dict[str, str]) -> dict:
        object.__setattr__(self, "seen_url", url)
        object.__setattr__(self, "seen_headers", headers)
        return {"data": []}


class FailingTronGridClient(TronGridClient):
    def open_json(self, url: str, headers: dict[str, str]) -> dict:
        calls = getattr(self, "calls", 0) + 1
        object.__setattr__(self, "calls", calls)
        raise urllib.error.HTTPError(
            url,
            401,
            "Unauthorized",
            hdrs=None,
            fp=BytesIO(b'{"Error":"ApiKey not exists"}'),
        )


def test_safe_header_value_ignores_non_latin_placeholder() -> None:
    assert safe_header_value("替换成你的TronGridKey") is None


def test_safe_header_value_trims_ascii_key() -> None:
    assert safe_header_value("  abc123  ") == "abc123"


def test_fetch_trc20_transfers_uses_tronscan_api_key_and_timestamp() -> None:
    client = CapturingTronGridClient(api_key="  key123  ")

    rows = client.fetch_trc20_transfers(
        "TGhAAySHUUcEGua33pZZ88wP3bA6XSeQuZ",
        contract_address=USDT,
        min_timestamp_ms=123,
    )

    assert rows == []
    assert client.seen_headers["TRON-PRO-API-KEY"] == "key123"
    assert "/token_trc20/transfers-with-status" in client.seen_url
    assert f"trc20Id={USDT}" in client.seen_url
    assert "start_timestamp=123" in client.seen_url


def test_fetch_trc20_transfers_can_still_use_trongrid_base() -> None:
    client = CapturingTronGridClient(api_base="https://api.trongrid.io", api_key="key123")

    client.fetch_trc20_transfers(
        "TGhAAySHUUcEGua33pZZ88wP3bA6XSeQuZ",
        contract_address=USDT,
        min_timestamp_ms=123,
    )

    assert client.seen_headers["TRON-PRO-API-KEY"] == "key123"
    assert "only_confirmed=false" in client.seen_url
    assert "min_timestamp=123" in client.seen_url


def test_trongrid_key_error_does_not_fallback_to_public_request() -> None:
    client = FailingTronGridClient(api_key="key123")

    with pytest.raises(TronGridError, match="TronGrid HTTP 401"):
        client.get_json("/v1/accounts/T/transactions/trc20", {})

    assert client.calls == 1


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
