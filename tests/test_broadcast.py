from __future__ import annotations

from ledger_bot.bot import LedgerBot


def test_extract_notify_all_option() -> None:
    assert LedgerBot.extract_notify_all_option("通知所有人 广播内容") == (True, "广播内容")
    assert LedgerBot.extract_notify_all_option("广播内容 通知所有人") == (True, "广播内容")
    assert LedgerBot.extract_notify_all_option("广播内容") == (False, "广播内容")


def test_broadcast_preview_for_photo_caption() -> None:
    message = {"message_id": 10, "photo": [{"file_id": "abc"}], "caption": "活动通知"}

    assert LedgerBot.broadcast_message_kind(message) == "photo"
    assert LedgerBot.broadcast_preview(message) == "[图片] 活动通知"
    assert LedgerBot.is_broadcastable_message(message)
