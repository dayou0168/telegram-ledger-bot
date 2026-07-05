from __future__ import annotations

import re
from dataclasses import dataclass
from decimal import Decimal, InvalidOperation


NUMBER = r"[+-]?\d+(?:\.\d+)?"


@dataclass(frozen=True)
class ParsedCommand:
    name: str
    args: dict


@dataclass(frozen=True)
class ParsedLedgerEntry:
    kind: str
    amount: Decimal
    currency: str
    multiplier: Decimal = Decimal("1")
    exchange_rate: Decimal | None = None
    fee_rate: Decimal | None = None
    subject: str | None = None
    note: str | None = None
    is_balance: bool = False


ParsedMessage = ParsedCommand | ParsedLedgerEntry | None


def parse_message(text: str) -> ParsedMessage:
    normalized = re.sub(r"\s+", " ", text.strip())
    if not normalized:
        return None

    exact_commands = {
        "开始": "start",
        "停止": "stop",
        "关闭": "stop",
        "上课": "open_business",
        "下课": "close_business",
        "+0": "show_compact_bill",
        "显示账单": "show_bill",
        "账单": "show_bill",
        "/我": "show_my_bill",
        "完整模式": "full_mode",
        "撤销": "undo_last",
        "撤销入款": "undo_deposit",
        "撤销下发": "undo_payout",
        "清除今日账单": "clear_today",
        "清除全部账单": "clear_all",
        "删除账单": "clear_all",
        "保存账单": "save_bill",
        "到期时间": "expires_at",
        "管理员": "show_operators",
        "权限人": "show_operators",
        "显示操作员": "show_operators",
        "记账置顶开启": "pin_on",
        "开启记账置顶": "pin_on",
        "记账置顶关闭": "pin_off",
        "关闭记账置顶": "pin_off",
        "设置实时汇率": "realtime_rate_on",
        "设置下发人民币模式": "set_payout_cny_mode",
        "设置下发币模式": "set_payout_coin_mode",
        "开启乘汇率模式": "multiply_exchange_on",
        "关闭乘汇率模式": "multiply_exchange_off",
        "显示人民币": "show_cny_on",
        "隐藏人民币": "show_cny_off",
        "关闭日切": "cutoff_off",
        "取消日切": "cutoff_off",
        "日切关闭": "cutoff_off",
        "关闭全局日切": "global_cutoff_off",
        "设置所有人": "all_members_on",
        "取消所有人": "all_members_off",
        "汇率": "rate_query",
        "机器人退群": "leave_group",
        "通知所有人": "notify_all",
        "OTC": "otc",
        "币价": "otc",
    }
    if normalized in exact_commands:
        return ParsedCommand(exact_commands[normalized], {})

    if match := re.fullmatch(r"[zZ](\d{1,2})(?:\s*([+-]?\d+(?:\.\d+)?))?", normalized):
        rank = int(match.group(1))
        if rank == 0:
            return ParsedCommand("otc", {})
        if 1 <= rank <= 10:
            offset = _decimal(match.group(2)) if match.group(2) else Decimal("0")
            return ParsedCommand("set_rate_from_otc_rank", {"rank": rank, "offset": offset})
        return None

    if match := re.fullmatch(r"设置(?:入款)?费率\s*(%s)\s*%%?" % NUMBER, normalized):
        return ParsedCommand("set_deposit_fee_rate", {"fee_rate": _decimal(match.group(1))})

    if match := re.fullmatch(r"设置下发费率\s*(%s)\s*%%?" % NUMBER, normalized):
        return ParsedCommand("set_payout_fee_rate", {"fee_rate": _decimal(match.group(1))})

    if match := re.fullmatch(r"设置(?:入款)?汇率\s*[zZ](\d{1,2})(?:\s*([+-]?\d+(?:\.\d+)?))?", normalized):
        rank = int(match.group(1))
        if 1 <= rank <= 10:
            offset = _decimal(match.group(2)) if match.group(2) else Decimal("0")
            return ParsedCommand("set_rate_from_otc_rank", {"rank": rank, "offset": offset})
        return None

    if match := re.fullmatch(r"设置(?:入款)?汇率\s*(%s)" % NUMBER, normalized):
        return ParsedCommand("set_deposit_exchange_rate", {"exchange_rate": _decimal(match.group(1))})

    if match := re.fullmatch(r"设置下发汇率\s*(%s)" % NUMBER, normalized):
        return ParsedCommand("set_payout_exchange_rate", {"exchange_rate": _decimal(match.group(1))})

    if match := re.fullmatch(r"设置实时汇率\s*(%s)" % NUMBER, normalized):
        return ParsedCommand("realtime_rate_on", {"offset": _decimal(match.group(1))})

    if match := re.fullmatch(r"设置代付价格\s*(%s)" % NUMBER, normalized):
        return ParsedCommand("set_payout_price", {"price": _decimal(match.group(1))})

    if match := re.fullmatch(r"设置币种\s*([A-Za-z]{2,8})", normalized):
        return ParsedCommand("set_currency", {"currency": match.group(1).upper()})

    if match := re.fullmatch(r"设置日切\s*(-?\d{1,2})", normalized):
        return ParsedCommand("set_cutoff", {"hour": int(match.group(1))})

    if match := re.fullmatch(r"(?:简洁模式|显示条数)\s*(\d{1,3})", normalized):
        return ParsedCommand("simple_mode", {"limit": int(match.group(1))})

    if match := re.fullmatch(r"修改汇款\s*(%s)" % NUMBER, normalized):
        return ParsedCommand("modify_exchange_for_bill", {"exchange_rate": _decimal(match.group(1))})

    if match := re.fullmatch(r"(添加操作员|删除操作员|设置操作人|移除操作人)(?:\s*(.*))?", normalized):
        name = "add_operator" if match.group(1) in {"添加操作员", "设置操作人"} else "remove_operator"
        mentions = _extract_mentions(match.group(2) or "")
        return ParsedCommand(name, {"mentions": mentions})

    if normalized in {"拉停"}:
        return ParsedCommand("stop_and_close_business", {})

    if re.fullmatch(r"(?:查汇率|查金价|今日金价|G|Y0|H0|m0)", normalized):
        return ParsedCommand("external_query", {"query": normalized})

    if re.fullmatch(r"(?:查汇率|币价)\s*.+", normalized):
        return ParsedCommand("external_query", {"query": normalized})

    if normalized.startswith("查询"):
        return ParsedCommand("external_query", {"query": normalized[2:].strip()})

    deposit = _parse_deposit(normalized)
    if deposit:
        return deposit

    payout = _parse_payout(normalized)
    if payout:
        return payout

    return None


def _parse_deposit(text: str) -> ParsedLedgerEntry | None:
    match = re.fullmatch(
        r"([+-])\s*(\d+(?:\.\d+)?)([uU])?(?:\s*\*\s*(\d+(?:\.\d+)?)(%)?)?(?:\s*/\s*(\d+(?:\.\d+)?))?(?:\s+(.+))?",
        text,
    )
    if not match:
        return None

    sign = Decimal("-1") if match.group(1) == "-" else Decimal("1")
    amount = sign * _decimal(match.group(2))
    currency = "USDT" if match.group(3) else "CNY"
    star_value = _decimal(match.group(4)) if match.group(4) else None
    multiplier = Decimal("1")
    inline_fee_rate = None
    if star_value is not None and match.group(5):
        inline_fee_rate = star_value
    elif star_value is not None:
        multiplier = star_value
    exchange_rate = _decimal(match.group(6)) if match.group(6) else None
    tail = match.group(7) or ""
    subject, fee_rate, note, is_balance = _parse_tail(tail)
    fee_rate = fee_rate if fee_rate is not None else inline_fee_rate
    return ParsedLedgerEntry(
        kind="deposit",
        amount=amount,
        currency=currency,
        multiplier=multiplier,
        exchange_rate=exchange_rate,
        fee_rate=fee_rate,
        subject=subject,
        note=note,
        is_balance=is_balance,
    )


def _parse_payout(text: str) -> ParsedLedgerEntry | None:
    match = re.fullmatch(
        r"下发\s*(%s)([uU])?(?:\s*\*\s*(\d+(?:\.\d+)?))?(?:\s*/\s*(\d+(?:\.\d+)?))?(?:\s+(.+))?" % NUMBER,
        text,
    )
    if not match:
        return None

    amount = _decimal(match.group(1))
    currency = "USDT" if match.group(2) else "CNY"
    multiplier = _decimal(match.group(3)) if match.group(3) else Decimal("1")
    exchange_rate = _decimal(match.group(4)) if match.group(4) else None
    if currency != "USDT" and multiplier == 1 and exchange_rate is None:
        return None
    subject, _fee_rate, note, is_balance = _parse_tail(match.group(5) or "")
    return ParsedLedgerEntry(
        kind="payout",
        amount=amount,
        currency=currency,
        multiplier=multiplier,
        exchange_rate=exchange_rate,
        subject=subject,
        note=note,
        is_balance=is_balance,
    )


def _parse_tail(tail: str) -> tuple[str | None, Decimal | None, str | None, bool]:
    if not tail.strip():
        return None, None, None, False

    tokens = tail.split()
    kept: list[str] = []
    note_tokens: list[str] = []
    fee_rate: Decimal | None = None
    is_balance = False

    for token in tokens:
        if token == "余额":
            is_balance = True
            note_tokens.append(token)
            continue
        fee = _parse_fee_token(token)
        if fee is not None:
            fee_rate = fee
            continue
        kept.append(token)

    subject = " ".join(kept).strip() or None
    note = " ".join(note_tokens).strip() or None
    return subject, fee_rate, note, is_balance


def _parse_fee_token(token: str) -> Decimal | None:
    match = re.fullmatch(r"(?:费率)?(\d+(?:\.\d+)?)%?", token)
    if not match:
        return None
    return _decimal(match.group(1))


def _extract_mentions(text: str) -> list[str]:
    return [match.group(1).lower() for match in re.finditer(r"@([A-Za-z0-9_]{3,32})", text)]


def _decimal(value: str) -> Decimal:
    try:
        return Decimal(value)
    except InvalidOperation as exc:
        raise ValueError(f"Invalid decimal: {value}") from exc
