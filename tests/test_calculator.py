from decimal import Decimal

from ledger_bot.calculator import calculate_expression, format_calculation_result, is_arithmetic_expression


def test_reference_division() -> None:
    result = calculate_expression("1000/6.8")
    assert format_calculation_result(result) == "147.05882352941"


def test_reference_subtract_two_divisions() -> None:
    result = calculate_expression("1000/7.5-1000/6.8")
    assert format_calculation_result(result) == "-13.725490196078"


def test_operator_precedence_and_parentheses() -> None:
    assert calculate_expression("(2+3)*4") == Decimal("20")


def test_expression_detection() -> None:
    assert is_arithmetic_expression("1000/6.8")
    assert is_arithmetic_expression("1000/7.5-1000/6.8")
    assert not is_arithmetic_expression("显示账单")
    assert not is_arithmetic_expression("1000")
