from __future__ import annotations

import os
from dataclasses import dataclass
from datetime import timedelta, timezone, tzinfo
from pathlib import Path
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError


def load_dotenv(path: Path = Path(".env")) -> None:
    if not path.exists():
        return

    for raw_line in path.read_text(encoding="utf-8").splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        key = key.strip()
        value = value.strip().strip('"').strip("'")
        os.environ.setdefault(key, value)


@dataclass(frozen=True)
class Config:
    bot_token: str
    telegram_api_base: str
    db_path: Path
    timezone_name: str
    public_bill_base_url: str | None
    public_bill_url_template: str | None
    public_bill_bot_name: str
    telegram_bot_username: str | None
    trongrid_api_base: str
    trongrid_api_key: str | None
    tron_usdt_contract: str
    tron_poll_interval_seconds: int
    tron_initial_lookback_minutes: int
    poll_timeout: int = 50
    request_timeout: int = 70

    @property
    def timezone(self) -> tzinfo:
        try:
            return ZoneInfo(self.timezone_name)
        except ZoneInfoNotFoundError:
            if self.timezone_name in {"Asia/Shanghai", "Asia/Chongqing", "Asia/Harbin"}:
                return timezone(timedelta(hours=8), name="Asia/Shanghai")
            if self.timezone_name.upper() == "UTC":
                return timezone.utc
            raise


def load_config() -> Config:
    load_dotenv()
    token = os.environ.get("TELEGRAM_BOT_TOKEN", "").strip()
    if not token:
        raise RuntimeError("TELEGRAM_BOT_TOKEN is missing. Put it in .env first.")

    db_path = Path(os.environ.get("BOT_DB_PATH", "data/ledger_bot.db"))
    return Config(
        bot_token=token,
        telegram_api_base=os.environ.get("TELEGRAM_API_BASE", "https://api.telegram.org").rstrip("/"),
        db_path=db_path,
        timezone_name=os.environ.get("BOT_TIMEZONE", "Asia/Shanghai"),
        public_bill_base_url=os.environ.get("PUBLIC_BILL_BASE_URL") or None,
        public_bill_url_template=os.environ.get("PUBLIC_BILL_URL_TEMPLATE") or None,
        public_bill_bot_name=os.environ.get("PUBLIC_BILL_BOT_NAME", "LEDGER_BOT"),
        telegram_bot_username=(os.environ.get("TELEGRAM_BOT_USERNAME") or "").lstrip("@") or None,
        trongrid_api_base=os.environ.get("TRONGRID_API_BASE", "https://api.trongrid.io").rstrip("/"),
        trongrid_api_key=os.environ.get("TRONGRID_API_KEY") or None,
        tron_usdt_contract=os.environ.get("TRON_USDT_CONTRACT", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"),
        tron_poll_interval_seconds=int(os.environ.get("TRON_POLL_INTERVAL_SECONDS", "5")),
        tron_initial_lookback_minutes=int(os.environ.get("TRON_INITIAL_LOOKBACK_MINUTES", "15")),
        poll_timeout=int(os.environ.get("BOT_POLL_TIMEOUT", "50")),
        request_timeout=int(os.environ.get("BOT_REQUEST_TIMEOUT", "70")),
    )
