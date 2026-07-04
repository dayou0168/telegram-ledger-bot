from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime, timedelta
from decimal import Decimal, ROUND_HALF_UP
import re
import time
from typing import Any
from urllib.parse import urlencode

from .address_watch import format_transfer_notice, should_notify_transfer
from .calculator import CalculatorError, calculate_expression, format_calculation_result, is_arithmetic_expression
from .config import Config
from .p2p_rates import P2POrderBookEntry, P2PRateClient, P2PRateError
from .parser import ParsedCommand, ParsedLedgerEntry, parse_message
from .storage import Storage, TelegramUser, business_day_key, business_day_range, user_from_telegram
from .telegram_api import TelegramAPIError, TelegramClient, run_with_backoff
from .tron_api import TronGridClient, TronGridError, parse_usdt_transfer


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
        self.offset: int | None = None
        self.next_tron_poll_at = 0.0

    def run_forever(self) -> None:
        print("Ledger bot is running.", flush=True)
        run_with_backoff(self._poll_once)

    def _poll_once(self) -> None:
        updates = self.client.get_updates(self.offset, self.telegram_poll_timeout())
        for update in updates:
            self.offset = int(update["update_id"]) + 1
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
        self.storage.ensure_group(int(chat["id"]), chat.get("title"), now)

    def handle_message(self, message: dict[str, Any]) -> None:
        text = (message.get("text") or message.get("caption") or "").strip()
        if not text:
            return

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
        self.storage.touch_user(ctx.chat_id, user, now)
        if ctx.reply_user:
            self.storage.touch_user(ctx.chat_id, ctx.reply_user, now)

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
                    reply_to_message_id=ctx.message_id,
                )
            return

        if isinstance(parsed, ParsedCommand):
            self.handle_command(ctx, parsed)
        else:
            self.handle_ledger_entry(ctx, parsed)

    def handle_command(self, ctx: MessageContext, command: ParsedCommand) -> None:
        group = self.storage.get_group(ctx.chat_id)
        if command.name in MANAGEMENT_COMMANDS and command.name != "start":
            if not self.require_operator(ctx):
                return

        match command.name:
            case "start":
                self.storage.activate_group(ctx.chat_id, ctx.user, ctx.now)
                self.client.send_message(
                    ctx.chat_id,
                    "机器人已开启，请开始记账",
                    reply_to_message_id=ctx.message_id,
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
                self.storage.update_group(ctx.chat_id, ctx.now, deposit_exchange_rate=str(command.args["exchange_rate"]))
                self.reply(ctx, f"固定汇率设置成功，当前固定汇率为： {format_number(command.args['exchange_rate'])}")
            case "set_payout_exchange_rate":
                self.storage.update_group(ctx.chat_id, ctx.now, payout_exchange_rate=str(command.args["exchange_rate"]))
                self.reply(ctx, f"下发汇率已设置为 {format_number(command.args['exchange_rate'])}。")
            case "set_cutoff":
                hour = int(command.args["hour"])
                if hour != -1 and not 0 <= hour <= 23:
                    self.reply(ctx, "日切时间只能是 0 到 23，或 -1 关闭日切。")
                    return
                self.storage.update_group(ctx.chat_id, ctx.now, day_cutoff_hour=hour)
                self.reply(ctx, "日切已关闭。" if hour == -1 else f"设置成功，当前日切时间为：{hour}点")
            case "cutoff_off" | "global_cutoff_off":
                self.storage.update_group(ctx.chat_id, ctx.now, day_cutoff_hour=-1)
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
                changed = self.storage.update_day_exchange_rate(
                    ctx.chat_id,
                    day_key,
                    rate,
                    ctx.now,
                    all_days=self.day_cutoff_disabled(group),
                )
                self.storage.update_group(ctx.chat_id, ctx.now, deposit_exchange_rate=rate)
                self.reply(ctx, f"已同步今日账单汇率为 {format_number(Decimal(rate))}，更新 {changed} 条。")
            case "pin_on":
                self.storage.update_group(ctx.chat_id, ctx.now, pin_enabled=1)
                self.reply(ctx, "记账置顶已开启。")
            case "pin_off":
                self.storage.update_group(ctx.chat_id, ctx.now, pin_enabled=0)
                self.reply(ctx, "记账置顶已关闭。")
            case "realtime_rate_on":
                offset = command.args.get("offset", Decimal("0"))
                self.storage.update_group(ctx.chat_id, ctx.now, realtime_rate=1, realtime_rate_offset=str(offset))
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
                self.reply(ctx, "查询接口还未接入。可以先正常记账，OTC/银行卡/地址查询会作为独立模块添加。")
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

        methods = self.config.p2p_rate_trade_methods
        method_text = "、".join(methods) if methods else "全部支付方式"
        lines = [
            f"OKX {self.config.p2p_rate_fiat_unit}买{self.config.p2p_rate_asset} TOP10",
            f"支付方式：{method_text}",
        ]
        for entry in entries:
            limits = self.format_entry_limits(entry)
            merchant = self.trim_text(entry.merchant_name, 10)
            lines.append(f"Z{entry.rank}  {format_number(entry.price)}  {merchant}{limits}")
        lines.append("")
        lines.append("发送 Z1 -0.1 或 Z1 +0.1 可按第1档偏移后设置汇率。")
        self.reply(ctx, "\n".join(lines))

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

    @staticmethod
    def format_entry_limits(entry: P2POrderBookEntry) -> str:
        if entry.limit_min is None or entry.limit_max is None:
            return ""
        return f"  限额{format_number(entry.limit_min)}-{format_number(entry.limit_max)}"

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
        if normalized in {"✍开始记账", "开始记账"}:
            self.send_start_group_help(chat_id, message_id)
            return
        if normalized in {"📃详细说明", "详细说明"}:
            self.client.send_message(chat_id, self.private_help_text(), reply_to_message_id=message_id)
            return
        if normalized in {"📣分组广播", "分组广播"}:
            self.client.send_message(chat_id, "分组广播用于按群分组发送通知，后台模块接入后启用。", reply_to_message_id=message_id)
            return
        if normalized in {"💵自助续费", "自助续费"}:
            self.client.send_message(chat_id, "自助续费入口已预留，后续可接支付或卡密。", reply_to_message_id=message_id)
            return
        if normalized in {"🔔地址监听", "地址监听", "监听地址"}:
            self.send_address_watch_menu(chat_id, user.user_id, message_id)
            return
        if normalized in {"📡群发广播", "群发广播"}:
            self.client.send_message(chat_id, "群发广播用于通知机器人所在群，后台模块接入后启用。", reply_to_message_id=message_id)
            return
        if normalized in {"🛠功能设置", "功能设置"}:
            self.client.send_message(chat_id, "功能设置已预留：记账置顶、日切、汇率模式、地址识别等会集中到这里。", reply_to_message_id=message_id)
            return
        if normalized in {"📒账单统计", "账单统计"}:
            self.client.send_message(chat_id, "账单统计已预留：后续可按群、日期、操作人、备注汇总。", reply_to_message_id=message_id)
            return
        if self.handle_address_watch_command(chat_id, user.user_id, normalized, now, message_id):
            return

        self.send_private_menu(chat_id, message_id)

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
                "设置费率3",
                "+1000",
                "+1000/7.1",
                "下发100U",
                "+0",
                "显示账单",
                "",
                "撤销：回复机器人记账回执，发送“撤销入款”或“撤销下发”。",
            ]
        )

    def send_address_watch_menu(
        self,
        chat_id: int,
        owner_user_id: int,
        reply_to_message_id: int | None = None,
    ) -> None:
        rows = self.storage.list_address_watches(owner_user_id)
        settings = self.storage.get_address_watch_settings(owner_user_id, datetime.now(self.config.timezone))
        lines = [
            "USDT 地址监听",
            f"模式：{'精简模式' if settings['display_mode'] == 'compact' else '完整模式'}",
            f"收入：{'开启' if settings['watch_income'] else '关闭'}",
            f"支出：{'开启' if settings['watch_expense'] else '关闭'}",
            f"TRX通知：{'开启' if settings['notify_trx'] else '关闭'}",
            "",
            "添加：添加监听地址 Txxxx 备注",
            "删除：删除监听地址 Txxxx",
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
        return {
            "inline_keyboard": [
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
        add_match = re.fullmatch(r"(?:添加监听地址|监听)\s+(\S+)(?:\s+(.+))?", text)
        if add_match:
            address = add_match.group(1).strip()
            network = detect_usdt_network(address)
            if not network:
                self.client.send_message(
                    chat_id,
                    "地址格式不支持。USDT 监听当前只支持 TRC20 的 T 开头地址。",
                    reply_to_message_id=reply_to_message_id,
                )
                return True
            label = (add_match.group(2) or "").strip() or None
            self.storage.add_address_watch(owner_user_id, network, address, label, now)
            self.client.send_message(chat_id, "监听地址已添加。", reply_to_message_id=reply_to_message_id)
            return True

        remove_match = re.fullmatch(r"(?:删除监听地址|取消监听)\s+(\S+)", text)
        if remove_match:
            address = remove_match.group(1).strip()
            removed = self.storage.remove_address_watch(owner_user_id, address)
            self.client.send_message(
                chat_id,
                "监听地址已删除。" if removed else "没有找到这个监听地址。",
                reply_to_message_id=reply_to_message_id,
            )
            return True
        return False

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
        label = "入款" if entry.kind == "deposit" else "下发"
        reply = f"已记录{label}：{format_number(Decimal(record['amount_cny']))} / {format_number(Decimal(record['exchange_rate']))}={format_money(Decimal(record['amount_usdt']))}U"
        sent = self.client.send_message(
            ctx.chat_id,
            reply + "\n\n" + self.build_bill_text(ctx.chat_id, self.day_key(ctx), compact=True),
            reply_to_message_id=ctx.message_id,
            reply_markup={
                "inline_keyboard": [
                    self.bill_button_row(ctx.chat_id, self.day_key(ctx)),
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
        text = self.build_bill_text(ctx.chat_id, self.day_key(ctx), only_user_id=only_user_id, compact=compact)
        self.client.send_message(
            ctx.chat_id,
            text,
            reply_to_message_id=ctx.message_id,
            reply_markup={"inline_keyboard": [self.bill_button_row(ctx.chat_id, self.day_key(ctx))]},
        )

    def build_bill_text(
        self,
        chat_id: int,
        day_key: str,
        *,
        only_user_id: int | None = None,
        compact: bool = False,
    ) -> str:
        group = self.storage.get_group(chat_id)
        rows = self.storage.list_records_for_day(chat_id, day_key, all_days=self.day_cutoff_disabled(group))
        if only_user_id is not None:
            rows = [row for row in rows if row["actor_user_id"] == only_user_id or row["subject_user_id"] == only_user_id]
        deposits = [row for row in rows if row["kind"] == "deposit"]
        payouts = [row for row in rows if row["kind"] == "payout"]
        limit = self.bill_limit(group, compact)
        shown_deposits = deposits[-limit:] if limit else deposits
        shown_payouts = payouts[-limit:] if limit else payouts

        lines = [f"今日入款（{len(deposits)}笔）"]
        lines.extend(self.format_record_line(row) for row in shown_deposits)
        lines.append("")
        lines.append(f"今日下发（{len(payouts)}笔）")
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
            f"汇率：{format_number(exchange)}",
            f"交易费率：{format_number(fee_rate)}%",
            "",
            f"应下发：{due}",
            f"已下发：{paid}",
            f"余额：{balance_text}",
        ])
        return "\n".join(lines)

    def format_record_line(self, row: Any) -> str:
        created = datetime.fromisoformat(row["created_at"]).astimezone(self.config.timezone)
        amount = Decimal(row["amount_cny"])
        rate = Decimal(row["exchange_rate"])
        gross_usdt = Decimal(row["amount_usdt"])
        net_usdt = Decimal(row["net_usdt"])
        fee_rate = Decimal(row["fee_rate"])
        subject = row["subject_name"] or row["actor_name"]
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
        return [{"text": "🌐 完整账单", "callback_data": "bill:full"}]

    def build_bill_url(self, chat_id: int, day_key: str) -> str | None:
        group = self.storage.get_group(chat_id)
        if self.day_cutoff_disabled(group):
            begin_time = ""
            end_time = ""
            all_flag = "1"
            bill_key = "active"
        else:
            begin, end = business_day_range(day_key, group["day_cutoff_hour"], self.config.timezone)
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
        return f"{base}/bill/{values['chat_id']}/{values['day_key']}"

    def ask_clear_confirm(self, ctx: MessageContext, scope: str) -> None:
        label = "今日账单" if scope == "today" else "全部账单"
        self.client.send_message(
            ctx.chat_id,
            f"确认删除{label}？此操作会软删除流水。",
            reply_to_message_id=ctx.message_id,
            reply_markup={
                "inline_keyboard": [
                    [
                        {"text": "确认删除", "callback_data": f"clear:{scope}"},
                        {"text": "取消", "callback_data": "clear:cancel"},
                    ]
                ]
            },
        )

    def undo_by_reply_or_last(self, ctx: MessageContext, kind: str | None) -> None:
        if ctx.reply_message_id:
            row = self.find_record_by_bot_message(ctx.chat_id, ctx.reply_message_id)
            if row and (kind is None or row["kind"] == kind):
                if not self.can_undo_record(ctx, row):
                    self.reply(ctx, "只能由加账本人回复错误记录撤销。最高权限可代处理。")
                    return
                self.storage.soft_delete_record(ctx.chat_id, row["id"], ctx.now, kind=kind)
                self.reply(ctx, "撤销成功")
                return
            self.reply(ctx, "没有找到被回复消息对应的记账记录。请回复机器人记账回执。")
            return
        if kind is None:
            self.reply(ctx, "请回复要撤销的错误记录。")
            return
        self.reply(ctx, "请回复要撤销的错误记录。")

    def find_record_by_bot_message(self, chat_id: int, message_id: int) -> Any | None:
        return self.storage.conn.execute(
            """
            SELECT * FROM records
            WHERE chat_id = ?
              AND deleted_at IS NULL
              AND bot_message_id = ?
            ORDER BY id DESC
            LIMIT 1
            """,
            (chat_id, message_id),
        ).fetchone()

    def can_undo_record(self, ctx: MessageContext, row: Any) -> bool:
        return row["actor_user_id"] == ctx.user.user_id or self.storage.is_owner(ctx.chat_id, ctx.user.user_id)

    def add_or_remove_operator(self, ctx: MessageContext, *, add: bool, mentions: list[str]) -> None:
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
            self.reply(ctx, "当前没有操作员。发送“开始”的用户会成为最高权限。")
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
        self.storage.touch_user(chat_id, user, now)
        fake_ctx = MessageContext(message=message, chat_id=chat_id, chat_title=chat.get("title"), user=user, text="", now=now)

        if data.startswith("watch:"):
            self.handle_address_watch_callback(chat_id, user.user_id, callback["id"], data, now)
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
            scope = data.split(":", 1)[1]
            if scope == "cancel":
                self.client.answer_callback_query(callback["id"], "已取消。")
                return
            group = self.storage.get_group(chat_id)
            day_key = business_day_key(now, group["day_cutoff_hour"], self.config.timezone)
            if scope == "today":
                count = self.storage.soft_delete_day(
                    chat_id,
                    day_key,
                    now,
                    all_days=self.day_cutoff_disabled(group),
                )
            else:
                count = self.storage.soft_delete_all(chat_id, now)
            self.client.answer_callback_query(callback["id"], f"已删除 {count} 条。")
            self.client.send_message(chat_id, f"已删除 {count} 条账单。")
            return

        if data == "bill:full":
            group = self.storage.get_group(chat_id)
            day_key = business_day_key(now, group["day_cutoff_hour"], self.config.timezone)
            self.client.answer_callback_query(callback["id"])
            self.client.send_message(chat_id, self.build_bill_text(chat_id, day_key))

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
                if transfer is None or not should_notify_transfer(transfer.direction, settings):
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
        ok = self.storage.is_operator(ctx.chat_id, ctx.user)
        if ok:
            return True
        text = "没有操作权限。请管理员添加操作员。"
        if callback_id:
            self.client.answer_callback_query(callback_id, text)
        else:
            self.reply(ctx, text)
        return False

    def day_key(self, ctx: MessageContext) -> str:
        group = self.storage.get_group(ctx.chat_id)
        return business_day_key(ctx.now, group["day_cutoff_hour"], self.config.timezone)

    @staticmethod
    def day_cutoff_disabled(group: Any) -> bool:
        return int(group["day_cutoff_hour"]) < 0

    def reply(self, ctx: MessageContext, text: str) -> None:
        self.client.send_message(ctx.chat_id, text, reply_to_message_id=ctx.message_id)


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


def sum_decimal(values: Any) -> Decimal:
    total = Decimal("0")
    for value in values:
        total += Decimal(value)
    return total


def detect_usdt_network(address: str) -> str | None:
    if re.fullmatch(r"T[1-9A-HJ-NP-Za-km-z]{33}", address):
        return "TRC20"
    return None
