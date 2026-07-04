from __future__ import annotations

from decimal import Decimal

from ledger_bot.p2p_rates import build_order_book_monitoring_body, parse_order_book_top


def test_order_book_body_uses_sell_usdt_direction() -> None:
    body = build_order_book_monitoring_body(
        market="okx",
        fiat_unit="CNY",
        asset="USDT",
        trade_methods=["aliPay"],
        limit=10,
    )

    assert body["tradeType1"] == "BUY"
    assert body["tradeType2"] == "BUY"
    assert body["tradeMethods1"] == ["aliPay"]


def test_parse_order_book_top_sell_usdt_entries() -> None:
    payload = {
        "success": True,
        "data": [
            {
                "pos": 1,
                "buy": {
                    "price": 6.73,
                    "limit_min": "99999.00",
                    "limit_max": "161520.00",
                    "surplus_amount": 24000,
                    "updated_at_ts": 1783181279,
                    "trade_methods": ["aliPay"],
                    "user": {
                        "nickname": "商家A",
                        "orders": 385,
                        "good_reviews_percent": 96.25,
                    },
                },
            },
            {
                "pos": 2,
                "buy": {
                    "price": "6.72",
                    "limit_min": "30000.00",
                    "limit_max": "2016000.00",
                    "surplus_amount": "300000",
                    "trade_methods": ["aliPay", "bank"],
                    "user": {
                        "nickname": "商家B",
                        "orders": 243793,
                        "good_reviews_percent": "99.98",
                    },
                },
            },
        ],
    }

    entries = parse_order_book_top(payload, limit=10)

    assert len(entries) == 2
    assert entries[0].rank == 1
    assert entries[0].price == Decimal("6.73")
    assert entries[0].merchant_name == "商家A"
    assert entries[0].methods == ("aliPay",)
    assert entries[0].limit_min == Decimal("99999.00")
    assert entries[1].price == Decimal("6.72")
    assert entries[1].methods == ("aliPay", "bank")
