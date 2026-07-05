from __future__ import annotations

from collections import defaultdict
from dataclasses import dataclass, field
from datetime import datetime, timedelta, tzinfo
from decimal import Decimal
from html import escape
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from io import BytesIO
import re
import threading
from typing import Any
from urllib.parse import parse_qs, quote, unquote, urlencode, urlparse
from zipfile import ZIP_DEFLATED, ZipFile

from .bot import (
    format_fee_multiplier,
    format_money,
    format_number,
    format_realtime_offset_label,
    join_non_empty,
    p2p_trade_method_label,
    sum_decimal,
)
from .config import Config
from .storage import Storage, bill_window_for_day, business_day_range, current_business_day_key, parse_stored_datetime


@dataclass
class BillWebResponse:
    status: int
    body: bytes
    content_type: str = "text/html; charset=utf-8"
    headers: dict[str, str] = field(default_factory=dict)


@dataclass
class BillPageData:
    group: Any
    chat_id: int
    day_key: str
    title_day: str
    group_title: str
    all_days: bool
    begin_time: str
    end_time: str
    rows: list[Any]
    deposits: list[Any]
    payouts: list[Any]
    use_day_key_records: bool = False


def start_bill_web_server(config: Config) -> ThreadingHTTPServer | None:
    if not config.bill_web_enabled or config.bill_web_port <= 0:
        return None

    handler_cls = make_bill_request_handler(config)
    server = ThreadingHTTPServer((config.bill_web_host, config.bill_web_port), handler_cls)
    server.daemon_threads = True
    thread = threading.Thread(target=server.serve_forever, name="bill-web", daemon=True)
    thread.start()
    print(f"Bill web server listening on {config.bill_web_host}:{config.bill_web_port}", flush=True)
    return server


def make_bill_request_handler(config: Config) -> type[BaseHTTPRequestHandler]:
    class BillRequestHandler(BaseHTTPRequestHandler):
        server_version = "LedgerBillWeb/1.0"

        def do_GET(self) -> None:
            self.write_response(include_body=True)

        def do_HEAD(self) -> None:
            self.write_response(include_body=False)

        def write_response(self, *, include_body: bool) -> None:
            response = handle_bill_web_response(config, self.path)
            self.send_response(response.status)
            self.send_header("Content-Type", response.content_type)
            self.send_header("Content-Length", str(len(response.body)))
            for key, value in response.headers.items():
                self.send_header(key, value)
            self.end_headers()
            if include_body:
                self.wfile.write(response.body)

        def log_message(self, format: str, *args: Any) -> None:
            print(f"bill-web: {format % args}", flush=True)

    return BillRequestHandler


def handle_bill_web_request(config: Config, raw_path: str) -> tuple[int, str]:
    response = handle_bill_web_response(config, raw_path)
    try:
        body = response.body.decode("utf-8")
    except UnicodeDecodeError:
        body = ""
    return response.status, body


def handle_bill_web_response(config: Config, raw_path: str) -> BillWebResponse:
    parsed = urlparse(raw_path)
    query = parse_qs(parsed.query)
    path = parsed.path.rstrip("/") or "/"

    if path == "/health":
        return html_response(HTTPStatus.OK, simple_page("OK", "<p>ok</p>"))
    if path == "/":
        return html_response(HTTPStatus.OK, simple_page("账单服务", "<p>账单网页服务已启动。</p>"))

    if not bill_web_token_allowed(config, query):
        return html_response(HTTPStatus.FORBIDDEN, simple_page("访问受限", "<p>链接无效或缺少访问令牌。</p>"))

    segments = [unquote(part) for part in path.strip("/").split("/") if part]
    if not segments or segments[0] not in {"bill", "day_xxb.php"}:
        return html_response(HTTPStatus.NOT_FOUND, simple_page("404", "<p>页面不存在。</p>"))

    try:
        if segments[0] == "day_xxb.php":
            chat_id = int((query.get("chat_id") or [""])[0])
            day_key = day_key_from_legacy_query(query)
        elif len(segments) >= 3:
            chat_id = int(segments[1])
            day_key = segments[2]
        else:
            chat_id = int((query.get("chat_id") or [""])[0])
            day_key = (query.get("day_key") or query.get("date") or ["today"])[0] or "today"
    except ValueError:
        return html_response(HTTPStatus.BAD_REQUEST, simple_page("参数错误", "<p>账单链接参数不正确。</p>"))

    storage = Storage(config.db_path)
    try:
        search_text = (query.get("firstname") or [""])[0]
        search_type = (query.get("type") or ["bjr"])[0]
        begin_time = (query.get("begintime") or query.get("begin_time") or [""])[0]
        end_time = (query.get("endtime") or query.get("end_time") or [""])[0]
        use_day_key_records = bool((query.get("created_at") or [""])[0] and not begin_time and not end_time)
        if is_download_query(query):
            data = load_bill_page_data(
                storage,
                chat_id,
                day_key,
                timezone=config.timezone,
                search_text=search_text,
                search_type=search_type,
                begin_time=begin_time,
                end_time=end_time,
                use_day_key_records=use_day_key_records,
            )
            filename = xlsx_filename(data.day_key, data.group_title, config.timezone)
            return BillWebResponse(
                HTTPStatus.OK,
                build_bill_xlsx(data, config.timezone, config.p2p_rate_trade_methods),
                content_type="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
                headers={
                    "Content-Disposition": content_disposition(filename),
                    "Cache-Control": "no-store",
                },
            )
        body = render_bill_page(
            storage,
            chat_id,
            day_key,
            timezone=config.timezone,
            trade_methods=config.p2p_rate_trade_methods,
            token=config.bill_web_token,
            search_text=search_text,
            search_type=search_type,
            begin_time=begin_time,
            end_time=end_time,
            use_day_key_records=use_day_key_records,
        )
    except KeyError:
        return html_response(HTTPStatus.NOT_FOUND, simple_page("未找到群组", "<p>没有找到这个群的账单。</p>"))
    finally:
        storage.conn.close()
    return html_response(HTTPStatus.OK, body)


def html_response(status: int, body: str) -> BillWebResponse:
    return BillWebResponse(int(status), body.encode("utf-8"))


def bill_web_token_allowed(config: Config, query: dict[str, list[str]]) -> bool:
    if not config.bill_web_token:
        return True
    return (query.get("token") or [""])[0] == config.bill_web_token


def day_key_from_legacy_query(query: dict[str, list[str]]) -> str:
    if (query.get("all") or [""])[0]:
        return "active"
    for key in ("day_key", "date", "created_at", "begintime", "begin_time"):
        value = (query.get(key) or [""])[0]
        if len(value) >= 10:
            return value[:10]
    return "today"


def is_download_query(query: dict[str, list[str]]) -> bool:
    for key in ("download", "export"):
        value = (query.get(key) or [""])[0].strip().casefold()
        if value in {"1", "true", "excel", "xlsx", "xls", "download"}:
            return True
    return (query.get("action") or [""])[0].strip().casefold() in {"download", "export"}


def render_bill_page(
    storage: Storage,
    chat_id: int,
    day_key: str,
    *,
    timezone: tzinfo,
    trade_methods: tuple[str, ...] = (),
    token: str | None = None,
    now: datetime | None = None,
    search_text: str = "",
    search_type: str = "bjr",
    begin_time: str = "",
    end_time: str = "",
    use_day_key_records: bool = False,
) -> str:
    data = load_bill_page_data(
        storage,
        chat_id,
        day_key,
        timezone=timezone,
        now=now,
        search_text=search_text,
        search_type=search_type,
        begin_time=begin_time,
        end_time=end_time,
        use_day_key_records=use_day_key_records,
    )

    content = f"""
    <div class="content-wrapper">
      <div class="container">
        {render_bill_toolbar(
            storage,
            data,
            token,
            cutoff_hour=int(data.group["day_cutoff_hour"]),
            timezone=timezone,
            search_text=search_text,
            search_type=search_type,
        )}
        {render_legacy_forms(
            chat_id,
            data.day_key,
            token,
            cutoff_hour=int(data.group["day_cutoff_hour"]),
            timezone=timezone,
            search_text=search_text,
            search_type=search_type,
            begin_time=data.begin_time,
            end_time=data.end_time,
            created_at=data.day_key if data.use_day_key_records else "",
        )}
        <section class="content">
          {render_record_table("入款", data.deposits, timezone)}
          {render_record_table("下发", data.payouts, timezone)}
          {render_people_stats_box("统计（按标记人）", data.deposits, data.payouts, "subject")}
          {render_people_stats_box("统计（按操作人）", data.deposits, data.payouts, "actor")}
          {render_people_stats_box("统计（按备注）", data.deposits, data.payouts, "note")}
          {render_rate_stats_box(data.deposits)}
          {render_summary_block(data.group, data.deposits, data.payouts, trade_methods)}
          {render_footer_links(
            chat_id,
            data.day_key,
            token,
            int(data.group["day_cutoff_hour"]),
            timezone,
            search_text=search_text,
            search_type=search_type,
            begin_time=data.begin_time,
            end_time=data.end_time,
            all_days=data.all_days,
            use_created_at_only=data.use_day_key_records,
          )}
        </section>
      </div>
    </div>
    """
    return page_shell(f"{data.group_title} - {data.title_day}", content)


def load_bill_page_data(
    storage: Storage,
    chat_id: int,
    day_key: str,
    *,
    timezone: tzinfo,
    now: datetime | None = None,
    search_text: str = "",
    search_type: str = "bjr",
    begin_time: str = "",
    end_time: str = "",
    use_day_key_records: bool = False,
) -> BillPageData:
    now = now or datetime.now(timezone)
    storage.apply_due_day_cutoff(chat_id, now, timezone)
    group = storage.get_group(chat_id)
    if day_key in {"today", ""}:
        day_key = current_business_day_key(now, group, timezone)
    all_days = int(group["day_cutoff_hour"]) < 0 or day_key in {"active", "all"}
    linked_begin = parse_stored_datetime(begin_time, timezone)
    linked_end = parse_stored_datetime(end_time, timezone)
    form_begin_time = begin_time
    form_end_time = end_time
    if all_days:
        rows = storage.list_records_for_day(chat_id, day_key, all_days=True)
    elif linked_begin is not None and linked_end is not None:
        rows = storage.list_records_for_period(chat_id, linked_begin, linked_end)
    elif use_day_key_records:
        rows = storage.list_records_for_day(chat_id, day_key)
        form_begin_time, form_end_time = "", ""
    else:
        begin, end = bill_window_for_day(group, day_key, timezone)
        rows = storage.list_records_for_period(chat_id, begin, end)
        form_begin_time = f"{begin:%Y-%m-%d %H:%M:%S}"
        form_end_time = f"{end:%Y-%m-%d %H:%M:%S}"
    rows = filter_records(rows, search_text, search_type)
    deposits = [row for row in rows if row["kind"] == "deposit"]
    payouts = [row for row in rows if row["kind"] == "payout"]
    return BillPageData(
        group=group,
        chat_id=chat_id,
        day_key=day_key,
        title_day="全部账单" if all_days else day_key,
        group_title=group["chat_title"] or str(chat_id),
        all_days=all_days,
        begin_time=form_begin_time,
        end_time=form_end_time,
        rows=rows,
        deposits=deposits,
        payouts=payouts,
        use_day_key_records=use_day_key_records,
    )


def filter_records(rows: list[Any], search_text: str, search_type: str) -> list[Any]:
    needle = search_text.strip().casefold()
    if not needle:
        return rows

    def haystack(row: Any) -> str:
        if search_type == "czr":
            return row["actor_name"] or ""
        if search_type == "bz":
            return row["note"] or ""
        return row["subject_name"] or row["actor_name"] or ""

    return [row for row in rows if needle in haystack(row).casefold()]


def render_legacy_forms(
    chat_id: int,
    day_key: str,
    token: str | None,
    *,
    cutoff_hour: int,
    timezone: tzinfo,
    search_text: str = "",
    search_type: str = "bjr",
    begin_time: str = "",
    end_time: str = "",
    created_at: str = "",
) -> str:
    if not created_at and (not begin_time or not end_time):
        begin_time, end_time = legacy_day_range(day_key, cutoff_hour, timezone)
    token_input = hidden_input("token", token) if token else ""
    created_at_input = hidden_input("created_at", created_at) if created_at else ""
    if created_at:
        date_fields = f'<input type="date" name="created_at" value="{escape(created_at[:10])}" placeholder="日期">'
        search_date_inputs = created_at_input
    else:
        begin_value = html_datetime_value(begin_time, timezone)
        end_value = html_datetime_value(end_time, timezone)
        date_fields = f"""
        <input type="datetime-local" step="1" name="begintime" value="{escape(begin_value)}">
        <input type="datetime-local" step="1" name="endtime" value="{escape(end_value)}">
        """
        search_date_inputs = hidden_input("begintime", begin_value) + hidden_input("endtime", end_value)
    return f"""
    <form method="GET" action="/day_xxb.php" class="date-form">
      <small>
        {date_fields}
      </small>
      {hidden_input("chat_id", str(chat_id))}
      {hidden_input("up_page", "1")}
      {hidden_input("down_page", "1")}
      {token_input}
    </form>
    <div align="center"><br>
      <form method="GET" action="/day_xxb.php" class="search-form">
        <input type="text" placeholder="请输入您要查询的名字或备注关键词" name="firstname" value="{escape(search_text)}">
        {hidden_input("chat_id", str(chat_id))}
        {hidden_input("up_page", "1")}
        {hidden_input("down_page", "1")}
        {search_date_inputs}
        {token_input}
        <select name="type">
          <option value="bjr"{selected_attr(search_type, "bjr")}>按标记人</option>
          <option value="czr"{selected_attr(search_type, "czr")}>按操作人</option>
          <option value="bz"{selected_attr(search_type, "bz")}>按备注</option>
        </select>
        <input type="submit" value="搜索">
      </form>
    </div>
    """


def html_datetime_value(value: str, timezone: tzinfo) -> str:
    parsed = parse_stored_datetime(value, timezone)
    if parsed is None:
        return ""
    return f"{parsed:%Y-%m-%dT%H:%M:%S}"


def selected_attr(current: str, expected: str) -> str:
    return ' selected="selected"' if current == expected else ""


def legacy_day_range(day_key: str, cutoff_hour: int, timezone: tzinfo) -> tuple[str, str]:
    if day_key in {"active", "all", "today", ""}:
        return "", ""
    try:
        begin, end = business_day_range(day_key[:10], cutoff_hour, timezone)
    except ValueError:
        return "", ""
    return f"{begin:%Y-%m-%d %H:%M:%S}", f"{end:%Y-%m-%d %H:%M:%S}"


def hidden_input(name: str, value: str | None) -> str:
    return f'<input type="hidden" name="{escape(name)}" value="{escape(value or "")}">'


def bill_totals(deposits: list[Any], payouts: list[Any]) -> dict[str, Decimal]:
    total_cny = sum_decimal(row["amount_cny"] for row in deposits)
    gross_in_usdt = sum_decimal(row["amount_usdt"] for row in deposits)
    net_in_usdt = sum_decimal(row["net_usdt"] for row in deposits)
    commission_cny = sum_decimal(row["commission_cny"] for row in deposits)
    net_in_cny = total_cny - commission_cny
    total_out_cny = sum_decimal(row["amount_cny"] for row in payouts)
    total_out_usdt = sum_decimal(row["amount_usdt"] for row in payouts)
    return {
        "total_cny": total_cny,
        "gross_in_usdt": gross_in_usdt,
        "commission_cny": commission_cny,
        "net_in_cny": net_in_cny,
        "net_in_usdt": net_in_usdt,
        "total_out_cny": total_out_cny,
        "total_out_usdt": total_out_usdt,
        "balance_cny": net_in_cny - total_out_cny,
        "balance_usdt": net_in_usdt - total_out_usdt,
    }


def render_summary_block(group: Any, deposits: list[Any], payouts: list[Any], trade_methods: tuple[str, ...]) -> str:
    totals = bill_totals(deposits, payouts)
    fee_rate = Decimal(group["deposit_fee_rate"])
    return f"""
    <div class="statistics">
      <div>汇率：<span class="money_rate">{escape(bill_exchange_display(group, trade_methods))}</span></div>
      <div>费率：<span class="profit_rate">{format_number(fee_rate)}%</span></div>
      <div>总入款金额：<span class="upMoney">{format_number(totals["total_cny"])} | {format_money(totals["gross_in_usdt"])}(USDT)</span></div>
      <div>应下发：<span class="sureDown">{format_money(totals["net_in_cny"])} | {format_money(totals["net_in_usdt"])} (USDT)</span></div>
      <div>已下发：<span class="haveDown">{format_number(totals["total_out_cny"])} | {format_money(totals["total_out_usdt"])} (USDT)</span></div>
      <div>未下发：<span class="noDown">{format_money(totals["balance_cny"])} | {format_money(totals["balance_usdt"])} (USDT)</span></div>
    </div>
    """


def bill_exchange_display(group: Any, trade_methods: tuple[str, ...]) -> str:
    if group["realtime_rate"]:
        method = p2p_trade_method_label(trade_methods[0] if trade_methods else "aliPay")
        rank = group["realtime_rate_rank"]
        offset = format_realtime_offset_label(group["realtime_rate_offset"])
        if rank is not None:
            return join_non_empty(f"{method}{int(rank)}档", offset)
        return join_non_empty(f"{method}实时价", offset)
    return format_number(Decimal(group["deposit_exchange_rate"]))


def render_record_table(title: str, rows: list[Any], timezone: tzinfo) -> str:
    body = "".join(render_record_row(row, timezone) for row in rows)
    if not body:
        body = '<tr><td colspan="5" class="empty">暂无记录</td></tr>'
    return f"""
    <section class="panel">
      <div class="box box-primary">
        <div class="box-header">
          <h3 class="box-title">{escape(title)} (<span>{len(rows)}</span>笔)</h3>
        </div>
        <div class="box-body">
      <div class="table-wrap">
        <table class="records">
          <colgroup>
            <col class="col-time">
            <col class="col-amount">
            <col class="col-rate">
            <col class="col-actor">
            <col class="col-note">
          </colgroup>
          <thead>
            <tr><td>时间</td><td>金额</td><td>标记人</td><td>操作人</td><td>备注</td></tr>
          </thead>
          <tbody>{body}</tbody>
        </table>
      </div>
        </div>
      </div>
    </section>
    """


def render_record_row(row: Any, timezone: tzinfo) -> str:
    created = datetime.fromisoformat(row["created_at"]).astimezone(timezone)
    actor = escape(row["actor_name"] or "")
    subject = escape(row["subject_name"] or row["actor_name"] or "")
    note = escape(row["note"] or "")
    return (
        "<tr>"
        f"<td>{created:%m-%d %H:%M:%S}</td>"
        f'<td><span class="copyable">{record_amount_display(row)}</span></td>'
        f"<td>{subject}</td>"
        f"<td>{actor}</td>"
        f"<td>{note}</td>"
        "</tr>"
    )


def record_amount_display(row: Any) -> str:
    amount = Decimal(row["amount_cny"])
    rate = Decimal(row["exchange_rate"])
    gross_usdt = Decimal(row["amount_usdt"])
    net_usdt = Decimal(row["net_usdt"])
    fee_rate = Decimal(row["fee_rate"])
    if row["currency"] == "USDT":
        main = f"{format_money(gross_usdt)}U"
    elif rate == 1:
        main = format_number(amount)
    elif row["kind"] == "payout":
        main = f"{format_money(gross_usdt)}U/{format_number(amount)}"
    elif fee_rate:
        main = f"{format_number(amount)}/{format_number(rate)}*{format_fee_multiplier(fee_rate)}={format_money(net_usdt)}U"
    else:
        main = f"{format_number(amount)}/{format_number(rate)}={format_money(gross_usdt)}U"
    return main


def render_people_stats_box(title: str, deposits: list[Any], payouts: list[Any], key: str) -> str:
    rows = people_stats(deposits, payouts, key)
    body = "".join(
        "<tr>"
        f"<td>{escape(row['name'])} ({row['count']} 笔)</td>"
        f"<td><span class='copyable'>{format_number(row['in_cny'])}</span>/<span class='copyable'>{format_money(row['in_usdt'])}</span>U</td>"
        f"<td><span class='copyable'>{format_number(row['out_cny'])}</span>/<span class='copyable'>{format_money(row['out_usdt'])}</span>U</td>"
        f"<td><span class='copyable'>{format_money(row['balance_cny'])}</span>/<span class='copyable'>{format_money(row['balance_usdt'])}</span>U</td>"
        "</tr>"
        for row in rows
    )
    if not body:
        body = '<tr><td colspan="4" class="empty">暂无统计</td></tr>'
    return render_table_box(title, f"{len(rows)} 人", '<tr class="table-head"><td>用户名</td><td>入款</td><td>已下发</td><td>未下发</td></tr>', body)


def people_stats(deposits: list[Any], payouts: list[Any], key: str) -> list[dict[str, Any]]:
    items: dict[str, dict[str, Any]] = defaultdict(
        lambda: {
            "name": "",
            "count": 0,
            "in_cny": Decimal("0"),
            "in_usdt": Decimal("0"),
            "net_cny": Decimal("0"),
            "net_usdt": Decimal("0"),
            "out_cny": Decimal("0"),
            "out_usdt": Decimal("0"),
        }
    )
    for row in deposits:
        name = stat_key(row, key)
        item = items[name]
        item["name"] = name
        item["count"] += 1
        item["in_cny"] += Decimal(row["amount_cny"])
        item["in_usdt"] += Decimal(row["amount_usdt"])
        item["net_cny"] += Decimal(row["amount_cny"]) - Decimal(row["commission_cny"])
        item["net_usdt"] += Decimal(row["net_usdt"])
    for row in payouts:
        name = stat_key(row, key)
        item = items[name]
        item["name"] = name
        item["count"] += 1
        item["out_cny"] += Decimal(row["amount_cny"])
        item["out_usdt"] += Decimal(row["amount_usdt"])
    result = []
    for item in items.values():
        item["balance_cny"] = item["net_cny"] - item["out_cny"]
        item["balance_usdt"] = item["net_usdt"] - item["out_usdt"]
        result.append(item)
    return sorted(result, key=lambda row: row["name"])


def stat_key(row: Any, key: str) -> str:
    if key == "actor":
        return row["actor_name"] or "未命名"
    if key == "note":
        return row["note"] or ""
    return row["subject_name"] or row["actor_name"] or "未命名"


def render_rate_stats_box(deposits: list[Any]) -> str:
    grouped: dict[str, dict[str, Decimal]] = defaultdict(lambda: {"amount": Decimal("0"), "usdt": Decimal("0")})
    for row in deposits:
        rate = f"{Decimal(row['exchange_rate']):.4f}"
        grouped[rate]["amount"] += Decimal(row["amount_cny"])
        grouped[rate]["usdt"] += Decimal(row["amount_usdt"])
    body = "".join(
        f"<tr><td>{escape(rate)}</td><td>{format_number(values['amount'])}</td><td>{format_money(values['usdt'])} U</td></tr>"
        for rate, values in sorted(grouped.items())
    )
    if not body:
        body = '<tr><td colspan="3" class="empty">暂无统计</td></tr>'
    return render_table_box("统计（按汇率分类）", "", '<tr class="table-head"><td>汇率</td><td>入款</td><td>换算U</td></tr>', body)


def render_table_box(title: str, suffix: str, header: str, body: str) -> str:
    suffix_text = f" ({escape(suffix)})" if suffix else ""
    return f"""
    <section class="panel">
      <div class="box box-primary">
        <div class="box-header">
          <h3 class="box-title">{escape(title)}{suffix_text}</h3>
        </div>
        <div class="box-body">
          <div class="table-wrap">
            <table class="records">
              <tbody>{header}{body}</tbody>
            </table>
          </div>
        </div>
      </div>
    </section>
    """


def render_bill_toolbar(
    storage: Storage,
    data: BillPageData,
    token: str | None,
    *,
    cutoff_hour: int,
    timezone: tzinfo,
    search_text: str = "",
    search_type: str = "bjr",
) -> str:
    if data.all_days:
        prev_next = ""
    else:
        prev_day, next_day = adjacent_days(data.day_key)
        prev_next = (
            f'<a class="btn" href="{escape(legacy_created_at_path(data.chat_id, prev_day, token))}">上一天</a>'
            f'<a class="btn" href="{escape(legacy_created_at_path(data.chat_id, next_day, token))}">下一天</a>'
        )
    download_url = legacy_bill_path(
        data.chat_id,
        data.day_key,
        token,
        cutoff_hour,
        timezone,
        begin_time=data.begin_time,
        end_time=data.end_time,
        search_text=search_text,
        search_type=search_type,
        download=True,
        all_days=data.all_days,
        use_created_at_only=data.use_day_key_records,
    )
    return f"""
    <section class="bill-toolbar">
      <div class="bill-heading">
        <strong>{escape(data.group_title)}</strong>
        <span>{escape(data.title_day)} · 群 ID：{data.chat_id}</span>
      </div>
      <nav class="toolbar-actions">
        <a class="toolbar-link" href="{escape(download_url)}">下载账单</a>
        {render_history_menu(storage, data.chat_id, data.day_key, token)}
        {prev_next}
        <a class="btn" href="{escape(legacy_bill_path(data.chat_id, "today", token, cutoff_hour, timezone))}">今日</a>
        <a class="btn" href="{escape(legacy_bill_path(data.chat_id, "active", token, cutoff_hour, timezone, all_days=True))}">全部</a>
      </nav>
    </section>
    """


def render_history_menu(storage: Storage, chat_id: int, current_day: str, token: str | None) -> str:
    days = list_bill_day_keys(storage, chat_id, limit=30)
    if not days:
        menu_body = '<span class="history-empty">无历史账单</span>'
    else:
        links = []
        for day in days:
            cls = "active" if day == current_day else ""
            links.append(f'<a class="{cls}" href="{escape(legacy_created_at_path(chat_id, day, token))}">{escape(short_day_label(day))}</a>')
        menu_body = "".join(links)
    return f"""
    <span class="history-menu">
      <button type="button" class="history-trigger">历史账单⌄</button>
      <span class="history-dropdown">{menu_body}</span>
    </span>
    """


def short_day_label(day_key: str) -> str:
    try:
        day = datetime.strptime(day_key[:10], "%Y-%m-%d")
    except ValueError:
        return day_key
    return f"{day:%m-%d}"


def render_day_links(storage: Storage, chat_id: int, current_day: str, all_days: bool, token: str | None) -> str:
    days = list_bill_day_keys(storage, chat_id)
    links = [f'<a href="{escape(bill_path(chat_id, "today", token))}">今日</a>']
    if days:
        links.append(f'<a href="{escape(bill_path(chat_id, days[0], token))}">最近账单</a>')
    if all_days:
        links.append('<span>全部</span>')
    else:
        links.append(f'<a href="{escape(bill_path(chat_id, "active", token))}">全部</a>')
    return "".join(links)


def render_footer_links(
    chat_id: int,
    day_key: str,
    token: str | None,
    cutoff_hour: int,
    timezone: tzinfo,
    *,
    search_text: str = "",
    search_type: str = "bjr",
    begin_time: str = "",
    end_time: str = "",
    all_days: bool = False,
    use_created_at_only: bool = False,
) -> str:
    download_url = legacy_bill_path(
        chat_id,
        day_key,
        token,
        cutoff_hour,
        timezone,
        begin_time=begin_time,
        end_time=end_time,
        search_text=search_text,
        search_type=search_type,
        download=True,
        all_days=all_days,
        use_created_at_only=use_created_at_only,
    )
    links = [
        f'<li><a href="{escape(download_url)}">下载excel</a></li>',
        f'<li><a href="{escape(legacy_bill_path(chat_id, "active", token, cutoff_hour, timezone, all_days=True))}">查看全部账单汇总</a></li>',
    ]
    if not all_days:
        prev_day, next_day = adjacent_days(day_key)
        links.insert(1, f'<li><a href="{escape(legacy_created_at_path(chat_id, prev_day, token))}">上一天</a></li>')
        links.insert(2, f'<li><a href="{escape(legacy_created_at_path(chat_id, next_day, token))}">下一天</a></li>')
    return f'<ul class="footer-links">{"".join(links)}</ul>'


def adjacent_days(day_key: str) -> tuple[str, str]:
    try:
        day = datetime.strptime(day_key[:10], "%Y-%m-%d")
    except ValueError:
        day = datetime.now()
    return f"{day - timedelta(days=1):%Y-%m-%d}", f"{day + timedelta(days=1):%Y-%m-%d}"


def bill_path(chat_id: int, day_key: str, token: str | None = None) -> str:
    path = f"/bill/{chat_id}/{day_key}"
    params = {}
    if token:
        params["token"] = token
    if params:
        path = f"{path}?{urlencode(params)}"
    return path


def bill_path_for_day(chat_id: int, day_key: str, token: str | None, cutoff_hour: int, timezone: tzinfo) -> str:
    begin_time, end_time = legacy_day_range(day_key, cutoff_hour, timezone)
    params = {
        "begintime": begin_time,
        "endtime": end_time,
    }
    if token:
        params["token"] = token
    return f"/bill/{chat_id}/{day_key}?{urlencode(params)}"


def legacy_bill_path(
    chat_id: int,
    day_key: str,
    token: str | None,
    cutoff_hour: int,
    timezone: tzinfo,
    *,
    begin_time: str = "",
    end_time: str = "",
    search_text: str = "",
    search_type: str = "bjr",
    download: bool = False,
    all_days: bool = False,
    use_created_at_only: bool = False,
) -> str:
    if all_days or day_key in {"active", "all"}:
        begin_time = ""
        end_time = ""
        created_at = ""
        all_flag = "1"
    else:
        created_at = day_key[:10] if len(day_key) >= 10 else ""
        if use_created_at_only:
            begin_time = ""
            end_time = ""
        elif not begin_time or not end_time:
            begin_time, end_time = legacy_day_range(day_key, cutoff_hour, timezone)
        all_flag = ""
    params = {
        "firstname": search_text,
        "chat_id": str(chat_id),
        "up_page": "1",
        "down_page": "1",
        "created_at": created_at,
        "begintime": begin_time,
        "endtime": end_time,
        "all": all_flag,
        "type": search_type or "bjr",
    }
    if token:
        params["token"] = token
    if download:
        params["download"] = "excel"
    return f"/day_xxb.php?{urlencode(params)}"


def legacy_created_at_path(chat_id: int, day_key: str, token: str | None) -> str:
    params = {
        "chat_id": str(chat_id),
        "created_at": day_key[:10],
    }
    if token:
        params["token"] = token
    return f"/day_xxb.php?{urlencode(params)}"


def list_bill_day_keys(storage: Storage, chat_id: int, limit: int = 120) -> list[str]:
    return [
        row["day_key"]
        for row in storage.conn.execute(
            """
            SELECT DISTINCT day_key FROM records
            WHERE chat_id = ? AND deleted_at IS NULL
            ORDER BY day_key DESC
            LIMIT ?
            """,
            (chat_id, limit),
        )
    ]


def build_bill_xlsx(data: BillPageData, timezone: tzinfo, trade_methods: tuple[str, ...]) -> bytes:
    rows: list[list[dict[str, Any]]] = []
    merges: list[str] = []

    def row_number() -> int:
        return len(rows) + 1

    def add_empty() -> None:
        rows.append([])

    def add_title(text: str) -> None:
        n = row_number()
        rows.append([xlsx_text(text, style=2)])
        merges.append(f"A{n}:H{n}")

    def add_row(values: list[Any], *, style: int = 1, number_columns: set[int] | None = None) -> None:
        numeric = number_columns or set()
        rows.append([xlsx_number(value, style=style) if index in numeric else xlsx_text(value, style=style) for index, value in enumerate(values)])

    title_day = data.day_key[:10] if len(data.day_key) >= 10 else data.title_day
    add_title(f"{title_day}  {weekday_label(title_day)}  【{data.group_title}】")
    add_empty()
    add_title(f"入款：{len(data.deposits)}笔")
    add_row(["序号", "时间", "金额", "应下发", "应下发(U)", "转账人", "回复人", "操作人"])
    for index, row in enumerate(data.deposits, start=1):
        created = parse_stored_datetime(row["created_at"], timezone) or datetime.fromisoformat(row["created_at"]).astimezone(timezone)
        add_row(
            [
                index,
                f"{created:%H:%M:%S}",
                original_amount(row),
                deposit_due_display(row),
                Decimal(row["net_usdt"]),
                row["note"] or "",
                row["subject_name"] or "",
                row["actor_name"] or "",
            ],
            number_columns={0, 2, 4},
        )

    add_empty()
    add_xlsx_people_section(rows, merges, "入款回复人小计", people_stats(data.deposits, [], "subject"), in_only=True)
    add_empty()
    add_xlsx_people_section(rows, merges, "入款操作人小计", people_stats(data.deposits, [], "actor"), in_only=True)
    add_empty()
    add_xlsx_rate_section(rows, merges, data.deposits)
    add_empty()

    add_title(f"下发：{len(data.payouts)}笔")
    add_row(["序号", "时间", "金额", "回复人", "操作人"])
    for index, row in enumerate(data.payouts, start=1):
        created = parse_stored_datetime(row["created_at"], timezone) or datetime.fromisoformat(row["created_at"]).astimezone(timezone)
        add_row(
            [
                index,
                f"{created:%H:%M:%S}",
                record_amount_display(row),
                row["subject_name"] or "",
                row["actor_name"] or "",
            ],
            number_columns={0},
        )

    add_empty()
    add_xlsx_people_section(rows, merges, "下发回复人小计", people_stats([], data.payouts, "subject"), in_only=False)
    add_empty()
    add_xlsx_total_section(rows, merges, data, trade_methods)

    return make_xlsx_package(rows, merges)


def add_xlsx_people_section(
    rows: list[list[dict[str, Any]]],
    merges: list[str],
    title: str,
    stats: list[dict[str, Any]],
    *,
    in_only: bool,
) -> None:
    n = len(rows) + 1
    rows.append([xlsx_text(title, style=2)])
    merges.append(f"A{n}:H{n}")
    for item in stats:
        count = f"{item['count']} 笔"
        if in_only:
            amount = f"{format_number(item['in_cny'])}  |  {format_money(item['in_usdt'])} U"
        else:
            amount = f"{format_number(item['out_cny'])}  |  {format_money(item['out_usdt'])} U"
        row_n = len(rows) + 1
        rows.append([xlsx_text(item["name"]), xlsx_text(""), xlsx_text(count), xlsx_text(amount)])
        merges.extend([f"A{row_n}:B{row_n}", f"D{row_n}:E{row_n}"])


def add_xlsx_rate_section(rows: list[list[dict[str, Any]]], merges: list[str], deposits: list[Any]) -> None:
    n = len(rows) + 1
    rows.append([xlsx_text("入款按汇率小计", style=2)])
    merges.append(f"A{n}:H{n}")
    grouped: dict[str, dict[str, Any]] = defaultdict(lambda: {"count": 0, "amount": Decimal("0"), "usdt": Decimal("0")})
    for row in deposits:
        rate = format_number(Decimal(row["exchange_rate"]))
        grouped[rate]["count"] += 1
        grouped[rate]["amount"] += Decimal(row["amount_cny"])
        grouped[rate]["usdt"] += Decimal(row["amount_usdt"])
    for rate, values in sorted(grouped.items()):
        row_n = len(rows) + 1
        rows.append(
            [
                xlsx_text(rate),
                xlsx_text(""),
                xlsx_text(f"{values['count']} 笔"),
                xlsx_text(f"{format_number(values['amount'])}  |  {format_money(values['usdt'])} U"),
            ]
        )
        merges.extend([f"A{row_n}:B{row_n}", f"D{row_n}:E{row_n}"])


def add_xlsx_total_section(
    rows: list[list[dict[str, Any]]],
    merges: list[str],
    data: BillPageData,
    trade_methods: tuple[str, ...],
) -> None:
    n = len(rows) + 1
    rows.append([xlsx_text("总计", style=2)])
    merges.append(f"A{n}:H{n}")
    totals = bill_totals(data.deposits, data.payouts)
    total_rows = [
        ("费率：", f"{format_number(Decimal(data.group['deposit_fee_rate']))}%"),
        ("汇率：", bill_exchange_display(data.group, trade_methods)),
        ("入款总数：", f"{format_number(totals['total_cny'])}  |  {format_money(totals['gross_in_usdt'])} U"),
        ("应下发：", f"{format_money(totals['net_in_cny'])}  |  {format_money(totals['net_in_usdt'])} U"),
        ("已下发：", f"{format_number(totals['total_out_cny'])}  |  {format_money(totals['total_out_usdt'])} U"),
        ("未下发：", f"{format_money(totals['balance_cny'])}  |  {format_money(totals['balance_usdt'])} U"),
    ]
    for label, value in total_rows:
        row_n = len(rows) + 1
        rows.append([xlsx_text(label), xlsx_text(""), xlsx_text(value)])
        merges.extend([f"A{row_n}:B{row_n}", f"C{row_n}:E{row_n}"])


def xlsx_text(value: Any, *, style: int = 1) -> dict[str, Any]:
    return {"value": "" if value is None else str(value), "style": style, "number": False}


def xlsx_number(value: Any, *, style: int = 1) -> dict[str, Any]:
    return {"value": excel_number(value), "style": style, "number": True}


def excel_number(value: Any) -> str:
    if isinstance(value, Decimal):
        return format(value.normalize(), "f").rstrip("0").rstrip(".") or "0"
    if isinstance(value, int):
        return str(value)
    return format_number(Decimal(str(value)))


def original_amount(row: Any) -> Decimal:
    return Decimal(row["amount"])


def deposit_due_display(row: Any) -> str:
    amount = Decimal(row["amount_cny"])
    rate = Decimal(row["exchange_rate"])
    gross_usdt = Decimal(row["amount_usdt"])
    net_usdt = Decimal(row["net_usdt"])
    fee_rate = Decimal(row["fee_rate"])
    if row["currency"] == "USDT":
        return f"{format_money(gross_usdt)}U"
    if rate == 1:
        return format_number(amount)
    if fee_rate:
        return f"{format_number(amount)}*{format_fee_multiplier(fee_rate)}/{format_number(rate)}={format_money(net_usdt)}"
    return f"{format_number(amount)}/{format_number(rate)}={format_money(gross_usdt)}"


def weekday_label(day_key: str) -> str:
    try:
        day = datetime.strptime(day_key[:10], "%Y-%m-%d")
    except ValueError:
        return ""
    return ["星期一", "星期二", "星期三", "星期四", "星期五", "星期六", "星期日"][day.weekday()]


def xlsx_filename(day_key: str, group_title: str, timezone: tzinfo) -> str:
    title_day = day_key[:10] if len(day_key) >= 10 else datetime.now(timezone).strftime("%Y-%m-%d")
    safe_title = re.sub(r'[\\/:*?"<>|\r\n\t]+', "_", group_title).strip() or "未命名群"
    safe_title = safe_title[:80]
    timestamp = int(datetime.now(timezone).timestamp() * 1000)
    return f"账单_{title_day}_{safe_title}_{timestamp}.xlsx"


def content_disposition(filename: str) -> str:
    return f"attachment; filename=\"ledger.xlsx\"; filename*=UTF-8''{quote(filename)}"


def make_xlsx_package(rows: list[list[dict[str, Any]]], merges: list[str]) -> bytes:
    output = BytesIO()
    with ZipFile(output, "w", ZIP_DEFLATED) as zf:
        zf.writestr("[Content_Types].xml", xlsx_content_types())
        zf.writestr("_rels/.rels", xlsx_root_rels())
        zf.writestr("docProps/app.xml", xlsx_app_props())
        zf.writestr("docProps/core.xml", xlsx_core_props())
        zf.writestr("xl/workbook.xml", xlsx_workbook())
        zf.writestr("xl/_rels/workbook.xml.rels", xlsx_workbook_rels())
        zf.writestr("xl/styles.xml", xlsx_styles())
        zf.writestr("xl/worksheets/sheet1.xml", xlsx_sheet(rows, merges))
    return output.getvalue()


def xlsx_sheet(rows: list[list[dict[str, Any]]], merges: list[str]) -> str:
    max_row = max(len(rows), 1)
    max_col = max((len(row) for row in rows), default=1)
    dimension = f"A1:{column_name(max(max_col, 8))}{max_row}"
    row_xml = []
    for row_index, row in enumerate(rows, start=1):
        cells = "".join(xlsx_cell(row_index, col_index, cell) for col_index, cell in enumerate(row, start=1))
        row_xml.append(f'<row r="{row_index}">{cells}</row>')
    merge_xml = ""
    if merges:
        merge_xml = '<mergeCells count="{}">{}</mergeCells>'.format(
            len(merges),
            "".join(f'<mergeCell ref="{escape(ref)}"/>' for ref in merges),
        )
    return f"""<?xml version="1.0" encoding="UTF-8"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
  <dimension ref="{dimension}"/>
  <cols>
    <col min="1" max="1" width="10" customWidth="true"/>
    <col min="2" max="2" width="20" customWidth="true"/>
    <col min="3" max="3" width="20" customWidth="true"/>
    <col min="4" max="4" width="30" customWidth="true"/>
    <col min="5" max="5" width="20" customWidth="true"/>
    <col min="6" max="6" width="20" customWidth="true"/>
    <col min="7" max="7" width="30" customWidth="true"/>
    <col min="8" max="8" width="30" customWidth="true"/>
  </cols>
  <sheetData>{''.join(row_xml)}</sheetData>
  {merge_xml}
</worksheet>"""


def xlsx_cell(row_index: int, col_index: int, cell: dict[str, Any]) -> str:
    ref = f"{column_name(col_index)}{row_index}"
    style = int(cell.get("style", 1))
    value = cell.get("value", "")
    if cell.get("number"):
        return f'<c r="{ref}" s="{style}" t="n"><v>{escape(str(value))}</v></c>'
    text = escape(str(value))
    preserve = ' xml:space="preserve"' if str(value).startswith(" ") or str(value).endswith(" ") or "  " in str(value) else ""
    return f'<c r="{ref}" s="{style}" t="inlineStr"><is><t{preserve}>{text}</t></is></c>'


def column_name(index: int) -> str:
    name = ""
    while index:
        index, remainder = divmod(index - 1, 26)
        name = chr(65 + remainder) + name
    return name


def xlsx_content_types() -> str:
    return """<?xml version="1.0" encoding="UTF-8"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Override PartName="/docProps/app.xml" ContentType="application/vnd.openxmlformats-officedocument.extended-properties+xml"/>
  <Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/>
  <Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>
  <Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>
  <Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>
</Types>"""


def xlsx_root_rels() -> str:
    return """<?xml version="1.0" encoding="UTF-8"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>
  <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/>
  <Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/extended-properties" Target="docProps/app.xml"/>
</Relationships>"""


def xlsx_workbook() -> str:
    return """<?xml version="1.0" encoding="UTF-8"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
  <workbookPr date1904="false"/>
  <bookViews><workbookView activeTab="0"/></bookViews>
  <sheets><sheet name="Sheet0" r:id="rId1" sheetId="1"/></sheets>
</workbook>"""


def xlsx_workbook_rels() -> str:
    return """<?xml version="1.0" encoding="UTF-8"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
  <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>
</Relationships>"""


def xlsx_app_props() -> str:
    return """<?xml version="1.0" encoding="UTF-8"?>
<Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/extended-properties"><Application>Ledger Bot</Application></Properties>"""


def xlsx_core_props() -> str:
    created = datetime.utcnow().replace(microsecond=0).isoformat() + "Z"
    return f"""<?xml version="1.0" encoding="UTF-8" standalone="no"?>
<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:dcterms="http://purl.org/dc/terms/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
  <dcterms:created xsi:type="dcterms:W3CDTF">{created}</dcterms:created>
  <dc:creator>Ledger Bot</dc:creator>
</cp:coreProperties>"""


def xlsx_styles() -> str:
    return """<?xml version="1.0" encoding="UTF-8"?>
<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <fonts count="3">
    <font><sz val="11"/><name val="Calibri"/><family val="2"/></font>
    <font><name val="Calibri"/><sz val="14"/><b val="true"/></font>
    <font><name val="Calibri"/><sz val="12"/></font>
  </fonts>
  <fills count="2"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill></fills>
  <borders count="2">
    <border><left/><right/><top/><bottom/><diagonal/></border>
    <border><left style="thin"/><right style="thin"/><top style="thin"/><bottom style="thin"/></border>
  </borders>
  <cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>
  <cellXfs count="3">
    <xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/>
    <xf numFmtId="0" fontId="2" fillId="0" borderId="1" xfId="0" applyBorder="true" applyFont="true"><alignment horizontal="left" vertical="center"/></xf>
    <xf numFmtId="0" fontId="1" fillId="0" borderId="0" xfId="0" applyFont="true"><alignment horizontal="left" vertical="center"/></xf>
  </cellXfs>
  <cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles>
</styleSheet>"""


def simple_page(title: str, body: str) -> str:
    return page_shell(title, f'<main class="simple"><h1>{escape(title)}</h1>{body}</main>')


def page_shell(title: str, body: str) -> str:
    return f"""<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{escape(title)}</title>
  <style>
    * {{ box-sizing: border-box; }}
    :root {{
      color-scheme: light;
      --bg: #f3f6fb;
      --panel: #ffffff;
      --panel-soft: #f8fafc;
      --line: #d8e1ed;
      --line-soft: #edf2f7;
      --text: #15233b;
      --muted: #65758b;
      --blue: #2563eb;
      --blue-soft: #eff6ff;
      --green: #047857;
      --shadow: 0 10px 28px rgba(20, 42, 75, 0.08);
    }}
    body, table, input, select {{
      font-family: Arial, "Microsoft YaHei", "PingFang SC", sans-serif;
      color: var(--text);
    }}
    body {{
      margin: 0;
      background: var(--bg);
      font-size: 14px;
      line-height: 1.5;
    }}
    a {{
      color: var(--blue);
      text-decoration: none;
    }}
    a:hover {{
      color: #1d4ed8;
      text-decoration: underline;
    }}
    .content-wrapper {{
      min-height: 100vh;
      padding: 24px;
    }}
    .container {{
      width: 100%;
      max-width: 1480px;
      margin: 0 auto;
    }}
    .content {{
      min-height: 250px;
      display: grid;
      grid-template-columns: minmax(0, 1fr);
      gap: 16px;
    }}
    .bill-toolbar,
    .date-form,
    .search-form,
    .box,
    .statistics {{
      background: var(--panel);
      border: 1px solid var(--line);
      border-radius: 8px;
      box-shadow: var(--shadow);
    }}
    .bill-toolbar {{
      display: flex;
      justify-content: space-between;
      gap: 16px;
      align-items: center;
      padding: 18px 20px;
      margin-bottom: 12px;
    }}
    .bill-heading {{
      display: flex;
      flex-direction: column;
      gap: 4px;
      min-width: 0;
    }}
    .bill-heading strong {{
      font-size: 22px;
      line-height: 1.25;
      overflow-wrap: anywhere;
    }}
    .bill-heading span,
    .muted {{
      color: var(--muted);
    }}
    .toolbar-actions {{
      display: flex;
      flex-wrap: wrap;
      gap: 8px;
      justify-content: flex-end;
      align-items: center;
    }}
    .toolbar-link {{
      display: inline-flex;
      align-items: center;
      min-height: 34px;
      color: var(--muted);
      font-weight: 700;
      padding: 0 2px;
      white-space: nowrap;
    }}
    .toolbar-link:hover {{
      color: var(--text);
      text-decoration: none;
    }}
    .history-menu {{
      position: relative;
      display: inline-flex;
      align-items: center;
      min-height: 34px;
      z-index: 5;
    }}
    .history-trigger {{
      border: 0;
      background: transparent;
      color: var(--muted);
      font: inherit;
      font-weight: 700;
      cursor: pointer;
      padding: 0 4px;
      height: 34px;
    }}
    .history-trigger:hover,
    .history-menu:focus-within .history-trigger {{
      color: var(--text);
    }}
    .history-dropdown {{
      display: none;
      position: absolute;
      top: 34px;
      left: 0;
      min-width: 92px;
      max-height: 520px;
      overflow-y: auto;
      padding: 6px 0;
      background: #fff;
      border: 1px solid var(--line);
      border-radius: 4px;
      box-shadow: 0 12px 28px rgba(20, 42, 75, 0.16);
    }}
    .history-menu:hover .history-dropdown,
    .history-menu:focus-within .history-dropdown {{
      display: block;
    }}
    .history-dropdown a,
    .history-empty {{
      display: block;
      padding: 3px 14px;
      line-height: 22px;
      color: var(--muted);
      white-space: nowrap;
    }}
    .history-dropdown a:hover {{
      background: var(--blue-soft);
      color: var(--blue);
      text-decoration: none;
    }}
    .history-dropdown a.active {{
      color: var(--blue);
      font-weight: 700;
    }}
    .btn {{
      display: inline-flex;
      align-items: center;
      justify-content: center;
      min-height: 34px;
      padding: 7px 12px;
      border: 1px solid var(--line);
      border-radius: 6px;
      background: #fff;
      color: var(--text);
      font-weight: 600;
      white-space: nowrap;
    }}
    .btn:hover {{
      background: var(--blue-soft);
      text-decoration: none;
    }}
    .btn.primary {{
      border-color: var(--blue);
      background: var(--blue);
      color: #fff;
    }}
    .btn.primary:hover {{
      background: #1d4ed8;
      color: #fff;
    }}
    .panel {{
      width: 100%;
      margin: 0;
      padding: 0;
      background: transparent;
      border: 0;
      box-shadow: none;
    }}
    .box {{
      margin: 0;
      padding: 16px;
      width: 100%;
    }}
    .box-primary {{
      border-left: 4px solid var(--blue);
    }}
    .box-header {{
      display: block;
      padding-bottom: 12px;
      border-bottom: 1px solid var(--line-soft);
      margin-bottom: 8px;
    }}
    .box-title {{
      display: inline-block;
      margin: 0;
      font-size: 17px;
      font-weight: bold;
      line-height: 1.2;
    }}
    .box-body {{
      padding: 0;
    }}
    .table-wrap {{ overflow-x: auto; }}
    table {{
      width: 100%;
      max-width: 100%;
      border-collapse: collapse;
      table-layout: auto;
    }}
    td {{
      padding: 9px 8px !important;
      overflow-wrap: anywhere;
      white-space: normal;
      vertical-align: top;
      border-top: 1px solid var(--line-soft);
    }}
    .records thead td,
    .records .table-head td {{
      font-weight: 600;
      color: #334155;
      background: var(--panel-soft);
    }}
    .records tbody tr:hover td {{ background: #fbfdff; }}
    .col-time {{ width: 18%; }}
    .col-amount {{ width: 32%; }}
    .col-rate {{ width: 18%; }}
    .col-actor {{ width: 22%; }}
    .col-note {{ width: 10%; }}
    .date-form {{
      padding: 12px 14px;
      margin-bottom: 10px;
    }}
    .date-form small {{
      display: grid;
      grid-template-columns: repeat(2, minmax(0, 1fr));
      gap: 10px;
    }}
    .date-form input[type="text"],
    .date-form input[type="date"],
    .date-form input[type="datetime-local"] {{
      width: 100%;
      height: 34px;
      font-size: 14px;
      border-radius: 6px;
      border: 1px solid var(--line);
      padding: 0 10px;
      background-color: #fff;
      cursor: pointer;
    }}
    .date-form input[name="endtime"] {{
      float: none;
    }}
    .date-form input[name="created_at"] {{
      max-width: 220px;
    }}
    .search-form {{
      display: flex;
      justify-content: center;
      gap: 8px;
      width: 100%;
      padding: 12px 14px;
      margin: 0 0 16px;
    }}
    .search-form input[type="text"] {{
      flex: 1 1 360px;
      min-width: 0;
      height: 34px;
      border-radius: 6px;
      border: 1px solid var(--line);
      padding: 0 10px;
      background-color: #fff;
    }}
    .search-form select {{
      flex: 0 0 130px;
      height: 34px;
      border-radius: 6px;
      border: 1px solid var(--line);
      background-color: #fff;
    }}
    .search-form input[type="submit"] {{
      flex: 0 0 86px;
      height: 34px;
      border-radius: 6px;
      background: var(--text);
      color: #fff;
      border: 0;
      cursor: pointer;
      font-weight: 700;
    }}
    .statistics {{
      grid-column: 1 / -1;
      display: grid;
      grid-template-columns: repeat(3, minmax(0, 1fr));
      gap: 10px;
      padding: 16px;
    }}
    .statistics div {{
      margin: 0;
      padding: 10px 12px;
      border: 1px solid var(--line-soft);
      border-radius: 6px;
      background: var(--panel-soft);
      color: var(--muted);
    }}
    .statistics span {{
      font-weight: bold;
      color: var(--text);
      display: block;
      margin-top: 2px;
    }}
    .footer-links {{
      grid-column: 1 / -1;
      display: flex;
      flex-wrap: wrap;
      gap: 10px;
      margin: 0 0 24px;
      padding: 0;
      list-style: none;
    }}
    .footer-links li {{
      margin: 0;
    }}
    .footer-links a {{
      display: inline-flex;
      padding: 7px 10px;
      border-radius: 6px;
      background: var(--panel);
      border: 1px solid var(--line);
      color: var(--text);
      font-weight: 600;
    }}
    .footer-links a:hover {{
      background: var(--blue-soft);
      text-decoration: none;
    }}
    .copyable {{
      cursor: pointer;
      border-bottom: 1px dotted #94a3b8;
    }}
    .copied {{
      color: var(--green);
    }}
    .empty {{
      color: var(--muted);
      text-align: center;
      padding: 18px 0 !important;
    }}
    .simple {{
      padding: 40px 33px;
    }}
    @media (max-width: 920px) {{
      .content-wrapper {{ padding: 12px; }}
      .content {{ grid-template-columns: 1fr; }}
      .bill-toolbar {{
        align-items: stretch;
        flex-direction: column;
      }}
      .toolbar-actions {{ justify-content: flex-start; }}
      .statistics {{ grid-template-columns: 1fr; }}
    }}
    @media (max-width: 640px) {{
      body {{ font-size: 13px; }}
      .box {{
        padding: 14px 10px;
        margin-bottom: 12px;
      }}
      .box-title {{
        font-size: 16px;
      }}
      td {{
        font-size: 12px;
        padding: 8px 6px !important;
      }}
      .date-form small {{ grid-template-columns: 1fr; }}
      .date-form input[type="text"],
      .date-form input[type="date"],
      .date-form input[type="datetime-local"] {{
        width: 100%;
        margin-bottom: 6px;
      }}
      .date-form input[name="endtime"] {{
        float: none;
      }}
      .search-form {{
        flex-wrap: wrap;
      }}
      .search-form input[type="text"],
      .search-form select,
      .search-form input[type="submit"] {{
        flex: 1 1 100%;
      }}
    }}
  </style>
  <script>
    document.addEventListener('pointerdown', function(event) {{
      var target = event.target.closest('.date-form input[type="date"], .date-form input[type="datetime-local"]');
      if (!target || typeof target.showPicker !== 'function') return;
      if (document.activeElement !== target) target.focus();
      try {{
        target.showPicker();
      }} catch (error) {{}}
    }});
    document.addEventListener('click', function(event) {{
      var target = event.target.closest('.copyable');
      if (!target || !navigator.clipboard) return;
      navigator.clipboard.writeText(target.textContent.trim()).then(function() {{
        target.classList.add('copied');
        setTimeout(function() {{ target.classList.remove('copied'); }}, 600);
      }}).catch(function() {{}});
    }});
  </script>
</head>
<body>
{body}
</body>
</html>"""
