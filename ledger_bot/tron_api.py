from __future__ import annotations

import json
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from datetime import datetime, tzinfo
from decimal import Decimal
from typing import Any

from .address_watch import Trc20Transfer


class TronGridError(RuntimeError):
    pass


@dataclass(frozen=True)
class TronGridClient:
    api_base: str = "https://api.trongrid.io"
    api_key: str | None = None
    request_timeout: int = 30

    def get_json(self, path: str, params: dict[str, Any]) -> dict[str, Any]:
        query = urllib.parse.urlencode({k: v for k, v in params.items() if v is not None})
        url = f"{self.api_base.rstrip('/')}{path}"
        if query:
            url = f"{url}?{query}"
        headers = {"Accept": "application/json"}
        if self.api_key:
            headers["TRON-PRO-API-KEY"] = self.api_key
        request = urllib.request.Request(url, headers=headers, method="GET")
        try:
            with urllib.request.urlopen(request, timeout=self.request_timeout) as response:
                return json.loads(response.read().decode("utf-8"))
        except urllib.error.HTTPError as exc:
            body = exc.read().decode("utf-8", errors="replace")
            raise TronGridError(f"TronGrid HTTP {exc.code}: {body}") from exc
        except (urllib.error.URLError, TimeoutError) as exc:
            raise TronGridError(f"TronGrid network error: {exc}") from exc

    def fetch_trc20_transfers(
        self,
        address: str,
        *,
        contract_address: str,
        min_timestamp_ms: int,
        limit: int = 200,
    ) -> list[dict[str, Any]]:
        path = f"/v1/accounts/{urllib.parse.quote(address)}/transactions/trc20"
        params: dict[str, Any] = {
            "only_confirmed": "true",
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
