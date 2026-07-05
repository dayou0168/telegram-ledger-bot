from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime, timedelta, tzinfo
from decimal import Decimal, InvalidOperation, ROUND_HALF_UP
from html import escape
import json
import re
import time
from typing import Any
from urllib.parse import urlencode

from .address_watch import format_transfer_notice, min_notify_amount, should_notify_transfer
from .calculator import CalculatorError, calculate_expression, format_calculation_result, is_arithmetic_expression
from .config import Config
from .p2p_rates import P2POrderBookEntry, P2PRateClient, P2PRateError
from .parser import ParsedCommand, ParsedLedgerEntry, parse_message
from .storage import (
    Storage,
    TelegramUser,
    bill_window_for_day,
    business_day_key,
    current_business_day_key,
    user_from_telegram,
)
from .telegram_api import TelegramAPIError, TelegramClient, TelegramRetryableError, run_with_backoff
from .tron_api import (
    TronGridClient,
    TronGridError,
    format_tron_address_query,
    parse_tron_address_info,
    parse_usdt_transfer,
)


MANAGEMENT_COMMANDS = {
    "start",
    "stop",
    "open_business",
    "close_business",
    "stop_and_close_business",
    "add_operator",
    "remove_operator",
    "set_deposit_fee_rate",
    "set_payout_fee_rate",
    "set_deposit_exchange_rate",
    "set_payout_exchange_rate",
    "set_cutoff",
    "cutoff_off",
    "global_cutoff_off",
    "simple_mode",
    "full_mode",
    "clear_today",
    "clear_all",
    "pin_on",
    "pin_off",
    "realtime_rate_on",
    "set_currency",
    "set_payout_cny_mode",
    "set_payout_coin_mode",
    "multiply_exchange_on",
    "multiply_exchange_off",
    "show_cny_on",
    "show_cny_off",
    "all_members_on",
    "all_members_off",
    "modify_exchange_for_bill",
    "set_rate_from_otc_rank",
    "save_bill",
    "leave_group",
}


@dataclass
class MessageContext:
    message: dict[str, Any]
    chat_id: int
    chat_title: str | None
    user: TelegramUser
    text: str
    now: datetime

    @property
    def message_id(self) -> int:
        return int(self.message["message_id"])

    @property
    def reply_user(self) -> TelegramUser | None:
        reply = self.message.get("reply_to_message")
        if not reply or "from" not in reply:
            return None
        return user_from_telegram(reply["from"])

    @property
    def reply_message_id(self) -> int | None:
        reply = self.message.get("reply_to_message")
        if not reply:
            return None
        return int(reply["message_id"])


class LedgerBot:
    def __init__(self, config: Config):
        self.config = config
        self.storage = Storage(config.db_path)
        self.client = TelegramClient(
            token=config.bot_token,
            api_base=config.telegram_api_base,
            request_timeout=config.request_timeout,
        )
        self.tron_client = TronGridClient(
            api_base=config.trongrid_api_base,
            api_key=config.trongrid_api_key,
            request_timeout=config.request_timeout,
        )
        self.p2p_rate_client = P2PRateClient(
            api_base=config.p2p_rate_api_base,
            front_api=config.p2p_rate_front_api,
            request_timeout=config.request_timeout,
        )
        last_update_id = self.storage.last_processed_update_id()
        self.offset: int | None = last_update_id + 1 if last_update_id is not None else None
        self.next_tron_poll_at = 0.0
        self.address_watch_pending: dict[int, str] = {}

    def run_forever(self) -> None:
        print("Ledger bot is running.", flush=True)
        run_with_backoff(self._poll_once)

    def _poll_once(self) -> None:
        updates = self.client.get_updates(self.offset, self.telegram_poll_timeout())
        for update in updates:
            update_id = int(update["update_id"])
            self.offset = update_id + 1
            if not self.storage.claim_update(update_id, datetime.now(self.config.timezone)):
                continue
            try:
                self.handle_update(update)
            except TelegramAPIError as exc:
                print(f"Telegram API error: {exc}", flush=True)
            except Exception as exc:
                print(f"Unhandled update error: {exc}", flush=True)
        self.poll_address_watches_if_due()

    def telegram_poll_timeout(self) -> int:
        if self.config.tron_poll_interval_seconds <= 0:
            return self.config.poll_timeout
        return min(self.config.poll_timeout, max(1, self.config.tron_poll_interval_seconds))

    def handle_update(self, update: dict[str, Any]) -> None:
        if "message" in update:
            self.handle_message(update["message"])
        elif "callback_query" in update:
            self.handle_callback(update["callback_query"])
        elif "my_chat_member" in update:
            self.handle_my_chat_member(update["my_chat_member"])

    def handle_my_chat_member(self, update: dict[str, Any]) -> None:
        chat = update.get("chat") or {}
        actor = update.get("from")
        if not actor or chat.get("type") not in {"group", "supergroup"}:
            return
        now = datetime.now(self.config.timezone)
        chat_id = int(chat["id"])
        new_status = (update.get("new_chat_member") or {}).get("status")
        if new_status in {"member", "administrator", "restricted"}:
            inviter = user_from_telegram(actor)
            self.storage.touch_user(chat_id, inviter, now)
            if not self.can_invite_bot(inviter):
                try:
                    self.client.send_message(chat_id, "邀请人没有授权，机器人将自动退出。")
                except TelegramAPIError:
                    pass
                self.client.leave_chat(chat_id)
                self.storage.forget_group(chat_id)
                return
            self.storage.ensure_group(chat_id, chat.get("title"), now)
            if not self.ensure_host_present_or_leave(chat_id, chat.get("title"), inviter, now):
                return
            return
        if new_status in {"left", "kicked"}:
            return
        self.storage.ensure_group(chat_id, chat.get("title"), now)

    def handle_message(self, message: dict[str, Any]) -> None:
        text = (message.get("text") or message.get("caption") or "").strip()

        chat = message["chat"]
        chat_type = chat.get("type")
        user = user_from_telegram(message["from"])
        now = datetime.now(self.config.timezone)

        if chat_type == "private":
            self.handle_private_message(
                message=message,
                chat_id=int(chat["id"]),
                user=user,
                text=text,
                now=now,
            )
            return

        if chat_type not in {"group", "supergroup"}:
            return

        ctx = MessageContext(
            message=message,
            chat_id=int(chat["id"]),
            chat_title=chat.get("title"),
            user=user,
            text=text,
            now=now,
        )
        self.storage.ensure_group(ctx.chat_id, ctx.chat_title, now)
        self.storage.apply_due_day_cutoff(ctx.chat_id, now, self.config.timezone)
        self.storage.touch_user(ctx.chat_id, user, now)
        if ctx.reply_user:
            self.storage.touch_user(ctx.chat_id, ctx.reply_user, now)
        if not self.ensure_host_present_or_leave(ctx.chat_id, ctx.chat_title, user, now):
            return

        if not text:
            return

        if self.handle_group_address_verification(ctx):
            return

        parsed = parse_message(text)
        if parsed is None:
            if is_arithmetic_expression(text):
                try:
                    result = calculate_expression(text)
                except CalculatorError:
                    return
                self.client.send_message(
                    ctx.chat_id,
                    f"{text.strip()}={format_calculation_result(result)}",
                )
            return

        if isinstance(parsed, ParsedCommand):
            self.handle_command(ctx, parsed)
        else:
            self.handle_ledger_entry(ctx, parsed)

    def handle_command(self, ctx: MessageContext, command: ParsedCommand) -> None:
        group = self.storage.get_group(ctx.chat_id)
        if command.name in MANAGEMENT_COMMANDS:
            if not self.require_operator(ctx):
                return

        match command.name:
            case "start":
                self.storage.activate_group(ctx.chat_id, ctx.now)
                self.client.send_message(
                    ctx.chat_id,
                    "机器人已开启，请开始记账",
                    reply_markup={"inline_keyboard": [[{"text": "🚩 使用说明", "callback_data": "help"}]]},
                )
            case "stop":
                self.storage.update_group(ctx.chat_id, ctx.now, active=0)
                self.reply(ctx, "已停止记账。发送“开始”可重新开启。")
            case "open_business":
                self.storage.update_group(ctx.chat_id, ctx.now, business_open=1)
                self._set_permissions(ctx, True)
            case "close_business":
                self.storage.update_group(ctx.chat_id, ctx.now, business_open=0)
                self._set_permissions(ctx, False)
            case "stop_and_close_business":
                self.storage.update_group(ctx.chat_id, ctx.now, active=0, business_open=0)
                self._set_permissions(ctx, False, prefix="已拉停：记账暂停，群员发言已关闭。")
            case "show_bill":
                self.send_bill(ctx)
            case "show_compact_bill":
                self.send_bill(ctx, compact=True)
            case "show_my_bill":
                self.send_bill(ctx, only_user_id=ctx.user.user_id, compact=True)
            case "set_deposit_fee_rate":
                self.storage.update_group(ctx.chat_id, ctx.now, deposit_fee_rate=str(command.args["fee_rate"]))
                self.reply(ctx, f"费率设置成功，当前交易费率为：{format_number(command.args['fee_rate'])}%")
            case "set_payout_fee_rate":
                self.storage.update_group(ctx.chat_id, ctx.now, payout_fee_rate=str(command.args["fee_rate"]))
                self.reply(ctx, f"下发费率已设置为 {format_number(command.args['fee_rate'])}%。")
            case "set_deposit_exchange_rate":
                self.storage.update_group(
                    ctx.chat_id,
                    ctx.now,
                    deposit_exchange_rate=str(command.args["exchange_rate"]),
                    realtime_rate=0,
                    realtime_rate_rank=None,
                    realtime_rate_offset="0",
                )
                self.reply(ctx, f"固定汇率设置成功，当前固定汇率为： {format_number(command.args['exchange_rate'])}")
            case "set_payout_exchange_rate":
                self.storage.update_group(ctx.chat_id, ctx.now, payout_exchange_rate=str(command.args["exchange_rate"]))
                self.reply(ctx, f"下发汇率已设置为 {format_number(command.args['exchange_rate'])}。")
            case "set_cutoff":
                hour = int(command.args["hour"])
                if hour != -1 and not 0 <= hour <= 23:
                    self.reply(ctx, "日切时间只能是 0 到 23，或 -1 关闭日切。")
                    return
                group = self.storage.set_day_cutoff_hour(ctx.chat_id, ctx.now, hour, self.config.timezone)
                self.reply(ctx, self.cutoff_set_reply(group, hour))
            case "cutoff_off" | "global_cutoff_off":
                self.storage.set_day_cutoff_hour(ctx.chat_id, ctx.now, -1, self.config.timezone)
                self.reply(ctx, "日切已关闭。")
            case "simple_mode":
                self.storage.update_group(ctx.chat_id, ctx.now, simple_limit=int(command.args["limit"]))
                self.reply(ctx, f"简洁模式已开启，只显示最近 {command.args['limit']} 条。")
            case "full_mode":
                self.storage.update_group(ctx.chat_id, ctx.now, simple_limit=0)
                self.reply(ctx, "完整模式已开启。")
            case "add_operator":
                self.add_or_remove_operator(ctx, add=True, mentions=command.args["mentions"])
            case "remove_operator":
                self.add_or_remove_operator(ctx, add=False, mentions=command.args["mentions"])
            case "show_operators":
                self.show_operators(ctx)
            case "undo_last":
                self.undo_by_reply_or_last(ctx, None)
            case "undo_deposit":
                self.undo_by_reply_or_last(ctx, "deposit")
            case "undo_payout":
                self.undo_by_reply_or_last(ctx, "payout")
            case "clear_today":
                self.ask_clear_confirm(ctx, "today")
            case "clear_all":
                self.ask_clear_confirm(ctx, "all")
            case "modify_exchange_for_bill":
                day_key = self.day_key(ctx)
                rate = str(command.args["exchange_rate"])
                group = self.storage.get_group(ctx.chat_id)
                if self.day_cutoff_disabled(group):
                    changed = self.storage.update_day_exchange_rate(ctx.chat_id, day_key, rate, ctx.now, all_days=True)
                else:
                    begin, end = self.bill_window(group, day_key)
                    changed = self.storage.update_period_exchange_rate(ctx.chat_id, begin, end, rate, ctx.now)
                self.storage.update_group(
                    ctx.chat_id,
                    ctx.now,
                    deposit_exchange_rate=rate,
                    realtime_rate=0,
                    realtime_rate_rank=None,
                    realtime_rate_offset="0",
                )
                self.reply(ctx, f"已同步今日账单汇率为 {format_number(Decimal(rate))}，更新 {changed} 条。")
            case "pin_on":
                self.storage.update_group(ctx.chat_id, ctx.now, pin_enabled=1)
                self.reply(ctx, "记账置顶已开启。")
            case "pin_off":
                self.storage.update_group(ctx.chat_id, ctx.now, pin_enabled=0)
                self.reply(ctx, "记账置顶已关闭。")
            case "realtime_rate_on":
                offset = command.args.get("offset", Decimal("0"))
                self.storage.update_group(
                    ctx.chat_id,
                    ctx.now,
                    realtime_rate=1,
                    realtime_rate_rank=None,
                    realtime_rate_offset=str(offset),
                )
                self.reply(ctx, "实时汇率已开启。第三方汇率源接入后会自动同步。")
            case "set_currency":
                self.storage.update_group(ctx.chat_id, ctx.now, currency=command.args["currency"])
                self.reply(ctx, f"币种已设置为 {command.args['currency']}。")
            case "set_payout_cny_mode":
                self.storage.update_group(ctx.chat_id, ctx.now, payout_mode="cny")
                self.reply(ctx, "下发人民币模式已开启。")
            case "set_payout_coin_mode":
                self.storage.update_group(ctx.chat_id, ctx.now, payout_mode="coin")
                self.reply(ctx, "下发币模式已开启。")
            case "multiply_exchange_on":
                self.storage.update_group(ctx.chat_id, ctx.now, multiply_exchange=1)
                self.reply(ctx, "乘汇率模式已开启。")
            case "multiply_exchange_off":
                self.storage.update_group(ctx.chat_id, ctx.now, multiply_exchange=0)
                self.reply(ctx, "乘汇率模式已关闭。")
            case "show_cny_on":
                self.storage.update_group(ctx.chat_id, ctx.now, show_cny=1)
                self.reply(ctx, "人民币显示已开启。")
            case "show_cny_off":
                self.storage.update_group(ctx.chat_id, ctx.now, show_cny=0)
                self.reply(ctx, "人民币显示已隐藏。")
            case "all_members_on":
                self.storage.update_group(ctx.chat_id, ctx.now, all_members_can_record=1)
                self.reply(ctx, "已设置所有群员都可记账。")
            case "all_members_off":
                self.storage.update_group(ctx.chat_id, ctx.now, all_members_can_record=0)
                self.reply(ctx, "已取消所有人记账，仅操作员可记账。")
            case "expires_at":
                self.reply(ctx, self.format_expiration(group))
            case "notify_all":
                self.notify_all(ctx)
            case "save_bill":
                self.reply(ctx, "账单已保存。当前版本会保留历史流水，过日切自动进入新账期。")
            case "leave_group":
                self.reply(ctx, "机器人即将退群，并清除本群权限和账单。")
                self.storage.delete_group_data(ctx.chat_id)
                self.client.leave_chat(ctx.chat_id)
            case "set_rate_from_otc_rank":
                self.set_rate_from_otc_rank(ctx, int(command.args["rank"]), command.args["offset"])
            case "otc":
                self.send_otc_rates(ctx)
            case "rate_query":
                self.reply(ctx, self.format_current_rate(group))
            case "external_query":
                if self.handle_external_query(ctx, command.args["query"]):
                    return
                self.reply(ctx, "查询接口还未接入。当前已支持 TRX 地址查询，发送：查询Txxxx")
            case _:
                self.reply(ctx, "这个指令已识别，但当前版本还未启用。")

    def fetch_otc_top_entries(self, limit: int = 10) -> list[P2POrderBookEntry]:
        return self.p2p_rate_client.fetch_order_book_top(
            market=self.config.p2p_rate_market,
            fiat_unit=self.config.p2p_rate_fiat_unit,
            asset=self.config.p2p_rate_asset,
            trade_methods=list(self.config.p2p_rate_trade_methods),
            limit=limit,
        )

    def send_otc_rates(self, ctx: MessageContext) -> None:
        try:
            entries = self.fetch_otc_top_entries(10)
        except P2PRateError as exc:
            self.reply(ctx, f"Z0 查询失败：{exc}")
            return

        market = self.config.p2p_rate_market.upper()
        lines = [f"<b>{escape(f'{market} OTC商家所有实时汇率 TOP 10')}</b>", ""]
        for entry in entries:
            merchant = escape(self.trim_text(entry.merchant_name, 10))
            lines.append(f"Z{entry.rank}  {format_number(entry.price)}  {merchant}")
        lines.append("")
        lines.append("发送 设置汇率 Z1 或 设置汇率 Z1 -0.1 可按第1档偏移后设置汇率。")
        self.client.send_message(ctx.chat_id, "\n".join(lines), parse_mode="HTML")

    def set_rate_from_otc_rank(self, ctx: MessageContext, rank: int, offset: Decimal) -> None:
        try:
            entries = self.fetch_otc_top_entries(max(10, rank))
        except P2PRateError as exc:
            self.reply(ctx, f"设置失败：{exc}")
            return

        entry = next((item for item in entries if item.rank == rank), None)
        if entry is None:
            self.reply(ctx, f"设置失败：没有找到 Z{rank} 档。")
            return

        rate = entry.price + offset
        if rate <= 0:
            self.reply(ctx, "设置失败：计算后的汇率不能小于或等于 0。")
            return

        rate_text = format_number(rate)
        self.storage.update_group(
            ctx.chat_id,
            ctx.now,
            deposit_exchange_rate=str(rate),
            realtime_rate=1,
            realtime_rate_rank=rank,
            realtime_rate_offset=str(offset),
        )
        self.reply(
            ctx,
            f"操作成功：Z{rank} 基准 {format_number(entry.price)}，偏移 {format_signed_decimal(offset)}，当前汇率 {rate_text}",
        )

    def format_current_rate(self, group: Any) -> str:
        return (
            f"当前入款汇率：{format_number(Decimal(group['deposit_exchange_rate']))}\n"
            f"当前下发汇率：{format_number(Decimal(group['payout_exchange_rate']))}"
        )

    def handle_external_query(self, ctx: MessageContext, query: str) -> bool:
        address = extract_tron_address(query)
        if not address:
            return False
        self.send_tron_address_query(ctx.chat_id, address, reply_to_message_id=ctx.message_id)
        return True

    def send_tron_address_query(
        self,
        chat_id: int,
        address: str,
        *,
        reply_to_message_id: int | None = None,
    ) -> None:
        try:
            account = self.tron_client.fetch_account(address)
            transfer_rows = self.tron_client.fetch_recent_trc20_transfers(
                address,
                contract_address=self.config.tron_usdt_contract,
                limit=5,
            )
        except TronGridError as exc:
            self.client.send_message(chat_id, f"TRX 地址查询失败：{exc}", reply_to_message_id=reply_to_message_id)
            return

        info = parse_tron_address_info(
            account,
            address=address,
            timezone=self.config.timezone,
            usdt_contract=self.config.tron_usdt_contract,
        )
        transfers = [
            transfer
            for row in transfer_rows
            if (
                transfer := parse_usdt_transfer(
                    row,
                    watched_address=address,
                    watched_label=None,
                    timezone=self.config.timezone,
                    usdt_contract=self.config.tron_usdt_contract,
                )
            )
            is not None
        ]
        self.client.send_message(
            chat_id,
            format_tron_address_query(info, transfers),
            reply_to_message_id=reply_to_message_id,
            parse_mode="HTML",
        )

    @staticmethod
    def trim_text(value: str, max_len: int) -> str:
        return value if len(value) <= max_len else value[:max_len] + "..."

    def handle_private_message(
        self,
        *,
        message: dict[str, Any],
        chat_id: int,
        user: TelegramUser,
        text: str,
        now: datetime,
    ) -> None:
        self.storage.touch_user(chat_id, user, now)
        normalized = text.strip()
        message_id = int(message["message_id"])

        if normalized in {"/start", "菜单", "功能", "返回菜单"}:
            self.send_private_menu(chat_id, message_id)
            return
        if normalized in {"我的ID", "/id", "id", "ID"}:
            self.client.send_message(chat_id, f"你的 Telegram ID：{user.user_id}", reply_to_message_id=message_id)
            return
        if self.handle_address_watch_pending_input(chat_id, user.user_id, normalized, now, message_id):
            return
        if self.handle_default_operator_command(chat_id, user, normalized, message_id):
            return
        if self.handle_broadcast_private_command(
            message=message,
            chat_id=chat_id,
            user=user,
            text=normalized,
            now=now,
            reply_to_message_id=message_id,
        ):
            return
        if normalized in {"✍开始记账", "开始记账"}:
            self.send_start_group_help(chat_id, message_id)
            return
        if normalized in {"📃详细说明", "详细说明"}:
            self.client.send_message(chat_id, self.private_help_text(), reply_to_message_id=message_id)
            return
        if normalized in {"💵自助续费", "自助续费"}:
            self.client.send_message(chat_id, "自助续费入口已预留，后续可接支付或卡密。", reply_to_message_id=message_id)
            return
        if normalized in {"🔔地址监听", "地址监听", "监听地址"}:
            self.send_address_watch_menu(chat_id, user.user_id, message_id)
            return
        if normalized in {"🛠功能设置", "功能设置"}:
            self.client.send_message(chat_id, "功能设置已预留：记账置顶、日切、汇率模式、地址识别等会集中到这里。", reply_to_message_id=message_id)
            return
        if normalized in {"📒账单统计", "账单统计"}:
            self.client.send_message(chat_id, "账单统计已预留：后续可按群、日期、操作人、备注汇总。", reply_to_message_id=message_id)
            return
        if self.handle_address_watch_command(chat_id, user.user_id, normalized, now, message_id):
            return
        if address := extract_tron_address(normalized):
            self.send_tron_address_query(chat_id, address, reply_to_message_id=message_id)
            return

        self.send_private_menu(chat_id, message_id)

    def handle_default_operator_command(
        self,
        chat_id: int,
        user: TelegramUser,
        text: str,
        reply_to_message_id: int,
    ) -> bool:
        if text in {"默认操作人", "显示默认操作人", "全局操作人"}:
            if not self.is_host(user.user_id):
                self.client.send_message(chat_id, "只有宿主可以查看默认操作人。", reply_to_message_id=reply_to_message_id)
                return True
            self.client.send_message(chat_id, self.format_default_operators(), reply_to_message_id=reply_to_message_id)
            return True

        match = re.fullmatch(r"(?:添加|设置)(?:默认|全局)?操作人\s+(.+)", text)
        if match:
            self.client.send_message(
                chat_id,
                "默认操作人只能由维护人员修改服务器配置 DEFAULT_OPERATOR_USER_IDS。",
                reply_to_message_id=reply_to_message_id,
            )
            return True

        match = re.fullmatch(r"(?:删除|移除)(?:默认|全局)?操作人\s+(.+)", text)
        if match:
            self.client.send_message(
                chat_id,
                "默认操作人只能由维护人员修改服务器配置 DEFAULT_OPERATOR_USER_IDS。",
                reply_to_message_id=reply_to_message_id,
            )
            return True

        return False

    @staticmethod
    def parse_operator_targets(text: str) -> list[str]:
        targets: list[str] = []
        for token in re.split(r"[\s,，]+", text.strip()):
            if not token:
                continue
            if re.fullmatch(r"@?[A-Za-z0-9_]{3,32}", token) and not token.lstrip("@").isdigit():
                targets.append(token if token.startswith("@") else f"@{token}")
            elif re.fullmatch(r"\d{4,20}", token):
                targets.append(token)
        return targets

    def format_default_operators(self) -> str:
        lines = ["默认操作人："]
        configured = sorted(self.config.default_operator_user_ids)
        if configured:
            lines.extend(f"- {user_id}" for user_id in configured)
        else:
            lines.append("暂无")
        lines.append("")
        lines.append("默认操作人由维护人员通过服务器配置 DEFAULT_OPERATOR_USER_IDS 管理。")
        return "\n".join(lines)

    def send_private_menu(self, chat_id: int, reply_to_message_id: int | None = None) -> None:
        self.client.send_message(
            chat_id,
            "请选择功能：",
            reply_to_message_id=reply_to_message_id,
            reply_markup={
                "keyboard": [
                    [{"text": "✍开始记账"}, {"text": "📃详细说明"}, {"text": "📣分组广播"}],
                    [{"text": "💵自助续费"}, {"text": "🔔地址监听"}, {"text": "📡群发广播"}],
                    [{"text": "🛠功能设置"}, {"text": "📒账单统计"}],
                ],
                "resize_keyboard": True,
                "one_time_keyboard": False,
            },
        )

    def send_start_group_help(self, chat_id: int, reply_to_message_id: int | None = None) -> None:
        text = "把机器人添加到群并设为管理员后，在群里发送“开始”即可开始记账。"
        reply_markup = None
        if self.config.telegram_bot_username:
            reply_markup = {
                "inline_keyboard": [
                    [
                        {
                            "text": "添加到群",
                            "url": f"https://t.me/{self.config.telegram_bot_username}?startgroup=1",
                        }
                    ]
                ]
            }
        self.client.send_message(chat_id, text, reply_to_message_id=reply_to_message_id, reply_markup=reply_markup)

    def private_help_text(self) -> str:
        return "\n".join(
            [
                "群内常用指令：",
                "开始 / 关闭",
                "设置汇率10",
                "Z0",
                "设置汇率 Z1",
                "设置费率3",
                "+1000",
                "+1000/7.1",
                "下发100U",
                "+0",
                "显示账单",
                "",
                "撤销：回复自己发送的原始加账消息，发送“撤销入款”或“撤销下发”。",
            ]
        )

    def handle_broadcast_private_command(
        self,
        *,
        message: dict[str, Any],
        chat_id: int,
        user: TelegramUser,
        text: str,
        now: datetime,
        reply_to_message_id: int,
    ) -> bool:
        if text in {"📣分组广播", "分组广播"}:
            self.send_broadcast_help(chat_id, user, grouped=True, reply_to_message_id=reply_to_message_id)
            return True
        if text in {"📡群发广播", "群发广播"}:
            if message.get("reply_to_message"):
                self.create_broadcast_from_private(chat_id, user, "", message, now, reply_to_message_id)
            else:
                self.send_broadcast_help(chat_id, user, grouped=False, reply_to_message_id=reply_to_message_id)
            return True
        if text in {"群列表", "群组列表", "广播群列表"}:
            self.send_broadcast_group_list(chat_id, user, reply_to_message_id=reply_to_message_id)
            return True
        if text in {"分组列表", "广播分组", "广播分组列表"}:
            self.send_named_broadcast_group_list(chat_id, user, reply_to_message_id=reply_to_message_id)
            return True
        match = re.fullmatch(r"(?:新建|创建)分组\s+(.+)", text)
        if match:
            if not self.can_use_broadcast(user):
                self.client.send_message(chat_id, "没有广播权限。", reply_to_message_id=reply_to_message_id)
                return True
            group = self.storage.create_named_broadcast_group(match.group(1), created_by=user.user_id, now=now)
            self.client.send_message(chat_id, f"分组已创建：{group['name']}", reply_to_message_id=reply_to_message_id)
            return True

        match = re.fullmatch(r"删除分组\s+(.+)", text)
        if match:
            if not self.can_use_broadcast(user):
                self.client.send_message(chat_id, "没有广播权限。", reply_to_message_id=reply_to_message_id)
                return True
            deleted = self.storage.delete_named_broadcast_group(match.group(1))
            self.client.send_message(
                chat_id,
                "分组已删除。" if deleted else "没有找到这个分组。",
                reply_to_message_id=reply_to_message_id,
            )
            return True

        match = re.fullmatch(r"(?:分组添加|添加分组群)\s+(\S+)\s+(.+)", text, flags=re.S)
        if match:
            self.add_or_remove_broadcast_group_members(
                chat_id,
                user,
                group_name=match.group(1),
                ids_text=match.group(2),
                add=True,
                now=now,
                reply_to_message_id=reply_to_message_id,
            )
            return True

        match = re.fullmatch(r"(?:分组移除|分组删除|删除分组群|移除分组群)\s+(\S+)\s+(.+)", text, flags=re.S)
        if match:
            self.add_or_remove_broadcast_group_members(
                chat_id,
                user,
                group_name=match.group(1),
                ids_text=match.group(2),
                add=False,
                now=now,
                reply_to_message_id=reply_to_message_id,
            )
            return True

        match = re.fullmatch(r"(?:分组详情|查看分组)\s+(.+)", text)
        if match:
            self.send_named_broadcast_group_detail(
                chat_id,
                user,
                match.group(1),
                reply_to_message_id=reply_to_message_id,
            )
            return True

        if text.startswith("群发广播 "):
            self.create_broadcast_from_private(
                chat_id,
                user,
                text.removeprefix("群发广播 ").strip(),
                message,
                now,
                reply_to_message_id,
            )
            return True

        if text.startswith("分组广播 "):
            self.create_grouped_broadcast_from_private(
                chat_id,
                user,
                text.removeprefix("分组广播 ").strip(),
                message,
                now,
                reply_to_message_id,
            )
            return True

        if self.broadcast_message_kind(message) != "text" and self.is_broadcastable_message(message):
            self.client.send_message(
                chat_id,
                "素材已收到。回复这条消息发送“群发广播”或“分组广播 分组名”。",
                reply_to_message_id=reply_to_message_id,
            )
            return True

        return False

    def send_broadcast_help(
        self,
        chat_id: int,
        user: TelegramUser,
        *,
        grouped: bool,
        reply_to_message_id: int | None = None,
    ) -> None:
        if not self.can_use_broadcast(user):
            self.client.send_message(chat_id, "没有广播权限。", reply_to_message_id=reply_to_message_id)
            return
        if grouped:
            named_groups = self.storage.list_named_broadcast_groups()
            lines = [
                "分组广播",
                "新建分组：新建分组 财务",
                "批量添加：分组添加 财务 -100111 -100222",
                "批量移除：分组移除 财务 -100111 -100222",
                "查看成员：分组详情 财务",
                "文字广播：分组广播 财务 广播内容",
                "图片广播：回复图片发送 分组广播 财务",
                "通知所有人：分组广播 财务 通知所有人 广播内容",
                "",
                f"当前分组：{len(named_groups)} 个",
            ]
            for row in named_groups[:20]:
                lines.append(f"{row['name']}（{row['member_count']}群）")
            if len(named_groups) > 20:
                lines.append(f"... 还有 {len(named_groups) - 20} 个，发送“分组列表”查看。")
        else:
            groups = self.storage.list_broadcast_groups()
            lines = [
                "群发广播",
                "文字广播：群发广播 广播内容",
                "图片广播：回复图片发送 群发广播",
                "通知所有人：群发广播 通知所有人 广播内容",
                "查看群：群列表",
                "发送后会先生成确认按钮，点确认才会发送。",
                "",
                f"当前已保存群组：{len(groups)} 个",
            ]
            for row in groups[:20]:
                lines.append(self.format_broadcast_group(row))
            if len(groups) > 20:
                lines.append(f"... 还有 {len(groups) - 20} 个，发送“群列表”查看。")
        self.client.send_message(chat_id, "\n".join(lines), reply_to_message_id=reply_to_message_id)

    def send_broadcast_group_list(
        self,
        chat_id: int,
        user: TelegramUser,
        *,
        reply_to_message_id: int | None = None,
    ) -> None:
        if not self.can_use_broadcast(user):
            self.client.send_message(chat_id, "没有广播权限。", reply_to_message_id=reply_to_message_id)
            return
        groups = self.storage.list_broadcast_groups()
        if not groups:
            self.client.send_message(chat_id, "当前没有已保存群组。", reply_to_message_id=reply_to_message_id)
            return
        chunks: list[list[str]] = [[]]
        for row in groups:
            line = self.format_broadcast_group(row)
            if sum(len(item) + 1 for item in chunks[-1]) + len(line) > 3500:
                chunks.append([])
            chunks[-1].append(line)
        for index, chunk in enumerate(chunks):
            title = f"已保存群组（{len(groups)}个）"
            if len(chunks) > 1:
                title += f" {index + 1}/{len(chunks)}"
            self.client.send_message(
                chat_id,
                title + "\n" + "\n".join(chunk),
                reply_to_message_id=reply_to_message_id if index == 0 else None,
            )

    def send_named_broadcast_group_list(
        self,
        chat_id: int,
        user: TelegramUser,
        *,
        reply_to_message_id: int | None = None,
    ) -> None:
        if not self.can_use_broadcast(user):
            self.client.send_message(chat_id, "没有广播权限。", reply_to_message_id=reply_to_message_id)
            return
        rows = self.storage.list_named_broadcast_groups()
        if not rows:
            self.client.send_message(
                chat_id,
                "当前没有广播分组。发送“新建分组 分组名”创建。",
                reply_to_message_id=reply_to_message_id,
            )
            return
        lines = ["广播分组："]
        for row in rows:
            lines.append(f"{row['name']}（{row['member_count']}群）")
        self.client.send_message(chat_id, "\n".join(lines), reply_to_message_id=reply_to_message_id)

    def send_named_broadcast_group_detail(
        self,
        chat_id: int,
        user: TelegramUser,
        group_name: str,
        *,
        reply_to_message_id: int | None = None,
    ) -> None:
        if not self.can_use_broadcast(user):
            self.client.send_message(chat_id, "没有广播权限。", reply_to_message_id=reply_to_message_id)
            return
        group = self.storage.get_named_broadcast_group(group_name)
        if group is None:
            self.client.send_message(chat_id, "没有找到这个分组。", reply_to_message_id=reply_to_message_id)
            return
        members = self.storage.list_broadcast_group_members(group["name"])
        lines = [f"分组：{group['name']}（{len(members)}群）"]
        lines.extend(self.format_broadcast_group(row) for row in members)
        if len(lines) == 1:
            lines.append("暂无群组。")
        self.client.send_message(chat_id, "\n".join(lines), reply_to_message_id=reply_to_message_id)

    @staticmethod
    def format_broadcast_group(row: Any) -> str:
        title = row["chat_title"] or "(未命名群)"
        return f"{row['chat_id']}  {title}"

    def add_or_remove_broadcast_group_members(
        self,
        chat_id: int,
        user: TelegramUser,
        *,
        group_name: str,
        ids_text: str,
        add: bool,
        now: datetime,
        reply_to_message_id: int,
    ) -> None:
        if not self.can_use_broadcast(user):
            self.client.send_message(chat_id, "没有广播权限。", reply_to_message_id=reply_to_message_id)
            return
        chat_ids = self.parse_chat_ids(ids_text)
        if not chat_ids:
            self.client.send_message(chat_id, "没有识别到群ID。", reply_to_message_id=reply_to_message_id)
            return
        try:
            if add:
                changed, known_ids, missing_ids = self.storage.add_broadcast_group_members(
                    group_name,
                    chat_ids,
                    now=now,
                )
                lines = [f"已添加 {changed} 个群到分组“{group_name}”。"]
                skipped = len(known_ids) - changed
                if skipped > 0:
                    lines.append(f"已存在/重复：{skipped} 个")
                if missing_ids:
                    lines.append("未保存的群ID：" + " ".join(str(value) for value in missing_ids))
            else:
                changed = self.storage.remove_broadcast_group_members(group_name, chat_ids, now=now)
                lines = [f"已从分组“{group_name}”移除 {changed} 个群。"]
        except KeyError:
            self.client.send_message(chat_id, "没有找到这个分组，请先发送“新建分组 分组名”。", reply_to_message_id=reply_to_message_id)
            return
        self.client.send_message(chat_id, "\n".join(lines), reply_to_message_id=reply_to_message_id)

    @staticmethod
    def parse_chat_ids(text: str) -> list[int]:
        return [int(value) for value in re.findall(r"-\d{5,20}", text)]

    def create_broadcast_from_private(
        self,
        chat_id: int,
        user: TelegramUser,
        body: str,
        message: dict[str, Any],
        now: datetime,
        reply_to_message_id: int,
    ) -> None:
        if not self.can_use_broadcast(user):
            self.client.send_message(chat_id, "没有广播权限。", reply_to_message_id=reply_to_message_id)
            return
        notify_all, text = self.extract_notify_all_option(body)
        source_message = message.get("reply_to_message") if not text else None
        if not text and not self.is_broadcastable_message(source_message):
            self.send_broadcast_help(chat_id, user, grouped=False, reply_to_message_id=reply_to_message_id)
            return
        groups = self.storage.list_broadcast_groups()
        target_ids = [int(row["chat_id"]) for row in groups]
        self.create_broadcast_job_reply(
            chat_id,
            user,
            "all",
            target_ids,
            text,
            source_message,
            notify_all,
            now,
            reply_to_message_id,
        )

    def create_grouped_broadcast_from_private(
        self,
        chat_id: int,
        user: TelegramUser,
        body: str,
        message: dict[str, Any],
        now: datetime,
        reply_to_message_id: int,
    ) -> None:
        if not self.can_use_broadcast(user):
            self.client.send_message(chat_id, "没有广播权限。", reply_to_message_id=reply_to_message_id)
            return
        if not body:
            self.send_broadcast_help(chat_id, user, grouped=True, reply_to_message_id=reply_to_message_id)
            return
        parts = body.split(maxsplit=1)
        group_name = parts[0]
        rest = parts[1].strip() if len(parts) > 1 else ""
        notify_all, text = self.extract_notify_all_option(rest)
        source_message = message.get("reply_to_message") if not text else None
        if not text and not self.is_broadcastable_message(source_message):
            self.send_broadcast_help(chat_id, user, grouped=True, reply_to_message_id=reply_to_message_id)
            return
        group = self.storage.get_named_broadcast_group(group_name)
        if group is None:
            self.client.send_message(chat_id, "没有找到这个分组。", reply_to_message_id=reply_to_message_id)
            return
        target_ids = self.storage.target_chat_ids_for_broadcast_group(group["name"])
        self.create_broadcast_job_reply(
            chat_id,
            user,
            f"group:{group['name']}",
            target_ids,
            text,
            source_message,
            notify_all,
            now,
            reply_to_message_id,
        )

    @staticmethod
    def extract_notify_all_option(body: str) -> tuple[bool, str]:
        text = body.strip()
        notify_all = False
        markers = ["通知所有人", "@所有人", "@all"]
        changed = True
        while changed:
            changed = False
            for marker in markers:
                if text == marker:
                    return True, ""
                if text.startswith(marker + " "):
                    text = text[len(marker):].strip()
                    notify_all = True
                    changed = True
                elif text.endswith(" " + marker):
                    text = text[: -len(marker)].strip()
                    notify_all = True
                    changed = True
        return notify_all, text

    def create_broadcast_job_reply(
        self,
        chat_id: int,
        user: TelegramUser,
        scope: str,
        target_chat_ids: list[int],
        text: str,
        source_message: dict[str, Any] | None,
        notify_all: bool,
        now: datetime,
        reply_to_message_id: int,
    ) -> None:
        target_chat_ids = list(dict.fromkeys(target_chat_ids))
        if not target_chat_ids:
            self.client.send_message(chat_id, "当前没有可广播的群组。", reply_to_message_id=reply_to_message_id)
            return
        source_chat_id: int | None = None
        source_message_id: int | None = None
        message_kind = "text"
        preview = text
        if source_message is not None:
            source_chat_id = chat_id
            source_message_id = int(source_message["message_id"])
            message_kind = self.broadcast_message_kind(source_message)
            preview = self.broadcast_preview(source_message)
        job = self.storage.create_broadcast_job(
            creator_user_id=user.user_id,
            scope=scope,
            target_chat_ids=target_chat_ids,
            text=preview,
            source_chat_id=source_chat_id,
            source_message_id=source_message_id,
            message_kind=message_kind,
            notify_all=notify_all,
            now=now,
        )
        preview_text = preview if len(preview) <= 600 else preview[:600] + "..."
        notify_text = "\n通知所有人：开启" if notify_all else ""
        self.client.send_message(
            chat_id,
            f"确认发送广播？\n目标群：{len(target_chat_ids)} 个\n类型：{message_kind}{notify_text}\n\n{preview_text}",
            reply_to_message_id=reply_to_message_id,
            reply_markup={
                "inline_keyboard": [
                    [
                        {"text": "确认发送", "callback_data": f"broadcast:send:{job['id']}"},
                        {"text": "取消", "callback_data": f"broadcast:cancel:{job['id']}"},
                    ]
                ]
            },
        )

    @staticmethod
    def broadcast_message_kind(message: dict[str, Any] | None) -> str:
        if not message:
            return "text"
        for kind in ("photo", "video", "animation", "document", "audio", "voice"):
            if kind in message:
                return kind
        return "text"

    @staticmethod
    def is_broadcastable_message(message: dict[str, Any] | None) -> bool:
        if not message:
            return False
        return bool(message.get("text") or message.get("caption")) or any(
            kind in message for kind in ("photo", "video", "animation", "document", "audio", "voice")
        )

    @classmethod
    def broadcast_preview(cls, message: dict[str, Any]) -> str:
        text = (message.get("text") or message.get("caption") or "").strip()
        kind = cls.broadcast_message_kind(message)
        if kind == "text":
            return text
        prefix = {
            "photo": "[图片]",
            "video": "[视频]",
            "animation": "[动图]",
            "document": "[文件]",
            "audio": "[音频]",
            "voice": "[语音]",
        }.get(kind, "[消息]")
        return f"{prefix} {text}".strip()

    def send_address_watch_menu(
        self,
        chat_id: int,
        owner_user_id: int,
        reply_to_message_id: int | None = None,
    ) -> None:
        rows = self.storage.list_address_watches(owner_user_id)
        settings = self.storage.get_address_watch_settings(owner_user_id, datetime.now(self.config.timezone))
        min_amount = min_notify_amount(settings)
        min_text = "不限" if min_amount == 0 else f"{format_number(min_amount)} USDT"
        lines = [
            "USDT 地址监听",
            f"模式：{'精简模式' if settings['display_mode'] == 'compact' else '完整模式'}",
            f"收入：{'开启' if settings['watch_income'] else '关闭'}",
            f"支出：{'开启' if settings['watch_expense'] else '关闭'}",
            f"TRX通知：{'开启' if settings['notify_trx'] else '关闭'}",
            f"最小提醒：{min_text}",
            "",
            "添加：设置地址 Txxxx 备注",
            "删除：删除监听地址 Txxxx",
            "备注：设置备注 Txxxx 备注",
            "金额：设置监听金额 10",
            "",
            "当前监听地址：",
        ]
        if rows:
            for row in rows[:20]:
                label = f" {row['label']}" if row["label"] else ""
                lines.append(f"{row['network']} {row['address']}{label}")
        else:
            lines.append("暂无")
        self.client.send_message(
            chat_id,
            "\n".join(lines),
            reply_to_message_id=reply_to_message_id,
            reply_markup=self.address_watch_keyboard(settings),
        )

    def address_watch_keyboard(self, settings: Any) -> dict[str, Any]:
        compact_text = "精简模式✅" if settings["display_mode"] == "compact" else "精简模式"
        full_text = "完整模式✅" if settings["display_mode"] == "full" else "完整模式"
        income_text = "收入✅" if settings["watch_income"] else "收入❌"
        expense_text = "支出✅" if settings["watch_expense"] else "支出❌"
        trx_text = "关闭TRX通知✅" if settings["notify_trx"] else "开启TRX通知❌"
        min_amount = min_notify_amount(settings)
        min_text = "最小金额：不限" if min_amount == 0 else f"最小金额：{format_number(min_amount)}U"
        return {
            "inline_keyboard": [
                [
                    {"text": "➕ 设置地址", "callback_data": "watch:prompt:add"},
                    {"text": "📝 设置备注", "callback_data": "watch:prompt:label"},
                ],
                [
                    {"text": "➖ 删除地址", "callback_data": "watch:prompt:remove"},
                    {"text": min_text, "callback_data": "watch:prompt:min"},
                ],
                [
                    {"text": compact_text, "callback_data": "watch:mode:compact"},
                    {"text": full_text, "callback_data": "watch:mode:full"},
                ],
                [
                    {"text": income_text, "callback_data": "watch:toggle:income"},
                    {"text": expense_text, "callback_data": "watch:toggle:expense"},
                ],
                [{"text": trx_text, "callback_data": "watch:toggle:trx"}],
            ]
        }

    def handle_address_watch_command(
        self,
        chat_id: int,
        owner_user_id: int,
        text: str,
        now: datetime,
        reply_to_message_id: int | None,
    ) -> bool:
        add_match = re.fullmatch(r"(?:添加监听地址|设置监听地址|添加地址|设置地址|监听)\s+(.+)", text)
        if add_match:
            self.add_address_watch_from_text(chat_id, owner_user_id, add_match.group(1), now, reply_to_message_id)
            return True

        remove_match = re.fullmatch(r"(?:删除监听地址|删除地址|取消监听)\s+(\S+)", text)
        if remove_match:
            address = remove_match.group(1).strip()
            removed = self.storage.remove_address_watch(owner_user_id, address)
            self.client.send_message(
                chat_id,
                "监听地址已删除。" if removed else "没有找到这个监听地址。",
                reply_to_message_id=reply_to_message_id,
            )
            return True

        label_match = re.fullmatch(r"(?:设置地址备注|设置备注|修改备注)\s+(.+)", text)
        if label_match:
            self.update_address_watch_label_from_text(chat_id, owner_user_id, label_match.group(1), now, reply_to_message_id)
            return True

        amount_match = re.fullmatch(r"(?:设置监听金额|设置最小金额|最小提醒金额|最小金额)\s*(\d+(?:\.\d+)?)", text)
        if amount_match:
            self.update_address_watch_min_amount(chat_id, owner_user_id, amount_match.group(1), now, reply_to_message_id)
            return True
        return False

    def handle_address_watch_pending_input(
        self,
        chat_id: int,
        owner_user_id: int,
        text: str,
        now: datetime,
        reply_to_message_id: int | None,
    ) -> bool:
        action = self.address_watch_pending.get(owner_user_id)
        if not action:
            return False
        if text in {"取消", "退出", "返回"}:
            self.address_watch_pending.pop(owner_user_id, None)
            self.client.send_message(chat_id, "已取消。", reply_to_message_id=reply_to_message_id)
            self.send_address_watch_menu(chat_id, owner_user_id)
            return True
        if action == "add":
            ok = self.add_address_watch_from_text(chat_id, owner_user_id, text, now, reply_to_message_id)
        elif action == "remove":
            removed = self.storage.remove_address_watch(owner_user_id, text.strip())
            self.client.send_message(
                chat_id,
                "监听地址已删除。" if removed else "没有找到这个监听地址。",
                reply_to_message_id=reply_to_message_id,
            )
            ok = bool(removed)
        elif action == "label":
            ok = self.update_address_watch_label_from_text(chat_id, owner_user_id, text, now, reply_to_message_id)
        elif action == "min":
            ok = self.update_address_watch_min_amount(chat_id, owner_user_id, text, now, reply_to_message_id)
        else:
            ok = False
        if ok:
            self.address_watch_pending.pop(owner_user_id, None)
            self.send_address_watch_menu(chat_id, owner_user_id)
        return True

    def add_address_watch_from_text(
        self,
        chat_id: int,
        owner_user_id: int,
        text: str,
        now: datetime,
        reply_to_message_id: int | None,
    ) -> bool:
        address, label = self.parse_address_and_tail(text)
        if not address:
            self.client.send_message(chat_id, "请发送地址，格式：T地址 备注", reply_to_message_id=reply_to_message_id)
            return False
        network = detect_usdt_network(address)
        if not network:
            self.client.send_message(
                chat_id,
                "地址格式不支持。USDT 监听当前只支持 TRC20 的 T 开头地址。",
                reply_to_message_id=reply_to_message_id,
            )
            return False
        self.storage.add_address_watch(owner_user_id, network, address, label, now)
        self.client.send_message(chat_id, "监听地址已保存。", reply_to_message_id=reply_to_message_id)
        return True

    def update_address_watch_label_from_text(
        self,
        chat_id: int,
        owner_user_id: int,
        text: str,
        now: datetime,
        reply_to_message_id: int | None,
    ) -> bool:
        address, label = self.parse_address_and_tail(text)
        if not address:
            self.client.send_message(chat_id, "请发送地址和备注，格式：T地址 备注", reply_to_message_id=reply_to_message_id)
            return False
        changed = self.storage.update_address_watch_label(owner_user_id, address, label, now)
        self.client.send_message(
            chat_id,
            "地址备注已更新。" if changed else "没有找到这个监听地址。",
            reply_to_message_id=reply_to_message_id,
        )
        return bool(changed)

    def update_address_watch_min_amount(
        self,
        chat_id: int,
        owner_user_id: int,
        text: str,
        now: datetime,
        reply_to_message_id: int | None,
    ) -> bool:
        amount = parse_non_negative_decimal(text.strip())
        if amount is None:
            self.client.send_message(chat_id, "请输入大于或等于 0 的 USDT 金额，例如：10", reply_to_message_id=reply_to_message_id)
            return False
        self.storage.update_address_watch_settings(owner_user_id, now, min_notify_amount=str(amount))
        label = "不限" if amount == 0 else f"{format_number(amount)} USDT"
        self.client.send_message(chat_id, f"最小提醒金额已设置为：{label}", reply_to_message_id=reply_to_message_id)
        return True

    @staticmethod
    def parse_address_and_tail(text: str) -> tuple[str, str | None]:
        parts = text.strip().split(maxsplit=1)
        if not parts:
            return "", None
        return parts[0].strip(), (parts[1].strip() if len(parts) > 1 and parts[1].strip() else None)

    def handle_group_address_verification(self, ctx: MessageContext) -> bool:
        address = ctx.text.strip()
        if detect_usdt_network(address) != "TRC20":
            return False
        verification = self.storage.record_address_verification(
            chat_id=ctx.chat_id,
            address=address,
            sender=ctx.user,
            now=ctx.now,
        )
        lines = [
            f"验证地址： <code>{escape(address)}</code>",
            f"验证次数： {verification['count']}",
        ]
        if verification["previous_sender_name"]:
            lines.append(f"上次发送人： {escape(verification['previous_sender_name'])}")
        lines.append(f"本次发送人： {escape(verification['current_sender_name'])}")
        self.client.send_message(
            ctx.chat_id,
            "\n".join(lines),
            reply_to_message_id=ctx.message_id,
            parse_mode="HTML",
        )
        return True

    def handle_ledger_entry(self, ctx: MessageContext, entry: ParsedLedgerEntry) -> None:
        group = self.storage.get_group(ctx.chat_id)
        if not group["active"]:
            if entry.kind == "deposit" and entry.amount == 0:
                self.send_bill(ctx)
            return

        if not self.can_record(ctx, group):
            return

        if entry.kind == "deposit" and entry.amount == 0:
            self.send_bill(ctx)
            return

        record = self.create_record(ctx, group, entry)
        day_key = record["day_key"]
        sent = self.client.send_message(
            ctx.chat_id,
            self.build_bill_text(ctx.chat_id, day_key, compact=True),
            parse_mode="HTML",
            reply_markup={
                "inline_keyboard": [
                    self.bill_button_row(ctx.chat_id, day_key),
                ]
            },
        )
        self.storage.set_record_bot_message(record["id"], int(sent["message_id"]))
        if group["pin_enabled"]:
            try:
                self.client.pin_chat_message(ctx.chat_id, int(sent["message_id"]))
            except TelegramAPIError:
                pass

    def create_record(self, ctx: MessageContext, group: Any, entry: ParsedLedgerEntry) -> Any:
        rate = self.effective_rate(group, entry)
        fee_rate = entry.fee_rate
        if fee_rate is None:
            fee_rate = Decimal(group["deposit_fee_rate"] if entry.kind == "deposit" else group["payout_fee_rate"])

        effective_amount = entry.amount * entry.multiplier
        amount_cny, amount_usdt, net_usdt, commission_cny = calculate_amounts(
            kind=entry.kind,
            amount=effective_amount,
            currency=entry.currency,
            rate=rate,
            fee_rate=fee_rate,
            payout_mode=group["payout_mode"],
            multiply_exchange=bool(group["multiply_exchange"]),
        )
        subject_user_id, subject_name = self.subject_for_entry(ctx, entry)

        return self.storage.insert_record(
            {
                "chat_id": ctx.chat_id,
                "kind": entry.kind,
                "amount": str(quant(effective_amount)),
                "currency": entry.currency,
                "exchange_rate": str(rate),
                "fee_rate": str(fee_rate),
                "amount_cny": str(amount_cny),
                "amount_usdt": str(amount_usdt),
                "commission_cny": str(commission_cny),
                "net_usdt": str(net_usdt),
                "actor_user_id": ctx.user.user_id,
                "actor_name": ctx.user.display_name,
                "subject_user_id": subject_user_id,
                "subject_name": subject_name,
                "note": entry.note,
                "is_balance": 1 if entry.is_balance else 0,
                "source_message_id": ctx.message_id,
                "day_key": self.day_key(ctx),
                "created_at": ctx.now.isoformat(),
            }
        )

    def effective_rate(self, group: Any, entry: ParsedLedgerEntry) -> Decimal:
        if entry.exchange_rate is not None:
            return entry.exchange_rate
        key = "deposit_exchange_rate" if entry.kind == "deposit" else "payout_exchange_rate"
        return Decimal(group[key])

    def subject_for_entry(self, ctx: MessageContext, entry: ParsedLedgerEntry) -> tuple[int | None, str]:
        if ctx.reply_user:
            return ctx.reply_user.user_id, ctx.reply_user.display_name
        if entry.subject:
            return None, entry.subject
        return ctx.user.user_id, ctx.user.display_name

    def send_bill(self, ctx: MessageContext, *, only_user_id: int | None = None, compact: bool = False) -> None:
        day_key = self.day_key(ctx)
        text = self.build_bill_text(ctx.chat_id, day_key, only_user_id=only_user_id, compact=compact)
        self.client.send_message(
            ctx.chat_id,
            text,
            parse_mode="HTML",
            reply_markup={"inline_keyboard": [self.bill_button_row(ctx.chat_id, day_key)]},
        )

    def build_bill_text(
        self,
        chat_id: int,
        day_key: str,
        *,
        only_user_id: int | None = None,
        compact: bool = False,
        cutoff_hour: int | None = None,
        begin: datetime | None = None,
        end: datetime | None = None,
    ) -> str:
        group = self.storage.get_group(chat_id)
        rows = self.records_for_bill(chat_id, day_key, group, cutoff_hour=cutoff_hour, begin=begin, end=end)
        if only_user_id is not None:
            rows = [row for row in rows if row["actor_user_id"] == only_user_id or row["subject_user_id"] == only_user_id]
        deposits = [row for row in rows if row["kind"] == "deposit"]
        payouts = [row for row in rows if row["kind"] == "payout"]
        limit = self.bill_limit(group, compact)
        shown_deposits = deposits[-limit:] if limit else deposits
        shown_payouts = payouts[-limit:] if limit else payouts

        lines = [f"<b>今日入款（{len(deposits)}笔）</b>"]
        lines.extend(self.format_record_line(row) for row in shown_deposits)
        lines.append("")
        lines.append(f"<b>今日下发（{len(payouts)}笔）</b>")
        lines.extend(self.format_record_line(row) for row in shown_payouts)

        total_cny = sum_decimal(row["amount_cny"] for row in deposits)
        gross_in_usdt = sum_decimal(row["amount_usdt"] for row in deposits)
        net_in_usdt = sum_decimal(row["net_usdt"] for row in deposits)
        total_out_usdt = sum_decimal(row["amount_usdt"] for row in payouts)
        balance = net_in_usdt - total_out_usdt
        exchange = Decimal(group["deposit_exchange_rate"])
        fee_rate = Decimal(group["deposit_fee_rate"])
        show_usdt_summary = any(self.record_uses_usdt_summary(row) for row in deposits + payouts)

        total_line = f"总入款：{format_number(total_cny)}"
        if show_usdt_summary:
            total_line += f" ({format_money(gross_in_usdt)}U)"
        due = f"{format_money(net_in_usdt)}U" if show_usdt_summary else format_number(net_in_usdt)
        paid = f"{format_money(total_out_usdt)}U" if show_usdt_summary else format_number(total_out_usdt)
        balance_text = f"{format_money(balance)}U" if show_usdt_summary else format_number(balance)

        lines.extend([
            "",
            total_line,
            *self.bill_exchange_lines(group, exchange),
            f"交易费率：{format_number(fee_rate)}%",
            "",
            f"应下发：{due}",
            f"已下发：{paid}",
            f"余额：{balance_text}",
        ])
        return "\n".join(lines)

    def bill_exchange_lines(self, group: Any, exchange: Decimal) -> list[str]:
        if group["realtime_rate"]:
            rank = group["realtime_rate_rank"]
            if rank is not None:
                return [
                    "实时汇率：",
                    join_non_empty(
                        self.realtime_rate_method_label() + f"{int(rank)}档",
                        format_realtime_offset_label(group["realtime_rate_offset"]),
                    ),
                ]
            return [
                "实时汇率：",
                join_non_empty(
                    self.realtime_rate_method_label() + "实时价",
                    format_realtime_offset_label(group["realtime_rate_offset"]),
                ),
            ]
        return [f"汇率：{format_number(exchange)}"]

    def realtime_rate_method_label(self) -> str:
        method = self.config.p2p_rate_trade_methods[0] if self.config.p2p_rate_trade_methods else "aliPay"
        return p2p_trade_method_label(method)

    def format_record_line(self, row: Any) -> str:
        created = datetime.fromisoformat(row["created_at"]).astimezone(self.config.timezone)
        amount = Decimal(row["amount_cny"])
        rate = Decimal(row["exchange_rate"])
        gross_usdt = Decimal(row["amount_usdt"])
        net_usdt = Decimal(row["net_usdt"])
        fee_rate = Decimal(row["fee_rate"])
        subject = escape(row["subject_name"] or row["actor_name"])
        if row["kind"] == "payout":
            if row["currency"] == "USDT":
                body = f"{format_money(gross_usdt)}U"
            elif rate == 1:
                body = format_fixed_2(amount)
            else:
                body = f"{format_number(amount)} / {format_number(rate)}={format_money(gross_usdt)}U"
            return f"{created:%H:%M:%S}  {body} {subject}"

        if row["currency"] == "USDT":
            body = f"{format_money(gross_usdt)}U"
            if fee_rate:
                body += f" * ({format_fee_multiplier(fee_rate)})={format_money(net_usdt)}U"
        elif rate == 1 and not fee_rate:
            body = format_fixed_2(amount)
        elif fee_rate:
            body = (
                f"{format_number(amount)} / {format_number(rate)} "
                f"* ({format_fee_multiplier(fee_rate)})={format_money(net_usdt)}U"
            )
        else:
            body = f"{format_number(amount)} / {format_number(rate)}={format_money(gross_usdt)}U"
        return f"{created:%H:%M:%S}  {body} {subject}"

    def bill_limit(self, group: Any, compact: bool) -> int | None:
        value = group["simple_limit"]
        if value == 0:
            return None
        if value is not None:
            return int(value)
        return 5 if compact else None

    def record_uses_usdt_summary(self, row: Any) -> bool:
        return row["currency"] == "USDT" or Decimal(row["exchange_rate"]) != 1 or Decimal(row["fee_rate"]) != 0

    def bill_button_row(self, chat_id: int, day_key: str) -> list[dict[str, str]]:
        url = self.build_bill_url(chat_id, day_key)
        if url:
            return [{"text": "🌐 完整账单", "url": url}]
        group = self.storage.get_group(chat_id)
        if self.day_cutoff_disabled(group):
            return [{"text": "🌐 完整账单", "callback_data": "bill:full:active"}]
        begin, end = self.bill_window(group, day_key)
        callback_data = f"bill:full:{day_key}:{self.compact_bill_time(begin)}:{self.compact_bill_time(end)}"
        return [{"text": "🌐 完整账单", "callback_data": callback_data}]

    def build_bill_url(self, chat_id: int, day_key: str) -> str | None:
        group = self.storage.get_group(chat_id)
        if self.day_cutoff_disabled(group):
            begin_time = ""
            end_time = ""
            all_flag = "1"
            bill_key = "active"
        else:
            begin, end = self.bill_window(group, day_key)
            begin_time = begin.strftime("%Y-%m-%d %H:%M:%S")
            end_time = end.strftime("%Y-%m-%d %H:%M:%S")
            all_flag = ""
            bill_key = day_key
        values = {
            "chat_id": str(chat_id),
            "day_key": bill_key,
            "begin_time": begin_time,
            "end_time": end_time,
            "all": all_flag,
            "bot_name": self.config.public_bill_bot_name,
            "up_page": "1",
            "down_page": "1",
        }
        if self.config.public_bill_url_template:
            return self.config.public_bill_url_template.format(**values)
        if not self.config.public_bill_base_url:
            return None

        base = self.config.public_bill_base_url.rstrip("/")
        if base.endswith(".php"):
            params = {
                "firstname": "",
                "chat_id": values["chat_id"],
                "up_page": values["up_page"],
                "down_page": values["down_page"],
                "created_at": "",
                "begintime": values["begin_time"],
                "endtime": values["end_time"],
                "all": values["all"],
                "phpname": values["bot_name"],
                "type": "bjr",
            }
            separator = "&" if "?" in base else "?"
            return f"{base}{separator}{urlencode(params)}"
        url = f"{base}/bill/{values['chat_id']}/{values['day_key']}"
        params = {}
        if values["begin_time"] or values["end_time"]:
            params["begintime"] = values["begin_time"]
            params["endtime"] = values["end_time"]
        if values["all"]:
            params["all"] = values["all"]
        if self.config.bill_web_token:
            params["token"] = self.config.bill_web_token
        if params:
            url = f"{url}?{urlencode(params)}"
        return url

    def ask_clear_confirm(self, ctx: MessageContext, scope: str) -> None:
        label = "今日账单" if scope == "today" else "全部账单"
        callback_data = f"clear:{scope}"
        if scope == "today":
            group = self.storage.get_group(ctx.chat_id)
            day_key = self.day_key(ctx)
            if not self.day_cutoff_disabled(group):
                begin, end = self.bill_window(group, day_key)
                callback_data = f"clear:today:{day_key}:{self.compact_bill_time(begin)}:{self.compact_bill_time(end)}"
        self.client.send_message(
            ctx.chat_id,
            f"确认删除{label}？此操作会软删除流水。",
            reply_markup={
                "inline_keyboard": [
                    [
                        {"text": "确认删除", "callback_data": callback_data},
                        {"text": "取消", "callback_data": "clear:cancel"},
                    ]
                ]
            },
        )

    def undo_by_reply_or_last(self, ctx: MessageContext, kind: str | None) -> None:
        if ctx.reply_message_id:
            row = self.find_record_by_message(ctx.chat_id, ctx.reply_message_id)
            if row and (kind is None or row["kind"] == kind):
                if not self.can_undo_record(ctx, row):
                    self.reply(ctx, "只能由加账本人回复错误记录撤销。最高权限可代处理。")
                    return
                self.storage.soft_delete_record(ctx.chat_id, row["id"], ctx.now, kind=kind)
                self.reply(ctx, "撤销成功")
                return
            self.reply(ctx, "没有找到被回复消息对应的记账记录。请回复自己发送的原始加账消息。")
            return
        if kind is None:
            self.reply(ctx, "请回复要撤销的错误记录。")
            return
        self.reply(ctx, "请回复要撤销的错误记录。")

    def find_record_by_message(self, chat_id: int, message_id: int) -> Any | None:
        return self.storage.conn.execute(
            """
            SELECT * FROM records
            WHERE chat_id = ?
              AND deleted_at IS NULL
              AND (bot_message_id = ? OR source_message_id = ?)
            ORDER BY id DESC
            LIMIT 1
            """,
            (chat_id, message_id, message_id),
        ).fetchone()

    def can_undo_record(self, ctx: MessageContext, row: Any) -> bool:
        return row["actor_user_id"] == ctx.user.user_id or self.storage.is_owner(ctx.chat_id, ctx.user.user_id)

    def add_or_remove_operator(self, ctx: MessageContext, *, add: bool, mentions: list[str]) -> None:
        if not self.require_operator_manager(ctx):
            return
        changed = 0
        if ctx.reply_user:
            if add:
                self.storage.add_operator(ctx.chat_id, ctx.reply_user, added_by=ctx.user.user_id, role="operator", now=ctx.now)
                changed += 1
            else:
                changed += self.storage.remove_operator(ctx.chat_id, user_id=ctx.reply_user.user_id)
        for username in mentions:
            if add:
                self.storage.add_operator_by_username(ctx.chat_id, username, added_by=ctx.user.user_id, now=ctx.now)
                changed += 1
            else:
                changed += self.storage.remove_operator(ctx.chat_id, username_norm=username)
        action = "添加" if add else "删除"
        if changed:
            self.reply(ctx, f"操作员已{action}成功。")
        else:
            self.reply(ctx, f"没有找到要{action}的操作员。可 @用户名，或回复对方消息后发送指令。")

    def show_operators(self, ctx: MessageContext) -> None:
        rows = self.storage.list_operators(ctx.chat_id)
        if not rows:
            self.reply(ctx, "当前没有单群操作员。宿主拥有最高权限，可发送“添加操作员 @user”设置操作员。")
            return
        lines = ["当前权限人："]
        for row in rows:
            role = "最高权限" if row["role"] == "owner" else "操作员"
            lines.append(f"{role}：{row['display_name']}")
        self.reply(ctx, "\n".join(lines))

    def notify_all(self, ctx: MessageContext) -> None:
        members = self.storage.recent_members(ctx.chat_id)
        mentions = []
        for member in members:
            if member["username"]:
                mentions.append(f"@{member['username']}")
            else:
                mentions.append(member["display_name"])
        self.reply(ctx, " ".join(mentions[:80]) if mentions else "暂无可通知成员。")

    def set_group_owner_if_missing(self, chat_id: int, user: TelegramUser, now: datetime) -> None:
        group = self.storage.get_group(chat_id)
        if group["owner_user_id"] == user.user_id:
            return
        self.storage.set_group_owner(chat_id, user, now=now)

    def ensure_host_present_or_leave(
        self,
        chat_id: int,
        chat_title: str | None,
        actor: TelegramUser,
        now: datetime,
    ) -> bool:
        host_user = self.find_host_in_group(chat_id, actor, now)
        if host_user is not None:
            self.set_group_owner_if_missing(chat_id, host_user, now)
            return True

        reason = "未配置宿主，机器人将自动退出。" if self.config.bot_host_user_id is None else "宿主不在群内，机器人将自动退出。"
        try:
            self.client.send_message(chat_id, reason)
        except TelegramAPIError:
            pass
        try:
            self.client.leave_chat(chat_id)
        finally:
            self.storage.forget_group(chat_id)
        print(f"Left group {chat_id} ({chat_title or 'untitled'}): host is not present", flush=True)
        return False

    def find_host_in_group(self, chat_id: int, actor: TelegramUser, now: datetime) -> TelegramUser | None:
        host_user_id = self.config.bot_host_user_id
        if host_user_id is None:
            return None
        if actor.user_id == host_user_id:
            return actor
        try:
            member = self.client.get_chat_member(chat_id, host_user_id)
        except TelegramRetryableError:
            raise
        except TelegramAPIError as exc:
            print(f"Could not verify host {host_user_id} in {chat_id}: {exc}", flush=True)
            return None
        status = member.get("status")
        if status in {"left", "kicked"}:
            return None
        user_data = member.get("user")
        if not user_data:
            return None
        host_user = user_from_telegram(user_data)
        self.storage.touch_user(chat_id, host_user, now)
        return host_user
        return None

    def format_expiration(self, group: Any) -> str:
        if not group["trial_until"]:
            return "还未激活试用。"
        expires = datetime.fromisoformat(group["trial_until"]).astimezone(self.config.timezone)
        return f"试用到期时间：{expires:%Y-%m-%d %H:%M:%S}"

    def handle_callback(self, callback: dict[str, Any]) -> None:
        data = callback.get("data") or ""
        message = callback.get("message") or {}
        chat = message.get("chat") or {}
        actor = callback.get("from")
        if not chat or not actor:
            return
        chat_id = int(chat["id"])
        now = datetime.now(self.config.timezone)
        user = user_from_telegram(actor)
        self.storage.ensure_group(chat_id, chat.get("title"), now)
        self.storage.apply_due_day_cutoff(chat_id, now, self.config.timezone)
        self.storage.touch_user(chat_id, user, now)
        if chat.get("type") in {"group", "supergroup"}:
            if not self.ensure_host_present_or_leave(chat_id, chat.get("title"), user, now):
                return
        fake_ctx = MessageContext(message=message, chat_id=chat_id, chat_title=chat.get("title"), user=user, text="", now=now)

        if data.startswith("watch:"):
            self.handle_address_watch_callback(chat_id, user.user_id, callback["id"], data, now)
            return

        if data.startswith("broadcast:"):
            self.handle_broadcast_callback(chat_id, user, callback["id"], data, now)
            return

        if data.startswith("undo:"):
            record_id = int(data.split(":", 1)[1])
            record = self.storage.get_record(record_id)
            if not self.can_undo_record(fake_ctx, record):
                self.client.answer_callback_query(callback["id"], "只能撤销自己加的账。")
                return
            deleted = self.storage.soft_delete_record(chat_id, record_id, now)
            self.client.answer_callback_query(callback["id"], "已撤销。" if deleted else "记录已不存在。")
            return

        if data.startswith("clear:"):
            if not self.require_operator(fake_ctx, callback_id=callback["id"]):
                return
            parts = data.split(":")
            scope = parts[1] if len(parts) > 1 else ""
            if scope == "cancel":
                self.client.answer_callback_query(callback["id"], "已取消。")
                return
            group = self.storage.get_group(chat_id)
            day_key = parts[2] if len(parts) >= 3 and parts[2] else business_day_key(now, group["day_cutoff_hour"], self.config.timezone)
            begin = self.parse_compact_bill_time(parts[3]) if len(parts) >= 4 else None
            end = self.parse_compact_bill_time(parts[4]) if len(parts) >= 5 else None
            if scope == "today":
                if self.day_cutoff_disabled(group):
                    count = self.storage.soft_delete_day(chat_id, day_key, now, all_days=True)
                elif begin is not None and end is not None:
                    count = self.storage.soft_delete_period(chat_id, begin, end, now)
                else:
                    begin, end = self.bill_window(group, day_key)
                    count = self.storage.soft_delete_period(chat_id, begin, end, now)
            else:
                count = self.storage.soft_delete_all(chat_id, now)
            self.client.answer_callback_query(callback["id"], f"已删除 {count} 条。")
            self.client.send_message(chat_id, f"已删除 {count} 条账单。")
            return

        if data == "bill:full" or data.startswith("bill:full:"):
            group = self.storage.get_group(chat_id)
            parts = data.split(":")
            day_key = parts[2] if len(parts) >= 3 and parts[2] else business_day_key(now, group["day_cutoff_hour"], self.config.timezone)
            begin = self.parse_compact_bill_time(parts[3]) if len(parts) >= 4 else None
            end = self.parse_compact_bill_time(parts[4]) if len(parts) >= 5 else None
            self.client.answer_callback_query(callback["id"])
            self.client.send_message(chat_id, self.build_bill_text(chat_id, day_key, begin=begin, end=end), parse_mode="HTML")

    def handle_broadcast_callback(
        self,
        chat_id: int,
        user: TelegramUser,
        callback_id: str,
        data: str,
        now: datetime,
    ) -> None:
        if not self.can_use_broadcast(user):
            self.client.answer_callback_query(callback_id, "没有广播权限。")
            return
        parts = data.split(":")
        if len(parts) != 3:
            self.client.answer_callback_query(callback_id, "广播任务无效。")
            return
        action, job_id_text = parts[1], parts[2]
        try:
            job = self.storage.get_broadcast_job(int(job_id_text))
        except (KeyError, ValueError):
            self.client.answer_callback_query(callback_id, "广播任务不存在。")
            return
        if int(job["creator_user_id"]) != user.user_id and not self.is_host(user.user_id):
            self.client.answer_callback_query(callback_id, "只能操作自己创建的广播。")
            return
        if job["status"] != "pending":
            self.client.answer_callback_query(callback_id, f"任务已是 {job['status']} 状态。")
            return
        if action == "cancel":
            self.storage.update_broadcast_job(job["id"], now, status="cancelled", completed_at=now.isoformat())
            self.client.answer_callback_query(callback_id, "已取消。")
            self.client.send_message(chat_id, "广播已取消。")
            return
        if action != "send":
            self.client.answer_callback_query(callback_id, "未知操作。")
            return

        self.client.answer_callback_query(callback_id, "开始发送。")
        self.storage.update_broadcast_job(job["id"], now, status="sending", confirmed_at=now.isoformat())
        success, failure = self.deliver_broadcast_job(job)
        self.storage.update_broadcast_job(
            job["id"],
            datetime.now(self.config.timezone),
            status="completed",
            success_count=success,
            failure_count=failure,
            completed_at=datetime.now(self.config.timezone).isoformat(),
        )
        self.client.send_message(chat_id, f"广播完成：成功 {success} 个，失败 {failure} 个。")

    def deliver_broadcast_job(self, job: Any) -> tuple[int, int]:
        target_chat_ids = [int(value) for value in json.loads(job["target_chat_ids"])]
        success = 0
        failure = 0
        for target_chat_id in target_chat_ids:
            try:
                self.send_broadcast_payload(job, target_chat_id)
                if int(job["notify_all"]):
                    self.send_notify_all_to_chat(target_chat_id)
                success += 1
            except TelegramAPIError as exc:
                failure += 1
                print(f"Broadcast failed for {target_chat_id}: {exc}", flush=True)
            time.sleep(0.05)
        return success, failure

    def send_broadcast_payload(self, job: Any, target_chat_id: int) -> None:
        if job["source_chat_id"] and job["source_message_id"]:
            self.client.copy_message(
                target_chat_id,
                int(job["source_chat_id"]),
                int(job["source_message_id"]),
            )
            return
        self.client.send_message(target_chat_id, job["text"])

    def send_notify_all_to_chat(self, chat_id: int) -> None:
        rows = self.storage.recent_members(chat_id, limit=200)
        mentions: list[str] = []
        for row in rows:
            username = row["username"]
            if username:
                mentions.append(f"@{username}")
            else:
                label = escape(row["display_name"] or str(row["user_id"]))
                mentions.append(f'<a href="tg://user?id={row["user_id"]}">{label}</a>')
        if not mentions:
            return
        chunks: list[list[str]] = [[]]
        for mention in mentions:
            if sum(len(item) + 1 for item in chunks[-1]) + len(mention) > 3300:
                chunks.append([])
            chunks[-1].append(mention)
        for index, chunk in enumerate(chunks):
            prefix = "通知所有人：\n" if index == 0 else ""
            self.client.send_message(chat_id, prefix + " ".join(chunk), parse_mode="HTML")

    def poll_address_watches_if_due(self) -> None:
        if self.config.tron_poll_interval_seconds <= 0:
            return
        now_monotonic = time.monotonic()
        if now_monotonic < self.next_tron_poll_at:
            return
        self.next_tron_poll_at = now_monotonic + self.config.tron_poll_interval_seconds
        self.poll_address_watches()

    def poll_address_watches(self) -> None:
        now = datetime.now(self.config.timezone)
        min_timestamp_ms = int(
            (now - timedelta(minutes=self.config.tron_initial_lookback_minutes)).timestamp() * 1000
        )
        for watch in self.storage.list_active_address_watch_targets():
            settings = self.storage.get_address_watch_settings(watch["owner_user_id"], now)
            try:
                rows = self.tron_client.fetch_trc20_transfers(
                    watch["address"],
                    contract_address=self.config.tron_usdt_contract,
                    min_timestamp_ms=min_timestamp_ms,
                )
            except TronGridError as exc:
                print(f"TronGrid error for {watch['address']}: {exc}", flush=True)
                continue

            for row in rows:
                transfer = parse_usdt_transfer(
                    row,
                    watched_address=watch["address"],
                    watched_label=watch["label"],
                    timezone=self.config.timezone,
                    usdt_contract=self.config.tron_usdt_contract,
                )
                if transfer is None:
                    continue
                inserted = self.storage.record_chain_event_notification(
                    owner_user_id=watch["owner_user_id"],
                    address=watch["address"],
                    tx_hash=transfer.tx_hash,
                    direction=transfer.direction,
                    token_symbol="USDT",
                    block_timestamp=int(row["block_timestamp"]),
                    now=now,
                )
                if not inserted:
                    continue
                if not should_notify_transfer(transfer, settings):
                    continue
                self.client.send_message(
                    watch["owner_user_id"],
                    format_transfer_notice(transfer),
                    parse_mode="HTML",
                )

    def handle_address_watch_callback(
        self,
        chat_id: int,
        owner_user_id: int,
        callback_id: str,
        data: str,
        now: datetime,
    ) -> None:
        settings = self.storage.get_address_watch_settings(owner_user_id, now)
        if data == "watch:prompt:add":
            self.address_watch_pending[owner_user_id] = "add"
            self.client.answer_callback_query(callback_id, "请发送地址和备注。")
            self.client.send_message(chat_id, "请发送要监听的 TRC20 地址，可在后面加备注：\nT地址 备注\n\n发送“取消”退出。")
            return
        if data == "watch:prompt:remove":
            self.address_watch_pending[owner_user_id] = "remove"
            self.client.answer_callback_query(callback_id, "请发送要删除的地址。")
            self.client.send_message(chat_id, "请发送要删除的监听地址：\nT地址\n\n发送“取消”退出。")
            return
        if data == "watch:prompt:label":
            self.address_watch_pending[owner_user_id] = "label"
            self.client.answer_callback_query(callback_id, "请发送地址和备注。")
            self.client.send_message(chat_id, "请发送要设置备注的地址和备注：\nT地址 新备注\n\n只发送地址可清空备注，发送“取消”退出。")
            return
        if data == "watch:prompt:min":
            self.address_watch_pending[owner_user_id] = "min"
            self.client.answer_callback_query(callback_id, "请发送最小提醒金额。")
            self.client.send_message(chat_id, "请发送最小提醒金额，单位 USDT。\n例如：10\n设置 0 表示不限制。\n\n发送“取消”退出。")
            return
        if data == "watch:mode:compact":
            self.storage.update_address_watch_settings(owner_user_id, now, display_mode="compact")
            text = "已切换精简模式。"
        elif data == "watch:mode:full":
            self.storage.update_address_watch_settings(owner_user_id, now, display_mode="full")
            text = "已切换完整模式。"
        elif data == "watch:toggle:income":
            value = 0 if settings["watch_income"] else 1
            self.storage.update_address_watch_settings(owner_user_id, now, watch_income=value)
            text = "收入监听已开启。" if value else "收入监听已关闭。"
        elif data == "watch:toggle:expense":
            value = 0 if settings["watch_expense"] else 1
            self.storage.update_address_watch_settings(owner_user_id, now, watch_expense=value)
            text = "支出监听已开启。" if value else "支出监听已关闭。"
        elif data == "watch:toggle:trx":
            value = 0 if settings["notify_trx"] else 1
            self.storage.update_address_watch_settings(owner_user_id, now, notify_trx=value)
            text = "TRX通知已开启。" if value else "TRX通知已关闭。"
        else:
            text = "未知设置。"
        self.client.answer_callback_query(callback_id, text)
        self.send_address_watch_menu(chat_id, owner_user_id)

    def _set_permissions(self, ctx: MessageContext, can_send: bool, prefix: str | None = None) -> None:
        try:
            self.client.set_chat_permissions(ctx.chat_id, can_send)
            text = prefix or ("已上课，全员可发送消息。" if can_send else "已下课，全员禁止发送消息。")
        except TelegramAPIError:
            text = "设置失败。请确认机器人已设为群管理员，并有管理群权限。"
        self.reply(ctx, text)

    def can_record(self, ctx: MessageContext, group: Any) -> bool:
        if group["all_members_can_record"]:
            return True
        return self.require_operator(ctx)

    def require_operator(self, ctx: MessageContext, callback_id: str | None = None) -> bool:
        ok = self.is_host(ctx.user.user_id) or self.is_default_operator(ctx.user) or self.storage.is_operator(ctx.chat_id, ctx.user)
        if ok:
            return True
        text = "没有操作权限。请管理员添加操作员。"
        if callback_id:
            self.client.answer_callback_query(callback_id, text)
        else:
            self.reply(ctx, text)
        return False

    def require_operator_manager(self, ctx: MessageContext) -> bool:
        if self.is_host(ctx.user.user_id) or self.storage.is_owner(ctx.chat_id, ctx.user.user_id):
            return True
        self.reply(ctx, "只有宿主或本群最高权限可以设置单群操作员。")
        return False

    def is_host(self, user_id: int) -> bool:
        return user_id == self.config.bot_host_user_id

    def is_default_operator(self, user: TelegramUser) -> bool:
        return user.user_id in self.config.default_operator_user_ids

    def can_invite_bot(self, user: TelegramUser) -> bool:
        return self.is_host(user.user_id) or self.is_default_operator(user)

    def can_use_broadcast(self, user: TelegramUser) -> bool:
        return self.is_host(user.user_id) or self.is_default_operator(user)

    def group_for_time(self, chat_id: int, now: datetime) -> Any:
        return self.storage.apply_due_day_cutoff(chat_id, now, self.config.timezone)

    def day_key(self, ctx: MessageContext) -> str:
        group = self.group_for_time(ctx.chat_id, ctx.now)
        return current_business_day_key(ctx.now, group, self.config.timezone)

    def bill_window(self, group: Any, day_key: str, cutoff_hour: int | None = None) -> tuple[datetime, datetime]:
        return bill_window_for_day(group, day_key, self.config.timezone, cutoff_hour=cutoff_hour)

    def records_for_bill(
        self,
        chat_id: int,
        day_key: str,
        group: Any,
        *,
        cutoff_hour: int | None = None,
        begin: datetime | None = None,
        end: datetime | None = None,
    ) -> list[Any]:
        if self.day_cutoff_disabled(group) or day_key in {"active", "all"}:
            return self.storage.list_records_for_day(chat_id, day_key, all_days=True)
        if begin is None or end is None:
            begin, end = self.bill_window(group, day_key, cutoff_hour)
        return self.storage.list_records_for_period(chat_id, begin, end)

    def compact_bill_time(self, value: datetime) -> str:
        return value.astimezone(self.config.timezone).strftime("%Y%m%d%H")

    def parse_compact_bill_time(self, value: str) -> datetime | None:
        if not value:
            return None
        try:
            return datetime.strptime(value, "%Y%m%d%H").replace(tzinfo=self.config.timezone)
        except ValueError:
            return None

    def cutoff_set_reply(self, group: Any, hour: int) -> str:
        if hour < 0:
            return "日切已关闭。"
        pending = group["pending_day_cutoff_hour"]
        effective_at = group["pending_day_cutoff_effective_at"]
        if pending is not None and effective_at:
            effective = datetime.fromisoformat(effective_at).astimezone(self.config.timezone)
            return f"设置成功，当前账期将延续到 {effective:%Y-%m-%d %H:%M}，之后日切时间为：{hour}点"
        return f"设置成功，当前日切时间为：{hour}点"

    @staticmethod
    def day_cutoff_disabled(group: Any) -> bool:
        return int(group["day_cutoff_hour"]) < 0

    def reply(self, ctx: MessageContext, text: str) -> None:
        self.client.send_message(ctx.chat_id, text)


def calculate_amounts(
    *,
    kind: str,
    amount: Decimal,
    currency: str,
    rate: Decimal,
    fee_rate: Decimal,
    payout_mode: str,
    multiply_exchange: bool,
) -> tuple[Decimal, Decimal, Decimal, Decimal]:
    if currency == "USDT":
        amount_usdt = amount
        amount_cny = amount * rate
    elif kind == "payout" and payout_mode == "coin":
        amount_usdt = amount
        amount_cny = amount * rate
    elif multiply_exchange:
        amount_cny = amount
        amount_usdt = amount * rate
    else:
        amount_cny = amount
        amount_usdt = amount / rate if rate != 0 else Decimal("0")
    commission_cny = amount_cny * fee_rate / Decimal("100")
    net_usdt = (amount_cny - commission_cny) / rate if rate != 0 else Decimal("0")
    return quant(amount_cny), quant(amount_usdt), quant(net_usdt), quant(commission_cny)


def quant(value: Decimal, places: str = "0.000001") -> Decimal:
    return value.quantize(Decimal(places), rounding=ROUND_HALF_UP)


def format_money(value: Decimal) -> str:
    return format_number(value.quantize(Decimal("0.01"), rounding=ROUND_HALF_UP))


def format_fixed_2(value: Decimal) -> str:
    return f"{value.quantize(Decimal('0.01'), rounding=ROUND_HALF_UP):f}"


def format_fee_multiplier(fee_rate: Decimal) -> str:
    multiplier = Decimal("1") - fee_rate / Decimal("100")
    return format_number(multiplier.quantize(Decimal("0.01"), rounding=ROUND_HALF_UP))


def format_number(value: Decimal) -> str:
    normalized = value.normalize()
    text = f"{normalized:f}"
    if "." in text:
        text = text.rstrip("0").rstrip(".")
    return text or "0"


def format_signed_decimal(value: Decimal) -> str:
    if value == 0:
        return "0"
    if value > 0:
        return f"+{format_number(value)}"
    return format_number(value)


def format_realtime_offset_label(value: Any) -> str:
    offset = Decimal(str(value or "0"))
    if offset == 0:
        return ""
    direction = "上浮" if offset > 0 else "下浮"
    return f"{direction}{format_fixed_2(abs(offset))}"


def p2p_trade_method_label(value: str) -> str:
    normalized = value.replace("_", "").replace("-", "").lower()
    labels = {
        "alipay": "支付宝",
        "ali": "支付宝",
        "wxpay": "微信",
        "wechat": "微信",
        "wechatpay": "微信",
        "bank": "银行卡",
        "bankcard": "银行卡",
    }
    return labels.get(normalized, value)


def join_non_empty(*parts: str) -> str:
    return " ".join(part for part in parts if part)


def parse_non_negative_decimal(value: str) -> Decimal | None:
    try:
        amount = Decimal(value)
    except InvalidOperation:
        return None
    if amount < 0:
        return None
    return amount


def sum_decimal(values: Any) -> Decimal:
    total = Decimal("0")
    for value in values:
        total += Decimal(value)
    return total


def detect_usdt_network(address: str) -> str | None:
    if re.fullmatch(r"T[1-9A-HJ-NP-Za-km-z]{33}", address):
        return "TRC20"
    return None


def extract_tron_address(text: str) -> str | None:
    match = re.search(r"T[1-9A-HJ-NP-Za-km-z]{33}", text)
    return match.group(0) if match else None
