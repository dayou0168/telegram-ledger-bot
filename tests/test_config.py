from __future__ import annotations

from ledger_bot.config import parse_bool, parse_user_id, parse_user_ids


def test_parse_user_ids_accepts_commas_and_semicolons() -> None:
    assert parse_user_ids("1001, 1002;1001") == frozenset({1001, 1002})


def test_parse_user_ids_ignores_empty_items() -> None:
    assert parse_user_ids(" , ; ") == frozenset()


def test_parse_single_host_user_id() -> None:
    assert parse_user_id("1001") == 1001
    assert parse_user_id(" ") is None


def test_parse_bool() -> None:
    assert parse_bool("1")
    assert parse_bool("true")
    assert parse_bool("on")
    assert not parse_bool("0")
    assert not parse_bool("")
