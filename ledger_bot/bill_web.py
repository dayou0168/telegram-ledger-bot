from __future__ import annotations

from collections import defaultdict
from datetime import datetime, timedelta, tzinfo
from decimal import Decimal
from html import escape
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import threading
from typing import Any
from urllib.parse import parse_qs, unquote, urlencode, urlparse

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
            status, body = handle_bill_web_request(config, self.path)
            payload = body.encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            if include_body:
                self.wfile.write(payload)

        def log_message(self, format: str, *args: Any) -> None:
            print(f"bill-web: {format % args}", flush=True)

    return BillRequestHandler


def handle_bill_web_request(config: Config, raw_path: str) -> tuple[int, str]:
    parsed = urlparse(raw_path)
    query = parse_qs(parsed.query)
    path = parsed.path.rstrip("/") or "/"

    if path == "/health":
        return HTTPStatus.OK, simple_page("OK", "<p>ok</p>")
    if path == "/":
        return HTTPStatus.OK, simple_page("账单服务", "<p>账单网页服务已启动。</p>")

    if not bill_web_token_allowed(config, query):
        return HTTPStatus.FORBIDDEN, simple_page("访问受限", "<p>链接无效或缺少访问令牌。</p>")

    segments = [unquote(part) for part in path.strip("/").split("/") if part]
    if not segments or segments[0] not in {"bill", "day_xxb.php"}:
        return HTTPStatus.NOT_FOUND, simple_page("404", "<p>页面不存在。</p>")

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
        return HTTPStatus.BAD_REQUEST, simple_page("参数错误", "<p>账单链接参数不正确。</p>")

    storage = Storage(config.db_path)
    try:
        body = render_bill_page(
            storage,
            chat_id,
            day_key,
            timezone=config.timezone,
            trade_methods=config.p2p_rate_trade_methods,
            token=config.bill_web_token,
            search_text=(query.get("firstname") or [""])[0],
            search_type=(query.get("type") or ["bjr"])[0],
            begin_time=(query.get("begintime") or query.get("begin_time") or [""])[0],
            end_time=(query.get("endtime") or query.get("end_time") or [""])[0],
        )
    except KeyError:
        return HTTPStatus.NOT_FOUND, simple_page("未找到群组", "<p>没有找到这个群的账单。</p>")
    finally:
        storage.conn.close()
    return HTTPStatus.OK, body


def bill_web_token_allowed(config: Config, query: dict[str, list[str]]) -> bool:
    if not config.bill_web_token:
        return True
    return (query.get("token") or [""])[0] == config.bill_web_token


def day_key_from_legacy_query(query: dict[str, list[str]]) -> str:
    if (query.get("all") or [""])[0]:
        return "active"
    for key in ("day_key", "date", "begintime", "begin_time"):
        value = (query.get(key) or [""])[0]
        if len(value) >= 10:
            return value[:10]
    return "today"


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
) -> str:
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
    else:
        begin, end = bill_window_for_day(group, day_key, timezone)
        rows = storage.list_records_for_period(chat_id, begin, end)
        form_begin_time = f"{begin:%Y-%m-%d %H:%M:%S}"
        form_end_time = f"{end:%Y-%m-%d %H:%M:%S}"
    rows = filter_records(rows, search_text, search_type)
    deposits = [row for row in rows if row["kind"] == "deposit"]
    payouts = [row for row in rows if row["kind"] == "payout"]

    title_day = "全部账单" if all_days else day_key
    group_title = group["chat_title"] or str(chat_id)

    content = f"""
    <div class="content-wrapper">
      <div class="container">
        {render_legacy_forms(
            chat_id,
            day_key,
            token,
            cutoff_hour=int(group["day_cutoff_hour"]),
            timezone=timezone,
            search_text=search_text,
            search_type=search_type,
            begin_time=form_begin_time,
            end_time=form_end_time,
        )}
        <section class="content">
          {render_record_table("入款", deposits, timezone)}
          {render_record_table("下发", payouts, timezone)}
          {render_people_stats_box("统计（按标记人）", deposits, payouts, "subject")}
          {render_people_stats_box("统计（按操作人）", deposits, payouts, "actor")}
          {render_people_stats_box("统计（按备注）", deposits, payouts, "note")}
          {render_rate_stats_box(deposits)}
          {render_summary_block(group, deposits, payouts, trade_methods)}
          {render_footer_links(chat_id, day_key, token, int(group["day_cutoff_hour"]), timezone)}
        </section>
      </div>
    </div>
    """
    return page_shell(f"{group_title} - {title_day}", content)


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
) -> str:
    if not begin_time or not end_time:
        begin_time, end_time = legacy_day_range(day_key, cutoff_hour, timezone)
    token_input = hidden_input("token", token) if token else ""
    return f"""
    <form method="GET" action="/day_xxb.php" class="date-form">
      <small>
        <input type="text" name="begintime" value="{escape(begin_time)}">
        <input type="text" name="endtime" value="{escape(end_time)}">
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
        {hidden_input("begintime", begin_time)}
        {hidden_input("endtime", end_time)}
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
    return render_table_box(title, f"{len(rows)} 人", "<tr><td>用户名</td><td>入款</td><td>已下发</td><td>未下发</td></tr>", body)


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
    return render_table_box("统计（按汇率分类）", "", "<tr><td>汇率</td><td>入款</td><td>换算U</td></tr>", body)


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


def render_footer_links(chat_id: int, day_key: str, token: str | None, cutoff_hour: int, timezone: tzinfo) -> str:
    prev_day, next_day = adjacent_days(day_key)
    links = [
        '<li><a href="#">下载excel</a></li>',
        f'<li><a href="{escape(bill_path_for_day(chat_id, prev_day, token, cutoff_hour, timezone))}">上一天</a></li>',
        f'<li><a href="{escape(bill_path_for_day(chat_id, next_day, token, cutoff_hour, timezone))}">下一天</a></li>',
        f'<li><a href="{escape(bill_path(chat_id, "active", token))}">查看全部账单汇总</a></li>',
    ]
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


def list_bill_day_keys(storage: Storage, chat_id: int) -> list[str]:
    return [
        row["day_key"]
        for row in storage.conn.execute(
            """
            SELECT DISTINCT day_key FROM records
            WHERE chat_id = ? AND deleted_at IS NULL
            ORDER BY day_key DESC
            """,
            (chat_id,),
        )
    ]


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
    body, table, input, select {{
      font-family: Arial, Helvetica, sans-serif;
      color: #333;
    }}
    body {{
      margin: 0;
      background: #f9f9f9;
      font-size: 14px;
      line-height: 1.42857143;
    }}
    a {{
      color: #337ab7;
      text-decoration: none;
    }}
    a:hover {{
      color: #23527c;
      text-decoration: underline;
    }}
    .content-wrapper {{
      margin-left: 0;
      background-color: #f9f9f9;
      padding: 5px;
    }}
    .container {{
      width: 100%;
      max-width: none;
      margin-right: auto;
      margin-left: auto;
      padding-right: 0;
      padding-left: 0;
      overflow-x: auto;
    }}
    .content {{
      min-height: 250px;
      padding: 0;
      margin-right: auto;
      margin-left: auto;
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
      position: relative;
      border: 1px solid #e0e0e0;
      border-radius: 5px;
      background: #fff;
      margin-bottom: 20px;
      padding: 20px;
      width: 100%;
    }}
    .box-primary {{
      border-left: 4px solid #007bff;
    }}
    .box-header {{
      color: #444;
      display: block;
      padding-bottom: 10px;
      border-bottom: 1px solid #e0e0e0;
      margin-bottom: 20px;
    }}
    .box-title {{
      display: inline-block;
      margin: 0;
      font-size: 18px;
      font-weight: bold;
      line-height: 1;
      color: #333;
    }}
    .box-body {{
      border-top-left-radius: 0;
      border-top-right-radius: 0;
      border-bottom-right-radius: 3px;
      border-bottom-left-radius: 3px;
      padding: 0;
    }}
    .table-wrap {{ overflow-x: auto; }}
    table {{
      width: 100%;
      max-width: 100%;
      border-collapse: collapse;
      table-layout: fixed;
    }}
    td {{
      max-width: 25%;
      padding: 6px 0 !important;
      word-wrap: break-word;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      vertical-align: top;
      border-top: 1px solid #ddd;
    }}
    .records tr:first-child td {{
      border-top: 0;
      font-weight: 600;
    }}
    .col-time {{ width: 18%; }}
    .col-amount {{ width: 32%; }}
    .col-rate {{ width: 18%; }}
    .col-actor {{ width: 22%; }}
    .col-note {{ width: 10%; }}
    .date-form small {{
      display: block;
      overflow: hidden;
    }}
    .date-form input[type="text"] {{
      width: 48%;
      height: 25px;
      font-size: 14px;
      border-radius: 0;
      border: 1px solid #ccc;
      padding-left: 10px;
      background-color: #fff;
    }}
    .date-form input[name="endtime"] {{
      float: right;
    }}
    .search-form {{
      display: flex;
      justify-content: center;
      width: 100%;
      margin: 0 0 10px;
    }}
    .search-form input[type="text"] {{
      width: 60%;
      height: 25px;
      border-radius: 0;
      border: 1px solid #ccc;
      padding-left: 10px;
      background-color: #fff;
    }}
    .search-form select {{
      width: 22%;
      height: 25px;
      border-radius: 0;
      border: 1px solid #ccc;
      background-color: #fff;
    }}
    .search-form input[type="submit"] {{
      width: 15%;
      height: 25px;
      border-radius: 0;
      background: #0066cc;
      color: #fff;
      border: 0;
      cursor: pointer;
    }}
    .statistics {{
      padding: 0 10px 10px;
    }}
    .statistics div {{
      margin: 10px 0;
      font-size: 14px;
      color: #555;
    }}
    .statistics span {{
      font-weight: bold;
      color: #333;
    }}
    .footer-links {{
      margin: 0 0 24px 22px;
      padding: 0;
    }}
    .footer-links li {{
      margin: 6px 0;
    }}
    .copyable {{
      cursor: pointer;
    }}
    .empty {{
      color: #777;
      text-align: center;
      padding: 18px 0 !important;
    }}
    .simple {{
      padding: 40px 33px;
    }}
    @media (max-width: 640px) {{
      body {{ font-size: 12px; }}
      .box {{
        padding: 14px 10px;
        margin-bottom: 12px;
      }}
      .box-title {{
        font-size: 16px;
      }}
      td {{
        white-space: normal;
        font-size: 12px;
      }}
      .date-form input[type="text"] {{
        width: 100%;
        margin-bottom: 6px;
      }}
      .date-form input[name="endtime"] {{
        float: none;
      }}
      .search-form input[type="text"] {{ width: 58%; }}
      .search-form select {{ width: 24%; }}
      .search-form input[type="submit"] {{ width: 18%; }}
    }}
  </style>
  <script>
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
