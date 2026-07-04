from decimal import Decimal

from ledger_bot.parser import ParsedCommand, ParsedLedgerEntry, parse_message


def test_deposit_with_rate() -> None:
    parsed = parse_message("+7000/11.5")
    assert isinstance(parsed, ParsedLedgerEntry)
    assert parsed.kind == "deposit"
    assert parsed.amount == Decimal("7000")
    assert parsed.exchange_rate == Decimal("11.5")


def test_deposit_with_multiplier_and_rate() -> None:
    parsed = parse_message("+10000*5/7.1")
    assert isinstance(parsed, ParsedLedgerEntry)
    assert parsed.multiplier == Decimal("5")
    assert parsed.exchange_rate == Decimal("7.1")


def test_deposit_with_inline_fee() -> None:
    parsed = parse_message("+1000*12%")
    assert isinstance(parsed, ParsedLedgerEntry)
    assert parsed.fee_rate == Decimal("12")
    assert parsed.multiplier == Decimal("1")


def test_payout_with_multiplier_and_rate() -> None:
    parsed = parse_message("下发5000*5/7.1")
    assert isinstance(parsed, ParsedLedgerEntry)
    assert parsed.kind == "payout"
    assert parsed.multiplier == Decimal("5")
    assert parsed.exchange_rate == Decimal("7.1")


def test_close_alias() -> None:
    parsed = parse_message("关闭")
    assert isinstance(parsed, ParsedCommand)
    assert parsed.name == "stop"


def test_plus_zero_shows_bill() -> None:
    parsed = parse_message("+0")
    assert isinstance(parsed, ParsedCommand)
    assert parsed.name == "show_compact_bill"


def test_operator_mentions() -> None:
    parsed = parse_message("设置操作人@alice @Bob_123")
    assert isinstance(parsed, ParsedCommand)
    assert parsed.name == "add_operator"
    assert parsed.args["mentions"] == ["alice", "bob_123"]


def test_cutoff_off_commands() -> None:
    for text in ["关闭日切", "取消日切", "日切关闭"]:
        parsed = parse_message(text)
        assert isinstance(parsed, ParsedCommand)
        assert parsed.name == "cutoff_off"


def test_set_cutoff_minus_one() -> None:
    parsed = parse_message("设置日切-1")
    assert isinstance(parsed, ParsedCommand)
    assert parsed.name == "set_cutoff"
    assert parsed.args["hour"] == -1


def test_z0_shows_otc_top() -> None:
    parsed = parse_message("Z0")
    assert isinstance(parsed, ParsedCommand)
    assert parsed.name == "otc"


def test_z_rank_sets_rate_with_offset() -> None:
    parsed = parse_message("z1 -0.1")
    assert isinstance(parsed, ParsedCommand)
    assert parsed.name == "set_rate_from_otc_rank"
    assert parsed.args["rank"] == 1
    assert parsed.args["offset"] == Decimal("-0.1")

    parsed = parse_message("Z10 +0.1")
    assert isinstance(parsed, ParsedCommand)
    assert parsed.name == "set_rate_from_otc_rank"
    assert parsed.args["rank"] == 10
    assert parsed.args["offset"] == Decimal("0.1")

    parsed = parse_message("设置汇率 Z1")
    assert isinstance(parsed, ParsedCommand)
    assert parsed.name == "set_rate_from_otc_rank"
    assert parsed.args["rank"] == 1
    assert parsed.args["offset"] == Decimal("0")

    parsed = parse_message("设置汇率 Z1 -0")
    assert isinstance(parsed, ParsedCommand)
    assert parsed.name == "set_rate_from_otc_rank"
    assert parsed.args["rank"] == 1
    assert parsed.args["offset"] == Decimal("-0")
