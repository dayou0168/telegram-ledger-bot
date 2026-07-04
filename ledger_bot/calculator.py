from __future__ import annotations

from dataclasses import dataclass
from decimal import Decimal, DivisionByZero, InvalidOperation, localcontext


class CalculatorError(ValueError):
    pass


@dataclass
class TokenStream:
    tokens: list[str]
    index: int = 0

    def peek(self) -> str | None:
        if self.index >= len(self.tokens):
            return None
        return self.tokens[self.index]

    def pop(self) -> str:
        token = self.peek()
        if token is None:
            raise CalculatorError("Unexpected end of expression")
        self.index += 1
        return token


def calculate_expression(expression: str) -> Decimal:
    tokens = tokenize(expression)
    if not tokens:
        raise CalculatorError("Empty expression")
    stream = TokenStream(tokens)
    with localcontext() as context:
        context.prec = 40
        result = parse_expr(stream)
    if stream.peek() is not None:
        raise CalculatorError("Unexpected token")
    return result


def format_calculation_result(value: Decimal) -> str:
    return format(value, ".14g")


def is_arithmetic_expression(text: str) -> bool:
    stripped = text.strip()
    if not stripped:
        return False
    if any(char not in "0123456789+-*/(). \t" for char in stripped):
        return False
    if not any(char.isdigit() for char in stripped):
        return False
    if not any(op in stripped for op in "+-*/"):
        return False
    return True


def tokenize(expression: str) -> list[str]:
    tokens: list[str] = []
    index = 0
    while index < len(expression):
        char = expression[index]
        if char.isspace():
            index += 1
            continue
        if char in "+-*/()":
            tokens.append(char)
            index += 1
            continue
        if char.isdigit() or char == ".":
            start = index
            dot_count = 0
            while index < len(expression) and (expression[index].isdigit() or expression[index] == "."):
                if expression[index] == ".":
                    dot_count += 1
                    if dot_count > 1:
                        raise CalculatorError("Invalid number")
                index += 1
            token = expression[start:index]
            if token == ".":
                raise CalculatorError("Invalid number")
            tokens.append(token)
            continue
        raise CalculatorError("Invalid character")
    return tokens


def parse_expr(stream: TokenStream) -> Decimal:
    value = parse_term(stream)
    while stream.peek() in {"+", "-"}:
        op = stream.pop()
        right = parse_term(stream)
        value = value + right if op == "+" else value - right
    return value


def parse_term(stream: TokenStream) -> Decimal:
    value = parse_factor(stream)
    while stream.peek() in {"*", "/"}:
        op = stream.pop()
        right = parse_factor(stream)
        if op == "*":
            value *= right
        else:
            if right == 0:
                raise CalculatorError("Division by zero")
            try:
                value /= right
            except (DivisionByZero, InvalidOperation) as exc:
                raise CalculatorError("Invalid division") from exc
    return value


def parse_factor(stream: TokenStream) -> Decimal:
    token = stream.pop()
    if token == "+":
        return parse_factor(stream)
    if token == "-":
        return -parse_factor(stream)
    if token == "(":
        value = parse_expr(stream)
        if stream.pop() != ")":
            raise CalculatorError("Missing closing parenthesis")
        return value
    try:
        return Decimal(token)
    except InvalidOperation as exc:
        raise CalculatorError("Invalid number") from exc

