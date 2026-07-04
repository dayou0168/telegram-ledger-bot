from __future__ import annotations

from dataclasses import dataclass
from decimal import Decimal, InvalidOperation
import json
from typing import Any
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode
from urllib.request import Request, urlopen


class P2PRateError(Exception):
    pass


@dataclass(frozen=True)
class P2PMethodRate:
    market: str
    fiat_unit: str
    asset: str
    method_id: str
    method_name: str
    price_type: str
    side: str
    price: Decimal
    buy_price: Decimal | None
    sell_price: Decimal | None
    activity_24h: int | None
    buy_ads: int | None
    sell_ads: int | None
    updated_at_minutes: int | None


@dataclass(frozen=True)
class P2POrderBookEntry:
    rank: int
    price: Decimal
    merchant_name: str
    methods: tuple[str, ...]
    limit_min: Decimal | None
    limit_max: Decimal | None
    surplus_amount: Decimal | None
    orders: int | None
    good_reviews_percent: Decimal | None
    updated_at_ts: int | None


class P2PRateClient:
    def __init__(
        self,
        *,
        api_base: str,
        front_api: str,
        request_timeout: int,
    ):
        self.api_base = api_base.rstrip("/")
        self.front_api = front_api
        self.request_timeout = request_timeout

    def fetch_method_rate(
        self,
        *,
        market: str,
        fiat_unit: str,
        asset: str,
        method_id: str,
        price_type: str,
        side: str,
    ) -> P2PMethodRate:
        params = {
            "market": market,
            "fiatUnit": fiat_unit,
            "asset": asset,
            "price_type": price_type,
        }
        url = f"{self.api_base}/p2p/prices/table?{urlencode(params)}"
        request = Request(
            url,
            headers={
                "Accept": "application/json",
                "Content-Type": "application/json",
                "User-Agent": "Mozilla/5.0",
                "X-Requested-With": "XMLHttpRequest",
                "X-FRONT-API": self.front_api,
                "X-Lang": "en",
            },
        )
        try:
            with urlopen(request, timeout=self.request_timeout) as response:
                payload = json.loads(response.read().decode("utf-8"))
        except HTTPError as exc:
            raise P2PRateError(f"报价源返回 HTTP {exc.code}") from exc
        except (OSError, URLError, json.JSONDecodeError) as exc:
            raise P2PRateError(f"报价源请求失败：{exc}") from exc

        return parse_p2p_rate_table(
            payload,
            market=market,
            fiat_unit=fiat_unit,
            asset=asset,
            method_id=method_id,
            price_type=price_type,
            side=side,
        )

    def fetch_order_book_top(
        self,
        *,
        market: str,
        fiat_unit: str,
        asset: str,
        limit: int,
        trade_methods: list[str] | None = None,
    ) -> list[P2POrderBookEntry]:
        trade_methods = trade_methods or []
        body = {
            "fiatUnit": fiat_unit,
            "market1": market,
            "market2": market,
            "asset1": asset,
            "asset2": asset,
            "tradeMethods1": trade_methods,
            "tradeMethods2": trade_methods,
            # In P2P Army's order-book terminology, SELL means makers are
            # selling USDT, so the visitor is buying USDT with fiat.
            "tradeType1": "SELL",
            "tradeType2": "SELL",
            "amount1": "",
            "amount2": "",
            "only_merchants1": False,
            "only_merchants2": False,
            "only_merchants_pro1": False,
            "only_merchants_pro2": False,
            "user_orders1from": "",
            "user_orders1to": "",
            "user_orders2from": "",
            "user_orders2to": "",
            "user_reviews1from": "",
            "user_reviews1to": "",
            "user_reviews2from": "",
            "user_reviews2to": "",
            "price1from": "",
            "price1to": "",
            "price2from": "",
            "price2to": "",
            "limit": limit,
        }
        request = Request(
            f"{self.api_base}/p2p/order-book/monitoring",
            data=json.dumps(body).encode("utf-8"),
            headers={
                "Accept": "application/json",
                "Content-Type": "application/json",
                "User-Agent": "Mozilla/5.0",
                "X-Requested-With": "XMLHttpRequest",
                "X-FRONT-API": self.front_api,
                "X-Lang": "en",
            },
            method="POST",
        )
        try:
            with urlopen(request, timeout=self.request_timeout) as response:
                payload = json.loads(response.read().decode("utf-8"))
        except HTTPError as exc:
            raise P2PRateError(f"订单簿源返回 HTTP {exc.code}") from exc
        except (OSError, URLError, json.JSONDecodeError) as exc:
            raise P2PRateError(f"订单簿请求失败：{exc}") from exc

        return parse_order_book_top(payload, limit=limit)


def parse_p2p_rate_table(
    payload: dict[str, Any],
    *,
    market: str,
    fiat_unit: str,
    asset: str,
    method_id: str,
    price_type: str,
    side: str,
) -> P2PMethodRate:
    side = side.lower()
    if side not in {"buy", "sell"}:
        raise P2PRateError("P2P_RATE_SIDE 只能是 buy 或 sell")

    for item in payload.get("items", []):
        method = item.get("method") or {}
        if str(method.get("id", "")).lower() != method_id.lower():
            continue

        buy_price = _price_at(item, "buy")
        sell_price = _price_at(item, "sell")
        selected = buy_price if side == "buy" else sell_price
        if selected is None:
            raise P2PRateError(f"{method.get('name') or method_id} 暂无 {side} 报价")

        ads_count = item.get("ads_count") or {}
        return P2PMethodRate(
            market=market,
            fiat_unit=fiat_unit,
            asset=asset,
            method_id=str(method.get("id") or method_id),
            method_name=str(method.get("name") or method_id),
            price_type=price_type,
            side=side,
            price=selected,
            buy_price=buy_price,
            sell_price=sell_price,
            activity_24h=_optional_int(item.get("activity_24h")),
            buy_ads=_optional_int(ads_count.get("buy")),
            sell_ads=_optional_int(ads_count.get("sell")),
            updated_at_minutes=_optional_int(item.get("updated_at")),
        )

    raise P2PRateError(f"报价源没有返回 {method_id} 支付方式")


def parse_order_book_top(payload: dict[str, Any], *, limit: int) -> list[P2POrderBookEntry]:
    if not payload.get("success", True):
        raise P2PRateError("订单簿源返回失败")

    entries: list[P2POrderBookEntry] = []
    for item in payload.get("data", []):
        buy = item.get("buy") or {}
        price = _decimal_or_none(buy.get("price"))
        if price is None:
            continue

        user = buy.get("user") or {}
        entries.append(
            P2POrderBookEntry(
                rank=int(item.get("pos") or len(entries) + 1),
                price=price,
                merchant_name=str(user.get("nickname") or "-"),
                methods=tuple(str(method) for method in buy.get("trade_methods") or []),
                limit_min=_decimal_or_none(buy.get("limit_min")),
                limit_max=_decimal_or_none(buy.get("limit_max")),
                surplus_amount=_decimal_or_none(buy.get("surplus_amount")),
                orders=_optional_int(user.get("orders")),
                good_reviews_percent=_decimal_or_none(user.get("good_reviews_percent")),
                updated_at_ts=_optional_int(buy.get("updated_at_ts") or buy.get("ts")),
            )
        )
        if len(entries) >= limit:
            break

    if not entries:
        raise P2PRateError("订单簿没有返回可用买 U 报价")
    return entries


def _price_at(item: dict[str, Any], side: str) -> Decimal | None:
    side_data = item.get(side) or {}
    value = side_data.get("price")
    if value is None:
        prices = side_data.get("prices") or []
        value = prices[0] if prices else None
    if value is None:
        return None
    return _decimal_or_none(value)


def _decimal_or_none(value: Any) -> Decimal | None:
    if value is None or value == "":
        return None
    try:
        return Decimal(str(value))
    except InvalidOperation as exc:
        raise P2PRateError(f"数字格式异常：{value}") from exc


def _optional_int(value: Any) -> int | None:
    if value is None:
        return None
    try:
        return int(value)
    except (TypeError, ValueError):
        return None
