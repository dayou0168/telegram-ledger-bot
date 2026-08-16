import hashlib
import json
import os
import signal
import threading
import time
from concurrent.futures import ThreadPoolExecutor, as_completed

import requests
from confluent_kafka import Consumer, Producer, TopicPartition


TRON_URL = os.getenv("TRON_URL", "http://tron-lite:8090").rstrip("/")
KAFKA_BROKERS = os.getenv("KAFKA_BROKERS", "kafka:9092")
KAFKA_PENDING_TOPIC = os.getenv(
    "KAFKA_PENDING_TOPIC",
    os.getenv("KAFKA_TOPIC", "tron.pending"),
)
KAFKA_TRANSACTION_TOPIC = os.getenv("KAFKA_TRANSACTION_TOPIC", "tron.transaction")
KAFKA_CONFIRMED_TOPIC = os.getenv("KAFKA_CONFIRMED_TOPIC", "tron.confirmed")
KAFKA_CONFIRMED_GROUP_ID = os.getenv(
    "KAFKA_CONFIRMED_GROUP_ID",
    "tron-confirmed-parser-v1",
)
USDT_CONTRACT = os.getenv(
    "USDT_CONTRACT",
    "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
)
POLL_INTERVAL = max(0.05, int(os.getenv("POLL_INTERVAL_MS", "100")) / 1000.0)
HTTP_TIMEOUT = max(0.5, float(os.getenv("HTTP_TIMEOUT_SECONDS", "2")))
WORKERS = max(1, int(os.getenv("FETCH_WORKERS", "32")))
SEEN_TTL = max(60, int(os.getenv("SEEN_TTL_SECONDS", "600")))
LOG_EACH_EVENT = os.getenv("LOG_EACH_EVENT", "false").strip().lower() in {
    "1", "true", "yes", "on"
}

running = True
seen = {}
retry_after = {}
thread_local = threading.local()
stats_lock = threading.Lock()
stats = {
    "pending_polls": 0,
    "pending_ids": 0,
    "pending_details": 0,
    "pending_published": 0,
    "pending_irrelevant": 0,
    "pending_missed": 0,
    "pending_errors": 0,
    "confirmed_read": 0,
    "confirmed_published": 0,
    "confirmed_irrelevant": 0,
    "confirmed_errors": 0,
}


def inc(name, value=1):
    with stats_lock:
        stats[name] += value


def stop_handler(_signum, _frame):
    global running
    running = False


signal.signal(signal.SIGTERM, stop_handler)
signal.signal(signal.SIGINT, stop_handler)


producer = Producer({
    "bootstrap.servers": KAFKA_BROKERS,
    "client.id": "tron-transfer-collector",
    "enable.idempotence": True,
    "acks": "all",
    "linger.ms": 0,
    "delivery.timeout.ms": 10000,
    "request.timeout.ms": 5000,
    "compression.type": "lz4",
})


def http_session():
    session = getattr(thread_local, "session", None)
    if session is None:
        session = requests.Session()
        session.headers.update({
            "Accept": "application/json",
            "Content-Type": "application/json",
        })
        thread_local.session = session
    return session


def tron_post(path, body):
    response = http_session().post(TRON_URL + path, json=body, timeout=HTTP_TIMEOUT)
    response.raise_for_status()
    text = response.text.strip()
    if text.startswith("this API is closed"):
        raise RuntimeError(text)
    if not text:
        return {}
    return response.json()


def publish(topic, txid, event):
    outcome = {"done": False, "ok": False}

    def delivered(error, _message):
        outcome["done"] = True
        outcome["ok"] = error is None
        if error is not None:
            print(f"kafka delivery failed topic={topic} tx={txid}: {error}", flush=True)

    payload = json.dumps(event, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    while running:
        try:
            producer.produce(
                topic,
                key=txid.encode("ascii"),
                value=payload,
                on_delivery=delivered,
            )
            break
        except BufferError:
            producer.poll(0.1)
    producer.flush(5)
    return outcome["done"] and outcome["ok"]


def list_pending():
    result = tron_post("/wallet/gettransactionlistfrompending", {"visible": True})
    txids = result.get("txId") or []
    return list(dict.fromkeys(str(txid).strip() for txid in txids if str(txid).strip()))


def fetch_pending(txid):
    result = tron_post(
        "/wallet/gettransactionfrompending",
        {"value": txid, "visible": True},
    )
    raw_data = result.get("raw_data")
    if not raw_data:
        return txid, None, False
    contracts = raw_data.get("contract") or []
    if not contracts:
        return txid, None, True
    contract = contracts[0]
    contract_type = str(contract.get("type") or "")
    value = contract.get("parameter", {}).get("value", {}) or {}
    kind = ""
    if contract_type == "TransferContract":
        kind = "trx_transfer"
    elif (
        contract_type == "TriggerSmartContract"
        and value.get("contract_address") == USDT_CONTRACT
    ):
        data = str(value.get("data") or "").lower()
        if data.startswith("a9059cbb"):
            kind = "usdt_transfer"
        elif data.startswith("23b872dd"):
            kind = "usdt_transfer_from"
        else:
            return txid, None, True
    else:
        return txid, None, True
    return txid, {
        "schema": "tron.pending.v1",
        "source": "lite_pending",
        "observedAt": int(time.time() * 1000),
        "txId": txid,
        "kind": kind,
        "contractType": contract_type,
        "timestamp": raw_data.get("timestamp", 0),
        "expiration": raw_data.get("expiration", 0),
        "contractValue": value,
    }, True


def base58check_tron(hex20):
    raw = bytes.fromhex(hex20[-40:])
    if len(raw) != 20:
        raise ValueError("invalid TRON address word")
    payload = b"\x41" + raw
    checksum = hashlib.sha256(hashlib.sha256(payload).digest()).digest()[:4]
    data = payload + checksum
    alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
    number = int.from_bytes(data, "big")
    encoded = ""
    while number:
        number, remainder = divmod(number, 58)
        encoded = alphabet[remainder] + encoded
    leading = len(data) - len(data.lstrip(b"\0"))
    return alphabet[0] * leading + encoded


def decode_usdt_call(owner, calldata):
    data = str(calldata or "").lower().removeprefix("0x")
    if data.startswith("a9059cbb") and len(data) >= 8 + 64 * 2:
        words = [data[8:72], data[72:136]]
        return "usdt_transfer", owner, base58check_tron(words[0]), str(int(words[1], 16))
    if data.startswith("23b872dd") and len(data) >= 8 + 64 * 3:
        words = [data[8:72], data[72:136], data[136:200]]
        return (
            "usdt_transfer_from",
            base58check_tron(words[0]),
            base58check_tron(words[1]),
            str(int(words[2], 16)),
        )
    return None


def normalize_status(value):
    value = str(value or "").strip().upper()
    return "SUCCESS" if value == "SUCCESS" else "FAILED"


def parse_confirmed_event(source):
    txid = str(source.get("transactionId") or source.get("txID") or "").strip().lower()
    if not txid:
        return None
    contract_type = str(source.get("contractType") or "")
    status = normalize_status(source.get("result"))
    timestamp = int(source.get("timeStamp") or 0)
    block_number = int(source.get("blockNumber") or 0)
    transaction_index = str(source.get("transactionIndex") or "0")

    if contract_type == "TransferContract":
        from_address = str(source.get("fromAddress") or "").strip()
        to_address = str(source.get("toAddress") or "").strip()
        amount = str(source.get("assetAmount") or "").strip()
        if not from_address or not to_address or not amount:
            tx = tron_post(
                "/wallet/gettransactionbyid",
                {"value": txid, "visible": True},
            )
            raw_data = tx.get("raw_data") or {}
            contracts = raw_data.get("contract") or []
            if not contracts:
                raise RuntimeError("confirmed TRX transaction has no contract")
            value = contracts[0].get("parameter", {}).get("value", {}) or {}
            from_address = str(value.get("owner_address") or "").strip()
            to_address = str(value.get("to_address") or "").strip()
            amount = str(value.get("amount") or "").strip()
            timestamp = timestamp or int(raw_data.get("timestamp") or 0)
        if not from_address or not to_address or not amount:
            raise RuntimeError("confirmed TRX fields are incomplete")
        kind, symbol, token, decimals = "trx_transfer", "TRX", "trx", 6

    elif (
        contract_type == "TriggerSmartContract"
        and str(source.get("contractAddress") or "").strip() == USDT_CONTRACT
    ):
        tx = tron_post(
            "/wallet/gettransactionbyid",
            {"value": txid, "visible": True},
        )
        raw_data = tx.get("raw_data") or {}
        contracts = raw_data.get("contract") or []
        if not contracts:
            raise RuntimeError("confirmed USDT transaction has no contract")
        value = contracts[0].get("parameter", {}).get("value", {}) or {}
        decoded = decode_usdt_call(value.get("owner_address"), value.get("data"))
        if decoded is None:
            return None
        kind, from_address, to_address, amount = decoded
        timestamp = timestamp or int(raw_data.get("timestamp") or 0)
        ret = tx.get("ret") or []
        if ret:
            status = normalize_status(ret[0].get("contractRet"))
        symbol, token, decimals = "USDT", USDT_CONTRACT, 6
    else:
        return None

    return {
        "schema": "tron.confirmed.v1",
        "source": "lite_event_plugin",
        "observedAt": int(time.time() * 1000),
        "txId": txid,
        "kind": kind,
        "contractType": contract_type,
        "timestamp": timestamp,
        "blockNumber": block_number,
        "transactionIndex": transaction_index,
        "status": status,
        "fromAddress": from_address,
        "toAddress": to_address,
        "amount": amount,
        "tokenSymbol": symbol,
        "tokenAddress": token,
        "tokenDecimals": decimals,
    }


def confirmed_loop():
    consumer = Consumer({
        "bootstrap.servers": KAFKA_BROKERS,
        "client.id": "tron-confirmed-parser",
        "group.id": KAFKA_CONFIRMED_GROUP_ID,
        "enable.auto.commit": False,
        "auto.offset.reset": "latest",
        "allow.auto.create.topics": False,
    })
    consumer.subscribe([KAFKA_TRANSACTION_TOPIC])
    print(
        "starting confirmed parser "
        f"input={KAFKA_TRANSACTION_TOPIC} output={KAFKA_CONFIRMED_TOPIC} "
        f"group={KAFKA_CONFIRMED_GROUP_ID}",
        flush=True,
    )
    try:
        while running:
            message = consumer.poll(0.5)
            if message is None:
                continue
            if message.error():
                inc("confirmed_errors")
                print(f"confirmed kafka read error: {message.error()}", flush=True)
                time.sleep(1)
                continue
            inc("confirmed_read")
            try:
                source = json.loads(message.value().decode("utf-8"))
                event = parse_confirmed_event(source)
                if event is None:
                    inc("confirmed_irrelevant")
                elif publish(KAFKA_CONFIRMED_TOPIC, event["txId"], event):
                    inc("confirmed_published")
                    if LOG_EACH_EVENT:
                        print(
                            f"confirmed tx={event['txId']} kind={event['kind']} "
                            f"status={event['status']}",
                            flush=True,
                        )
                else:
                    raise RuntimeError("confirmed Kafka delivery did not complete")
                consumer.commit(message=message, asynchronous=False)
            except (json.JSONDecodeError, UnicodeDecodeError) as exc:
                inc("confirmed_errors")
                print(f"drop invalid transaction event: {exc}", flush=True)
                consumer.commit(message=message, asynchronous=False)
            except Exception as exc:
                inc("confirmed_errors")
                print(
                    f"confirmed parse error tx_offset={message.offset()}: "
                    f"{type(exc).__name__}: {exc}",
                    flush=True,
                )
                consumer.seek(TopicPartition(message.topic(), message.partition(), message.offset()))
                time.sleep(1)
    finally:
        consumer.close()


def cleanup_state(now):
    for txid in [key for key, timestamp in seen.items() if now - timestamp >= SEEN_TTL]:
        seen.pop(txid, None)
    for txid in [key for key, timestamp in retry_after.items() if timestamp <= now]:
        retry_after.pop(txid, None)


def print_stats():
    with stats_lock:
        snapshot = dict(stats)
    print("stats " + " ".join(f"{key}={value}" for key, value in snapshot.items()), flush=True)


def pending_loop():
    last_stats = time.monotonic()
    last_cleanup = time.monotonic()
    with ThreadPoolExecutor(max_workers=WORKERS) as pool:
        while running:
            loop_started = time.monotonic()
            try:
                txids = list_pending()
                inc("pending_polls")
                inc("pending_ids", len(txids))
                now = time.monotonic()
                candidates = [
                    txid for txid in txids
                    if txid not in seen and retry_after.get(txid, 0) <= now
                ]
                futures = {pool.submit(fetch_pending, txid): txid for txid in candidates}
                for future in as_completed(futures):
                    txid = futures[future]
                    try:
                        fetched_txid, event, complete = future.result()
                        inc("pending_details")
                        if not complete:
                            inc("pending_missed")
                            retry_after[txid] = time.monotonic() + 0.25
                            continue
                        if event is None:
                            inc("pending_irrelevant")
                            seen[fetched_txid] = time.monotonic()
                            continue
                        if publish(KAFKA_PENDING_TOPIC, fetched_txid, event):
                            seen[fetched_txid] = time.monotonic()
                            inc("pending_published")
                            if LOG_EACH_EVENT:
                                print(
                                    f"pending tx={fetched_txid} kind={event['kind']}",
                                    flush=True,
                                )
                        else:
                            inc("pending_errors")
                            retry_after[txid] = time.monotonic() + 1
                    except Exception as exc:
                        inc("pending_errors")
                        retry_after[txid] = time.monotonic() + 1
                        print(
                            f"pending fetch error tx={txid}: "
                            f"{type(exc).__name__}: {exc}",
                            flush=True,
                        )
            except Exception as exc:
                inc("pending_errors")
                print(f"pending poll error: {type(exc).__name__}: {exc}", flush=True)
                time.sleep(1)

            now = time.monotonic()
            if now - last_cleanup >= 60:
                cleanup_state(now)
                last_cleanup = now
            if now - last_stats >= 10:
                print_stats()
                last_stats = now
            remaining = POLL_INTERVAL - (time.monotonic() - loop_started)
            if remaining > 0:
                time.sleep(remaining)


def main():
    print(
        "starting tron transfer collector "
        f"tron={TRON_URL} kafka={KAFKA_BROKERS} "
        f"pending={KAFKA_PENDING_TOPIC} poll_ms={int(POLL_INTERVAL * 1000)} "
        f"workers={WORKERS}",
        flush=True,
    )
    confirmed_thread = threading.Thread(
        target=confirmed_loop,
        name="confirmed-parser",
        daemon=True,
    )
    confirmed_thread.start()
    try:
        pending_loop()
    finally:
        producer.flush(10)
        confirmed_thread.join(timeout=5)
        print("collector stopped", flush=True)


if __name__ == "__main__":
    main()
