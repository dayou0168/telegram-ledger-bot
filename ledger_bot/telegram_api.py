from __future__ import annotations

import json
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from typing import Any


RETRYABLE_STATUS = {500, 502, 503, 504}


class TelegramAPIError(RuntimeError):
    pass


class TelegramRetryableError(TelegramAPIError):
    pass


@dataclass(frozen=True)
class TelegramClient:
    token: str
    api_base: str = "https://api.telegram.org"
    request_timeout: int = 70

    def request(self, method: str, payload: dict[str, Any] | None = None) -> Any:
        url = f"{self.api_base.rstrip('/')}/bot{self.token}/{method}"
        body = json.dumps(payload or {}).encode("utf-8")
        request = urllib.request.Request(
            url,
            data=body,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        try:
            with urllib.request.urlopen(request, timeout=self.request_timeout) as response:
                data = json.loads(response.read().decode("utf-8"))
        except urllib.error.HTTPError as exc:
            if exc.code in RETRYABLE_STATUS:
                raise TelegramRetryableError(f"Telegram HTTP {exc.code}") from exc
            details = exc.read().decode("utf-8", errors="replace")
            raise TelegramAPIError(f"Telegram HTTP {exc.code}: {details}") from exc
        except (urllib.error.URLError, TimeoutError) as exc:
            raise TelegramRetryableError(f"Telegram network error: {exc}") from exc

        if not data.get("ok"):
            description = data.get("description", "unknown Telegram error")
            raise TelegramAPIError(description)
        return data.get("result")

    def get_updates(self, offset: int | None, timeout: int) -> list[dict[str, Any]]:
        payload: dict[str, Any] = {
            "timeout": timeout,
            "allowed_updates": ["message", "callback_query", "my_chat_member"],
        }
        if offset is not None:
            payload["offset"] = offset
        return self.request("getUpdates", payload)

    def send_message(
        self,
        chat_id: int,
        text: str,
        *,
        reply_to_message_id: int | None = None,
        reply_markup: dict[str, Any] | None = None,
        disable_web_page_preview: bool = True,
        parse_mode: str | None = None,
    ) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "chat_id": chat_id,
            "text": text,
            "disable_web_page_preview": disable_web_page_preview,
        }
        if parse_mode:
            payload["parse_mode"] = parse_mode
        if reply_to_message_id:
            payload["reply_to_message_id"] = reply_to_message_id
            payload["allow_sending_without_reply"] = True
        if reply_markup:
            payload["reply_markup"] = reply_markup
        return self.request("sendMessage", payload)

    def answer_callback_query(self, callback_query_id: str, text: str | None = None) -> None:
        payload: dict[str, Any] = {"callback_query_id": callback_query_id}
        if text:
            payload["text"] = text
        self.request("answerCallbackQuery", payload)

    def set_chat_permissions(self, chat_id: int, can_send_messages: bool) -> None:
        permissions = {
            "can_send_messages": can_send_messages,
            "can_send_audios": can_send_messages,
            "can_send_documents": can_send_messages,
            "can_send_photos": can_send_messages,
            "can_send_videos": can_send_messages,
            "can_send_video_notes": can_send_messages,
            "can_send_voice_notes": can_send_messages,
            "can_send_polls": can_send_messages,
            "can_send_other_messages": can_send_messages,
            "can_add_web_page_previews": can_send_messages,
        }
        self.request("setChatPermissions", {"chat_id": chat_id, "permissions": permissions})

    def pin_chat_message(self, chat_id: int, message_id: int) -> None:
        self.request(
            "pinChatMessage",
            {
                "chat_id": chat_id,
                "message_id": message_id,
                "disable_notification": True,
            },
        )

    def leave_chat(self, chat_id: int) -> None:
        self.request("leaveChat", {"chat_id": chat_id})


def run_with_backoff(fn, *, base_sleep: int = 5, max_sleep: int = 300) -> None:
    sleep_seconds = base_sleep
    while True:
        try:
            fn()
            sleep_seconds = base_sleep
        except TelegramRetryableError as exc:
            print(f"{exc}; retrying in {sleep_seconds}s", flush=True)
            time.sleep(sleep_seconds)
            sleep_seconds = min(max_sleep, sleep_seconds * 2)
