from decimal import Decimal

from ledger_bot.bot import calculate_amounts


def test_screenshot_style_deposit_conversion() -> None:
    amount_cny, amount_usdt, net_usdt, commission = calculate_amounts(
        kind="deposit",
        amount=Decimal("7000"),
        currency="CNY",
        rate=Decimal("11.5"),
        fee_rate=Decimal("0"),
        payout_mode="cny",
        multiply_exchange=False,
    )
    assert amount_cny == Decimal("7000.000000")
    assert amount_usdt == Decimal("608.695652")
    assert net_usdt == Decimal("608.695652")
    assert commission == Decimal("0.000000")


def test_multiplier_is_applied_before_conversion() -> None:
    amount_cny, amount_usdt, net_usdt, _commission = calculate_amounts(
        kind="deposit",
        amount=Decimal("50000"),
        currency="CNY",
        rate=Decimal("10"),
        fee_rate=Decimal("0"),
        payout_mode="cny",
        multiply_exchange=False,
    )
    assert amount_cny == Decimal("50000.000000")
    assert amount_usdt == Decimal("5000.000000")
    assert net_usdt == Decimal("5000.000000")


def test_fee_keeps_gross_and_net_separate() -> None:
    amount_cny, amount_usdt, net_usdt, commission = calculate_amounts(
        kind="deposit",
        amount=Decimal("1000"),
        currency="CNY",
        rate=Decimal("10"),
        fee_rate=Decimal("3"),
        payout_mode="cny",
        multiply_exchange=False,
    )
    assert amount_cny == Decimal("1000.000000")
    assert amount_usdt == Decimal("100.000000")
    assert net_usdt == Decimal("97.000000")
    assert commission == Decimal("30.000000")


def test_payout_cny_conversion_stays_cny_even_in_coin_mode() -> None:
    amount_cny, amount_usdt, net_usdt, commission = calculate_amounts(
        kind="payout",
        amount=Decimal("100"),
        currency="CNY",
        rate=Decimal("10"),
        fee_rate=Decimal("0"),
        payout_mode="coin",
        multiply_exchange=False,
    )
    assert amount_cny == Decimal("100.000000")
    assert amount_usdt == Decimal("10.000000")
    assert net_usdt == Decimal("10.000000")
    assert commission == Decimal("0.000000")


def test_payout_with_u_suffix_is_usdt() -> None:
    amount_cny, amount_usdt, net_usdt, commission = calculate_amounts(
        kind="payout",
        amount=Decimal("100"),
        currency="USDT",
        rate=Decimal("10"),
        fee_rate=Decimal("0"),
        payout_mode="cny",
        multiply_exchange=False,
    )
    assert amount_cny == Decimal("1000.000000")
    assert amount_usdt == Decimal("100.000000")
    assert net_usdt == Decimal("100.000000")
    assert commission == Decimal("0.000000")
