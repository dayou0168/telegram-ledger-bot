from __future__ import annotations

from datetime import datetime, tzinfo
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
from .storage import Storage, business_day_key


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
            status, body = handle_bill_web_request(config, self.path)
            payload = body.encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
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
    if not segments or segments[0] != "bill":
        return HTTPStatus.NOT_FOUND, simple_page("404", "<p>页面不存在。</p>")

    try:
        if len(segments) >= 3:
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


def render_bill_page(
    storage: Storage,
    chat_id: int,
    day_key: str,
    *,
    timezone: tzinfo,
    trade_methods: tuple[str, ...] = (),
    token: str | None = None,
    now: datetime | None = None,
) -> str:
    group = storage.get_group(chat_id)
    if day_key in {"today", ""}:
        now = now or datetime.now(timezone)
        day_key = business_day_key(now, int(group["day_cutoff_hour"]), timezone)
    all_days = int(group["day_cutoff_hour"]) < 0 or day_key in {"active", "all"}
    rows = storage.list_records_for_day(chat_id, day_key, all_days=all_days)
    deposits = [row for row in rows if row["kind"] == "deposit"]
    payouts = [row for row in rows if row["kind"] == "payout"]

    summary = bill_summary(group, deposits, payouts, trade_methods)
    title_day = "全部账单" if all_days else day_key
    group_title = group["chat_title"] or str(chat_id)
    day_links = render_day_links(storage, chat_id, day_key, all_days, token)

    content = f"""
    <header class="topbar">
      <div>
        <div class="eyebrow">Telegram 记账机器人</div>
        <h1>{escape(group_title)}</h1>
        <div class="muted">群 ID：{chat_id} · {escape(title_day)}</div>
      </div>
      <div class="nav">{day_links}</div>
    </header>

    <section class="summary">
      {''.join(render_summary_card(label, value) for label, value in summary)}
    </section>

    <section class="grid">
      {render_record_table("今日入款", deposits, timezone)}
      {render_record_table("今日下发", payouts, timezone)}
    </section>
    """
    return page_shell(f"{group_title} - {title_day}", content)


def bill_summary(group: Any, deposits: list[Any], payouts: list[Any], trade_methods: tuple[str, ...]) -> list[tuple[str, str]]:
    total_cny = sum_decimal(row["amount_cny"] for row in deposits)
    gross_in_usdt = sum_decimal(row["amount_usdt"] for row in deposits)
    net_in_usdt = sum_decimal(row["net_usdt"] for row in deposits)
    total_out_usdt = sum_decimal(row["amount_usdt"] for row in payouts)
    balance = net_in_usdt - total_out_usdt
    fee_rate = Decimal(group["deposit_fee_rate"])
    summary = [
        ("总入款", f"{format_number(total_cny)} / {format_money(gross_in_usdt)}U"),
        ("交易费率", f"{format_number(fee_rate)}%"),
        ("应下发", f"{format_money(net_in_usdt)}U"),
        ("已下发", f"{format_money(total_out_usdt)}U"),
        ("余额", f"{format_money(balance)}U"),
    ]
    summary.insert(1, ("汇率", bill_exchange_display(group, trade_methods)))
    return summary


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
      <h2>{escape(title)} <span>{len(rows)}笔</span></h2>
      <div class="table-wrap">
        <table>
          <thead>
            <tr><th>时间</th><th>金额</th><th>汇率</th><th>操作人</th><th>备注</th></tr>
          </thead>
          <tbody>{body}</tbody>
        </table>
      </div>
    </section>
    """


def render_record_row(row: Any, timezone: tzinfo) -> str:
    created = datetime.fromisoformat(row["created_at"]).astimezone(timezone)
    actor = escape(row["actor_name"] or "")
    subject = escape(row["subject_name"] or row["actor_name"] or "")
    note = escape(row["note"] or "")
    if subject and subject != actor:
        actor = f"{actor}<br><span class=\"muted\">{subject}</span>"
    return (
        "<tr>"
        f"<td>{created:%Y-%m-%d}<br><strong>{created:%H:%M:%S}</strong></td>"
        f"<td>{record_amount_display(row)}</td>"
        f"<td>{escape(format_number(Decimal(row['exchange_rate'])))}</td>"
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
    else:
        main = f"{format_number(amount)} / {format_number(rate)} = {format_money(gross_usdt)}U"
    if row["kind"] == "deposit" and fee_rate:
        main += f"<br><span class=\"muted\">扣费后 {format_money(net_usdt)}U · {format_fee_multiplier(fee_rate)}</span>"
    return main


def render_summary_card(label: str, value: str) -> str:
    return f'<div class="metric"><span>{escape(label)}</span><strong>{escape(value)}</strong></div>'


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


def bill_path(chat_id: int, day_key: str, token: str | None = None) -> str:
    path = f"/bill/{chat_id}/{day_key}"
    if token:
        path = f"{path}?{urlencode({'token': token})}"
    return path


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
    :root {{
      color-scheme: light;
      --bg: #f5f7fb;
      --panel: #ffffff;
      --line: #dce3ee;
      --text: #172033;
      --muted: #667085;
      --accent: #0f766e;
    }}
    * {{ box-sizing: border-box; }}
    body {{
      margin: 0;
      background: var(--bg);
      color: var(--text);
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", "Microsoft YaHei", sans-serif;
      font-size: 14px;
      line-height: 1.5;
    }}
    .topbar {{
      display: flex;
      justify-content: space-between;
      gap: 24px;
      padding: 28px clamp(16px, 4vw, 44px) 18px;
      align-items: flex-end;
    }}
    h1 {{ margin: 0; font-size: 26px; font-weight: 700; }}
    h2 {{ margin: 0 0 12px; font-size: 18px; }}
    h2 span {{ color: var(--muted); font-size: 14px; font-weight: 500; }}
    .eyebrow {{ color: var(--accent); font-weight: 700; margin-bottom: 4px; }}
    .muted {{ color: var(--muted); }}
    .nav {{ display: flex; gap: 8px; flex-wrap: wrap; justify-content: flex-end; }}
    .nav a, .nav span {{
      display: inline-flex;
      align-items: center;
      min-height: 34px;
      padding: 0 12px;
      border: 1px solid var(--line);
      border-radius: 6px;
      background: var(--panel);
      color: var(--text);
      text-decoration: none;
    }}
    .summary {{
      display: grid;
      grid-template-columns: repeat(6, minmax(120px, 1fr));
      gap: 10px;
      padding: 0 clamp(16px, 4vw, 44px) 18px;
    }}
    .metric {{
      background: var(--panel);
      border: 1px solid var(--line);
      border-radius: 8px;
      padding: 12px 14px;
    }}
    .metric span {{ display: block; color: var(--muted); margin-bottom: 4px; }}
    .metric strong {{ display: block; font-size: 16px; overflow-wrap: anywhere; }}
    .grid {{
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 14px;
      padding: 0 clamp(16px, 4vw, 44px) 40px;
    }}
    .panel {{
      background: var(--panel);
      border: 1px solid var(--line);
      border-radius: 8px;
      padding: 16px;
      min-width: 0;
    }}
    .table-wrap {{ overflow-x: auto; }}
    table {{ width: 100%; border-collapse: collapse; min-width: 640px; }}
    th, td {{ text-align: left; padding: 10px 8px; border-top: 1px solid var(--line); vertical-align: top; }}
    th {{ color: var(--muted); font-weight: 600; background: #f8fafc; }}
    td strong {{ font-weight: 700; }}
    .empty {{ color: var(--muted); text-align: center; padding: 26px 8px; }}
    .simple {{ padding: 40px clamp(16px, 4vw, 44px); }}
    @media (max-width: 1100px) {{
      .summary {{ grid-template-columns: repeat(3, minmax(120px, 1fr)); }}
      .grid {{ grid-template-columns: 1fr; }}
    }}
    @media (max-width: 640px) {{
      .topbar {{ display: block; }}
      .nav {{ justify-content: flex-start; margin-top: 14px; }}
      .summary {{ grid-template-columns: 1fr 1fr; }}
    }}
  </style>
</head>
<body>
{body}
</body>
</html>"""
