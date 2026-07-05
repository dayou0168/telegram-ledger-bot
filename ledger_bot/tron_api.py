from __future__ import annotations

import json
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from datetime import datetime, tzinfo
from decimal import Decimal
from html import escape
from typing import Any

from .address_watch import Trc20Transfer, format_address, format_amount, short_hash, tronscan_tx_url


class TronGridError(RuntimeError):
    pass


@dataclass(frozen=True)
class TronAddressInfo:
    address: str
    trx_balance: Decimal
    usdt_balance: Decimal
    created_at: datetime | None
    latest_active_at: datetime | None
    permission_summary: str


def safe_header_value(value: str | None) -> str | None:
    if value is None:
        return None
    cleaned = value.strip()
    if not cleaned:
        return None
    try:
        cleaned.encode("latin-1")
    except UnicodeEncodeError:
        return None
    return cleaned


def normalize_tronscan_transfer(row: dict[str, Any], *, contract_address: str) -> dict[str, Any]:
    decimals = row.get("decimals")
    token_info = row.get("tokenInfo") if isinstance(row.get("tokenInfo"), dict) else {}
    if decimals is None:
        decimals = token_info.get("tokenDecimal", 6)
    return {
        "transaction_id": row.get("hash") or row.get("transaction_id"),
        "token_info": {
            "symbol": token_info.get("tokenAbbr") or row.get("token_name") or "USDT",
            "address": row.get("contract_address") or row.get("id") or contract_address,
            "decimals": int(decimals or 6),
        },
        "block_timestamp": row.get("block_timestamp"),
        "from": row.get("from"),
        "to": row.get("to"),
        "type": row.get("event_type") or "Transfer",
        "value": row.get("amount") or row.get("value") or "0",
    }


def normalize_tronscan_account(row: dict[str, Any]) -> dict[str, Any]:
    trc20_tokens: list[dict[str, str]] = []
    for token in row.get("trc20token_balances") or []:
        token_id = token.get("tokenId")
        if not token_id:
            continue
        trc20_tokens.append({token_id: str(token.get("balance") or "0")})
    return {
        "balance": row.get("balance") or row.get("trxBalance") or 0,
        "trc20": trc20_tokens,
        "create_time": row.get("date_created") or row.get("create_time") or row.get("date_created_at"),
        "latest_opration_time": row.get("latest_operation_time")
        or row.get("latest_opration_time")
        or row.get("latest_transfer_time"),
        "owner_permission": row.get("ownerPermission") or row.get("owner_permission"),
        "active_permission": row.get("activePermissions") or row.get("active_permission") or [],
    }


@dataclass(frozen=True)
class TronGridClient:
    api_base: str = "https://apilist.tronscanapi.com/api"
    api_key: str | None = None
    request_timeout: int = 30

    def uses_tronscan_api(self) -> bool:
        return "tronscanapi.com" in self.api_base.lower()

    def get_json(self, path: str, params: dict[str, Any]) -> dict[str, Any]:
        query = urllib.parse.urlencode({k: v for k, v in params.items() if v is not None})
        url = f"{self.api_base.rstrip('/')}{path}"
        if query:
            url = f"{url}?{query}"
        headers = {"Accept": "application/json"}
        api_key = safe_header_value(self.api_key)
        if api_key:
            headers["TRON-PRO-API-KEY"] = api_key
        try:
            return self.open_json(url, headers)
        except urllib.error.HTTPError as exc:
            body = exc.read().decode("utf-8", errors="replace")
            raise TronGridError(f"TronGrid HTTP {exc.code}: {body}") from exc
        except (urllib.error.URLError, TimeoutError) as exc:
            raise TronGridError(f"TronGrid network error: {exc}") from exc

    def open_json(self, url: str, headers: dict[str, str]) -> dict[str, Any]:
        request = urllib.request.Request(url, headers=headers, method="GET")
        with urllib.request.urlopen(request, timeout=self.request_timeout) as response:
            return json.loads(response.read().decode("utf-8"))

    def fetch_trc20_transfers(
        self,
        address: str,
        *,
        contract_address: str,
        min_timestamp_ms: int,
        limit: int = 200,
    ) -> list[dict[str, Any]]:
        if self.uses_tronscan_api():
            return self.fetch_tronscan_trc20_transfers(
                address,
                contract_address=contract_address,
                min_timestamp_ms=min_timestamp_ms,
                limit=limit,
            )
        path = f"/v1/accounts/{urllib.parse.quote(address)}/transactions/trc20"
        params: dict[str, Any] = {
            "only_confirmed": "false",
            "contract_address": contract_address,
            "min_timestamp": min_timestamp_ms,
            "limit": limit,
            "order_by": "block_timestamp,asc",
        }
        rows: list[dict[str, Any]] = []
        fingerprint: str | None = None
        while True:
            if fingerprint:
                params["fingerprint"] = fingerprint
            data = self.get_json(path, params)
            rows.extend(data.get("data") or [])
            fingerprint = (data.get("meta") or {}).get("fingerprint")
            if not fingerprint:
                break
        return rows

    def fetch_tronscan_trc20_transfers(
        self,
        address: str,
        *,
        contract_address: str,
        min_timestamp_ms: int,
        limit: int = 200,
    ) -> list[dict[str, Any]]:
        rows: list[dict[str, Any]] = []
        page_limit = min(max(limit, 1), 50)
        for direction in (1, 2):
            start = 0
            while len(rows) < limit:
                data = self.get_json(
                    "/token_trc20/transfers-with-status",
                    {
                        "trc20Id": contract_address,
                        "address": address,
                        "direction": direction,
                        "start_timestamp": min_timestamp_ms,
                        "start": start,
                        "limit": page_limit,
                        "reverse": "false",
                    },
                )
                page_rows = data.get("data") or []
                rows.extend(
                    normalize_tronscan_transfer(row, contract_address=contract_address)
                    for row in page_rows
                )
                if len(page_rows) < page_limit:
                    break
                start += page_limit
        rows.sort(key=lambda row: int(row.get("block_timestamp") or 0))
        return rows[:limit]

    def fetch_account(self, address: str) -> dict[str, Any] | None:
        if self.uses_tronscan_api():
            data = self.get_json("/account", {"address": address})
            return normalize_tronscan_account(data)
        path = f"/v1/accounts/{urllib.parse.quote(address)}"
        data = self.get_json(path, {})
        rows = data.get("data") or []
        return rows[0] if rows else None

    def fetch_recent_trc20_transfers(
        self,
        address: str,
        *,
        contract_address: str,
        limit: int = 5,
    ) -> list[dict[str, Any]]:
        if self.uses_tronscan_api():
            rows: list[dict[str, Any]] = []
            for direction in (1, 2):
                data = self.get_json(
                    "/token_trc20/transfers-with-status",
                    {
                        "trc20Id": contract_address,
                        "address": address,
                        "direction": direction,
                        "start": 0,
                        "limit": min(max(limit, 1), 50),
                        "reverse": "true",
                    },
                )
                rows.extend(
                    normalize_tronscan_transfer(row, contract_address=contract_address)
                    for row in (data.get("data") or [])
                )
            rows.sort(key=lambda row: int(row.get("block_timestamp") or 0), reverse=True)
            return rows[:limit]
        path = f"/v1/accounts/{urllib.parse.quote(address)}/transactions/trc20"
        data = self.get_json(
            path,
            {
                "only_confirmed": "true",
                "contract_address": contract_address,
                "limit": limit,
                "order_by": "block_timestamp,desc",
            },
        )
        return data.get("data") or []


def parse_usdt_transfer(
    row: dict[str, Any],
    *,
    watched_address: str,
    watched_label: str | None,
    timezone: tzinfo,
    usdt_contract: str,
) -> Trc20Transfer | None:
    token_info = row.get("token_info") or {}
    if token_info.get("address") != usdt_contract:
        return None
    if str(row.get("type", "")).lower() != "transfer":
        return None

    from_address = row.get("from")
    to_address = row.get("to")
    if to_address == watched_address:
        direction = "income"
    elif from_address == watched_address:
        direction = "expense"
    else:
        return None

    decimals = int(token_info.get("decimals", 6))
    value = Decimal(str(row.get("value", "0"))) / (Decimal(10) ** decimals)
    tx_time = datetime.fromtimestamp(int(row["block_timestamp"]) / 1000, tz=timezone)
    return Trc20Transfer(
        direction=direction,
        amount=value,
        from_address=from_address,
        to_address=to_address,
        tx_time=tx_time,
        tx_hash=row["transaction_id"],
        watched_address=watched_address,
        watched_label=watched_label,
    )


def parse_tron_address_info(
    row: dict[str, Any] | None,
    *,
    address: str,
    timezone: tzinfo,
    usdt_contract: str,
) -> TronAddressInfo:
    account = row or {}
    return TronAddressInfo(
        address=address,
        trx_balance=sun_to_trx(account.get("balance")),
        usdt_balance=trc20_token_balance(account.get("trc20"), usdt_contract),
        created_at=timestamp_ms_to_datetime(account.get("create_time"), timezone),
        latest_active_at=latest_active_time(account, timezone),
        permission_summary=permission_summary(account),
    )


def format_tron_address_query(info: TronAddressInfo, transfers: list[Trc20Transfer]) -> str:
    lines = [
        "<b>TRX 地址查询</b>",
        "",
        f"查询地址： {format_address(info.address)}",
        f"TRX余额： {format_amount(info.trx_balance)} TRX",
        f"USDT余额： {format_amount(info.usdt_balance)} USDT",
        f"创建时间： {format_optional_datetime(info.created_at)}",
        f"活跃时间： {format_optional_datetime(info.latest_active_at)}",
        f"权限： {escape(info.permission_summary)}",
        "",
        "<b>最近流水</b>",
    ]
    if not transfers:
        lines.append("暂无 USDT 流水")
        return "\n".join(lines)

    for transfer in transfers[:5]:
        is_income = transfer.direction == "income"
        direction = "收入" if is_income else "支出"
        sign = "+" if is_income else "-"
        counterparty_label = "来自" if is_income else "去向"
        counterparty = transfer.from_address if is_income else transfer.to_address
        lines.append(
            f"{transfer.tx_time:%m-%d %H:%M} {direction} {sign}{format_amount(transfer.amount)} USDT "
            f"{counterparty_label} {format_address(counterparty)} "
            f'<a href="{tronscan_tx_url(transfer.tx_hash)}">{escape(short_hash(transfer.tx_hash))}</a>'
        )
    return "\n".join(lines)


def sun_to_trx(value: Any) -> Decimal:
    return Decimal(str(value or "0")) / Decimal("1000000")


def trc20_token_balance(tokens: Any, contract_address: str) -> Decimal:
    raw_value: Any = 0
    if isinstance(tokens, dict):
        raw_value = tokens.get(contract_address) or tokens.get(contract_address.lower()) or 0
    elif isinstance(tokens, list):
        for item in tokens:
            if isinstance(item, dict):
                if contract_address in item:
                    raw_value = item[contract_address]
                    break
                if contract_address.lower() in item:
                    raw_value = item[contract_address.lower()]
                    break
    return Decimal(str(raw_value or "0")) / Decimal("1000000")


def timestamp_ms_to_datetime(value: Any, timezone: tzinfo) -> datetime | None:
    if value in {None, "", 0, "0"}:
        return None
    return datetime.fromtimestamp(int(value) / 1000, tz=timezone)


def latest_active_time(row: dict[str, Any], timezone: tzinfo) -> datetime | None:
    candidates = [
        row.get("latest_opration_time"),
        row.get("latest_operation_time"),
        row.get("latest_consume_time"),
        row.get("latest_consume_free_time"),
        row.get("latest_withdraw_time"),
    ]
    values = [int(value) for value in candidates if value not in {None, "", 0, "0"}]
    if not values:
        return None
    return timestamp_ms_to_datetime(max(values), timezone)


def permission_summary(row: dict[str, Any]) -> str:
    if not row:
        return "未激活"

    flags: list[str] = []
    owner_permission = row.get("owner_permission") or {}
    owner_keys = owner_permission.get("keys") or []
    owner_threshold = int(owner_permission.get("threshold") or 1)
    if owner_threshold > 1 or len(owner_keys) > 1:
        flags.append("Owner多签")

    active_permissions = row.get("active_permission") or []
    multi_active = False
    for permission in active_permissions:
        keys = permission.get("keys") or []
        threshold = int(permission.get("threshold") or 1)
        if threshold > 1 or len(keys) > 1:
            multi_active = True
            break
    if multi_active:
        flags.append("Active多签")
    elif len(active_permissions) > 1:
        flags.append(f"Active权限{len(active_permissions)}组")

    return "、".join(flags) if flags else "普通权限"


def format_optional_datetime(value: datetime | None) -> str:
    return value.strftime("%Y-%m-%d %H:%M:%S") if value else "暂无"
