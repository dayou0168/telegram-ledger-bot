from __future__ import annotations

import sqlite3
from dataclasses import dataclass
from datetime import datetime, timedelta, tzinfo
import json
from pathlib import Path
import threading
from typing import Any


@dataclass(frozen=True)
class TelegramUser:
    user_id: int
    username: str | None
    display_name: str

    @property
    def username_norm(self) -> str | None:
        return self.username.lower() if self.username else None


class Storage:
    def __init__(self, path: Path):
        path.parent.mkdir(parents=True, exist_ok=True)
        self.conn = sqlite3.connect(path, timeout=30)
        self.conn.row_factory = sqlite3.Row
        self.conn.execute("PRAGMA foreign_keys = ON")
        self.conn.execute("PRAGMA busy_timeout = 30000")
        self.conn.execute("PRAGMA journal_mode = WAL")
        self.migrate()

    def migrate(self) -> None:
        self.conn.executescript(
            """
            CREATE TABLE IF NOT EXISTS groups (
                chat_id INTEGER PRIMARY KEY,
                chat_title TEXT,
                active INTEGER NOT NULL DEFAULT 0,
                business_open INTEGER NOT NULL DEFAULT 1,
                owner_user_id INTEGER,
                deposit_fee_rate TEXT NOT NULL DEFAULT '0',
                payout_fee_rate TEXT NOT NULL DEFAULT '0',
                deposit_exchange_rate TEXT NOT NULL DEFAULT '1',
                payout_exchange_rate TEXT NOT NULL DEFAULT '1',
                currency TEXT NOT NULL DEFAULT 'USDT',
                payout_mode TEXT NOT NULL DEFAULT 'cny',
                multiply_exchange INTEGER NOT NULL DEFAULT 0,
                show_cny INTEGER NOT NULL DEFAULT 1,
                all_members_can_record INTEGER NOT NULL DEFAULT 0,
                simple_limit INTEGER,
                day_cutoff_hour INTEGER NOT NULL DEFAULT 0,
                pending_day_cutoff_hour INTEGER,
                pending_day_cutoff_effective_at TEXT,
                open_bill_day_key TEXT,
                open_bill_begin_at TEXT,
                open_bill_end_at TEXT,
                pin_enabled INTEGER NOT NULL DEFAULT 0,
                realtime_rate INTEGER NOT NULL DEFAULT 0,
                realtime_rate_rank INTEGER,
                realtime_rate_offset TEXT NOT NULL DEFAULT '0',
                activated_at TEXT,
                trial_started_at TEXT,
                trial_until TEXT,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL
            );

            CREATE TABLE IF NOT EXISTS users (
                user_id INTEGER PRIMARY KEY,
                username TEXT,
                display_name TEXT NOT NULL,
                last_seen_at TEXT NOT NULL
            );

            CREATE TABLE IF NOT EXISTS group_members (
                chat_id INTEGER NOT NULL,
                user_id INTEGER NOT NULL,
                username TEXT,
                display_name TEXT NOT NULL,
                last_seen_at TEXT NOT NULL,
                PRIMARY KEY (chat_id, user_id)
            );

            CREATE TABLE IF NOT EXISTS operators (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                chat_id INTEGER NOT NULL,
                user_id INTEGER,
                username_norm TEXT,
                username TEXT,
                display_name TEXT NOT NULL,
                role TEXT NOT NULL DEFAULT 'operator',
                added_by INTEGER,
                created_at TEXT NOT NULL
            );
            CREATE UNIQUE INDEX IF NOT EXISTS idx_operators_user
                ON operators(chat_id, user_id)
                WHERE user_id IS NOT NULL;
            CREATE UNIQUE INDEX IF NOT EXISTS idx_operators_username
                ON operators(chat_id, username_norm)
                WHERE username_norm IS NOT NULL;

            CREATE TABLE IF NOT EXISTS records (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                chat_id INTEGER NOT NULL,
                kind TEXT NOT NULL,
                amount TEXT NOT NULL,
                currency TEXT NOT NULL,
                exchange_rate TEXT NOT NULL,
                fee_rate TEXT NOT NULL,
                amount_cny TEXT NOT NULL,
                amount_usdt TEXT NOT NULL,
                commission_cny TEXT NOT NULL,
                net_usdt TEXT NOT NULL,
                actor_user_id INTEGER NOT NULL,
                actor_name TEXT NOT NULL,
                subject_user_id INTEGER,
                subject_name TEXT,
                note TEXT,
                is_balance INTEGER NOT NULL DEFAULT 0,
                source_message_id INTEGER,
                bot_message_id INTEGER,
                day_key TEXT NOT NULL,
                created_at TEXT NOT NULL,
                deleted_at TEXT
            );

            CREATE INDEX IF NOT EXISTS idx_records_chat_day
                ON records(chat_id, day_key, deleted_at);
            CREATE INDEX IF NOT EXISTS idx_records_chat_kind
                ON records(chat_id, kind, id);

            CREATE TABLE IF NOT EXISTS address_watches (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                owner_user_id INTEGER NOT NULL,
                network TEXT NOT NULL,
                address TEXT NOT NULL,
                label TEXT,
                active INTEGER NOT NULL DEFAULT 1,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL
            );
            CREATE UNIQUE INDEX IF NOT EXISTS idx_address_watches_owner_address
                ON address_watches(owner_user_id, address);

            CREATE TABLE IF NOT EXISTS address_watch_settings (
                owner_user_id INTEGER PRIMARY KEY,
                watch_income INTEGER NOT NULL DEFAULT 1,
                watch_expense INTEGER NOT NULL DEFAULT 1,
                notify_trx INTEGER NOT NULL DEFAULT 1,
                display_mode TEXT NOT NULL DEFAULT 'compact',
                min_notify_amount TEXT NOT NULL DEFAULT '0',
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL
            );

            CREATE TABLE IF NOT EXISTS chain_event_notifications (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                owner_user_id INTEGER NOT NULL,
                address TEXT NOT NULL,
                tx_hash TEXT NOT NULL,
                direction TEXT NOT NULL,
                token_symbol TEXT NOT NULL,
                block_timestamp INTEGER NOT NULL,
                created_at TEXT NOT NULL,
                UNIQUE(owner_user_id, address, tx_hash, direction)
            );

            CREATE TABLE IF NOT EXISTS processed_updates (
                update_id INTEGER PRIMARY KEY,
                processed_at TEXT NOT NULL
            );

            CREATE TABLE IF NOT EXISTS address_verifications (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                chat_id INTEGER NOT NULL,
                address TEXT NOT NULL,
                count INTEGER NOT NULL DEFAULT 0,
                last_sender_user_id INTEGER NOT NULL,
                last_sender_name TEXT NOT NULL,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL,
                UNIQUE(chat_id, address)
            );

            CREATE TABLE IF NOT EXISTS address_whitelist (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                chat_id INTEGER NOT NULL,
                network TEXT NOT NULL DEFAULT 'TRC20',
                address TEXT NOT NULL,
                label TEXT,
                image_url TEXT,
                enabled INTEGER NOT NULL DEFAULT 1,
                created_by INTEGER,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL,
                UNIQUE(chat_id, address)
            );

            CREATE TABLE IF NOT EXISTS broadcast_jobs (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                creator_user_id INTEGER NOT NULL,
                scope TEXT NOT NULL,
                target_chat_ids TEXT NOT NULL,
                text TEXT NOT NULL,
                source_chat_id INTEGER,
                source_message_id INTEGER,
                message_kind TEXT NOT NULL DEFAULT 'text',
                photo TEXT,
                notify_all INTEGER NOT NULL DEFAULT 0,
                status TEXT NOT NULL DEFAULT 'pending',
                success_count INTEGER NOT NULL DEFAULT 0,
                failure_count INTEGER NOT NULL DEFAULT 0,
                created_at TEXT NOT NULL,
                confirmed_at TEXT,
                completed_at TEXT,
                updated_at TEXT
            );

            CREATE TABLE IF NOT EXISTS broadcast_job_targets (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                job_id INTEGER NOT NULL,
                target_chat_id INTEGER NOT NULL,
                status TEXT NOT NULL DEFAULT 'pending',
                sent_message_id INTEGER,
                error_message TEXT,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL,
                UNIQUE(job_id, target_chat_id),
                FOREIGN KEY(job_id) REFERENCES broadcast_jobs(id) ON DELETE CASCADE
            );

            CREATE TABLE IF NOT EXISTS broadcast_operators (
                user_id INTEGER PRIMARY KEY,
                username TEXT,
                display_name TEXT,
                remark TEXT,
                status TEXT NOT NULL DEFAULT 'active',
                allow_group_broadcast INTEGER NOT NULL DEFAULT 1,
                allow_direct_send INTEGER NOT NULL DEFAULT 1,
                allow_manage_operators INTEGER NOT NULL DEFAULT 1,
                receive_sent_notifications INTEGER NOT NULL DEFAULT 0,
                receive_reply_notifications INTEGER NOT NULL DEFAULT 0,
                created_by INTEGER NOT NULL,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL
            );

            CREATE TABLE IF NOT EXISTS broadcast_groups (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                name TEXT NOT NULL,
                name_norm TEXT NOT NULL UNIQUE,
                created_by INTEGER NOT NULL,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL
            );

            CREATE TABLE IF NOT EXISTS broadcast_group_members (
                group_id INTEGER NOT NULL,
                chat_id INTEGER NOT NULL,
                created_at TEXT NOT NULL,
                PRIMARY KEY (group_id, chat_id),
                FOREIGN KEY(group_id) REFERENCES broadcast_groups(id) ON DELETE CASCADE
            );

            CREATE TABLE IF NOT EXISTS broadcast_group_permissions (
                user_id INTEGER NOT NULL,
                group_id INTEGER NOT NULL,
                created_by INTEGER NOT NULL,
                created_at TEXT NOT NULL,
                PRIMARY KEY (user_id, group_id),
                FOREIGN KEY(group_id) REFERENCES broadcast_groups(id) ON DELETE CASCADE
            );

            CREATE TABLE IF NOT EXISTS broadcast_chat_permissions (
                user_id INTEGER NOT NULL,
                chat_id INTEGER NOT NULL,
                created_by INTEGER NOT NULL,
                created_at TEXT NOT NULL,
                PRIMARY KEY (user_id, chat_id)
            );

            CREATE TABLE IF NOT EXISTS broadcast_replacement_settings (
                id INTEGER PRIMARY KEY CHECK (id = 1),
                enabled INTEGER NOT NULL DEFAULT 0,
                text TEXT,
                photo TEXT,
                updated_by INTEGER,
                updated_at TEXT NOT NULL
            );
            """
        )
        self._add_missing_columns(
            "groups",
            {
                "deposit_fee_rate": "TEXT NOT NULL DEFAULT '0'",
                "payout_fee_rate": "TEXT NOT NULL DEFAULT '0'",
                "deposit_exchange_rate": "TEXT NOT NULL DEFAULT '1'",
                "payout_exchange_rate": "TEXT NOT NULL DEFAULT '1'",
                "currency": "TEXT NOT NULL DEFAULT 'USDT'",
                "payout_mode": "TEXT NOT NULL DEFAULT 'cny'",
                "multiply_exchange": "INTEGER NOT NULL DEFAULT 0",
                "show_cny": "INTEGER NOT NULL DEFAULT 1",
                "all_members_can_record": "INTEGER NOT NULL DEFAULT 0",
                "pending_day_cutoff_hour": "INTEGER",
                "pending_day_cutoff_effective_at": "TEXT",
                "open_bill_day_key": "TEXT",
                "open_bill_begin_at": "TEXT",
                "open_bill_end_at": "TEXT",
                "realtime_rate": "INTEGER NOT NULL DEFAULT 0",
                "realtime_rate_rank": "INTEGER",
                "realtime_rate_offset": "TEXT NOT NULL DEFAULT '0'",
            },
        )
        self._add_missing_columns(
            "records",
            {
                "bot_message_id": "INTEGER",
            },
        )
        self._add_missing_columns(
            "broadcast_operators",
            {
                "allow_group_broadcast": "INTEGER NOT NULL DEFAULT 1",
                "allow_direct_send": "INTEGER NOT NULL DEFAULT 1",
                "allow_manage_operators": "INTEGER NOT NULL DEFAULT 1",
                "receive_sent_notifications": "INTEGER NOT NULL DEFAULT 0",
                "receive_reply_notifications": "INTEGER NOT NULL DEFAULT 0",
            },
        )
        self._add_missing_columns(
            "broadcast_jobs",
            {
                "source_chat_id": "INTEGER",
                "source_message_id": "INTEGER",
                "message_kind": "TEXT NOT NULL DEFAULT 'text'",
                "photo": "TEXT",
                "notify_all": "INTEGER NOT NULL DEFAULT 0",
                "updated_at": "TEXT",
            },
        )
        self._add_missing_columns(
            "address_watch_settings",
            {
                "min_notify_amount": "TEXT NOT NULL DEFAULT '0'",
            },
        )
        self.conn.commit()

    def claim_update(self, update_id: int, now: datetime) -> bool:
        cursor = self.conn.execute(
            """
            INSERT OR IGNORE INTO processed_updates(update_id, processed_at)
            VALUES (?, ?)
            """,
            (update_id, now.isoformat()),
        )
        self.conn.commit()
        return cursor.rowcount == 1

    def last_processed_update_id(self) -> int | None:
        row = self.conn.execute("SELECT MAX(update_id) AS update_id FROM processed_updates").fetchone()
        if row is None or row["update_id"] is None:
            return None
        return int(row["update_id"])

    def record_address_verification(
        self,
        *,
        chat_id: int,
        address: str,
        sender: TelegramUser,
        now: datetime,
    ) -> dict[str, Any]:
        sender_name = f"@{sender.username}" if sender.username else sender.display_name
        existing = self.conn.execute(
            "SELECT * FROM address_verifications WHERE chat_id = ? AND address = ?",
            (chat_id, address),
        ).fetchone()
        if existing is None:
            self.conn.execute(
                """
                INSERT INTO address_verifications(
                    chat_id,
                    address,
                    count,
                    last_sender_user_id,
                    last_sender_name,
                    created_at,
                    updated_at
                )
                VALUES (?, ?, 1, ?, ?, ?, ?)
                """,
                (
                    chat_id,
                    address,
                    sender.user_id,
                    sender_name,
                    now.isoformat(),
                    now.isoformat(),
                ),
            )
            self.conn.commit()
            return {"count": 1, "previous_sender_name": None, "current_sender_name": sender_name}

        new_count = int(existing["count"]) + 1
        previous_sender_name = existing["last_sender_name"]
        self.conn.execute(
            """
            UPDATE address_verifications
            SET count = ?,
                last_sender_user_id = ?,
                last_sender_name = ?,
                updated_at = ?
            WHERE chat_id = ? AND address = ?
            """,
            (
                new_count,
                sender.user_id,
                sender_name,
                now.isoformat(),
                chat_id,
                address,
            ),
        )
        self.conn.commit()
        return {"count": new_count, "previous_sender_name": previous_sender_name, "current_sender_name": sender_name}

    def list_broadcast_groups(self, *, chat_ids: list[int] | None = None) -> list[sqlite3.Row]:
        if chat_ids is not None:
            if not chat_ids:
                return []
            placeholders = ", ".join("?" for _ in chat_ids)
            return list(
                self.conn.execute(
                    f"""
                    SELECT * FROM groups
                    WHERE chat_id < 0 AND chat_id IN ({placeholders})
                    ORDER BY updated_at DESC, chat_id ASC
                    """,
                    chat_ids,
                )
            )
        return list(
            self.conn.execute(
                """
                SELECT * FROM groups
                WHERE chat_id < 0
                ORDER BY updated_at DESC, chat_id ASC
                """
            )
        )

    def list_realtime_rate_groups(self) -> list[sqlite3.Row]:
        return list(
            self.conn.execute(
                """
                SELECT * FROM groups
                WHERE chat_id < 0
                  AND realtime_rate = 1
                  AND realtime_rate_rank IS NOT NULL
                ORDER BY chat_id ASC
                """
            )
        )

    def create_broadcast_job(
        self,
        *,
        creator_user_id: int,
        scope: str,
        target_chat_ids: list[int],
        text: str,
        source_chat_id: int | None = None,
        source_message_id: int | None = None,
        message_kind: str = "text",
        photo: str | None = None,
        notify_all: bool = False,
        now: datetime,
    ) -> sqlite3.Row:
        cursor = self.conn.execute(
            """
            INSERT INTO broadcast_jobs(
                creator_user_id,
                scope,
                target_chat_ids,
                text,
                source_chat_id,
                source_message_id,
                message_kind,
                photo,
                notify_all,
                created_at,
                updated_at
            )
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            (
                creator_user_id,
                scope,
                json.dumps(target_chat_ids),
                text,
                source_chat_id,
                source_message_id,
                message_kind,
                photo,
                1 if notify_all else 0,
                now.isoformat(),
                now.isoformat(),
            ),
        )
        job_id = int(cursor.lastrowid)
        for target_chat_id in list(dict.fromkeys(target_chat_ids)):
            self.conn.execute(
                """
                INSERT OR IGNORE INTO broadcast_job_targets(
                    job_id,
                    target_chat_id,
                    status,
                    created_at,
                    updated_at
                )
                VALUES (?, ?, 'pending', ?, ?)
                """,
                (job_id, int(target_chat_id), now.isoformat(), now.isoformat()),
            )
        self.conn.commit()
        return self.get_broadcast_job(job_id)

    def get_broadcast_job(self, job_id: int) -> sqlite3.Row:
        row = self.conn.execute("SELECT * FROM broadcast_jobs WHERE id = ?", (job_id,)).fetchone()
        if row is None:
            raise KeyError(f"Broadcast job {job_id} is missing")
        return row

    def update_broadcast_job(self, job_id: int, now: datetime, **fields: Any) -> sqlite3.Row:
        fields["updated_at"] = now.isoformat()
        existing = {row["name"] for row in self.conn.execute("PRAGMA table_info(broadcast_jobs)")}
        fields = {key: value for key, value in fields.items() if key in existing}
        if not fields:
            return self.get_broadcast_job(job_id)
        columns = ", ".join(f"{name} = ?" for name in fields)
        values = list(fields.values())
        values.append(job_id)
        self.conn.execute(f"UPDATE broadcast_jobs SET {columns} WHERE id = ?", values)
        self.conn.commit()
        return self.get_broadcast_job(job_id)

    def mark_broadcast_job_target(
        self,
        job_id: int,
        target_chat_id: int,
        *,
        status: str,
        now: datetime,
        sent_message_id: int | None = None,
        error_message: str | None = None,
    ) -> None:
        self.conn.execute(
            """
            INSERT INTO broadcast_job_targets(
                job_id,
                target_chat_id,
                status,
                sent_message_id,
                error_message,
                created_at,
                updated_at
            )
            VALUES (?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT(job_id, target_chat_id) DO UPDATE SET
                status = excluded.status,
                sent_message_id = excluded.sent_message_id,
                error_message = excluded.error_message,
                updated_at = excluded.updated_at
            """,
            (
                job_id,
                target_chat_id,
                status,
                sent_message_id,
                error_message,
                now.isoformat(),
                now.isoformat(),
            ),
        )
        self.conn.commit()

    def list_broadcast_job_targets(self, job_id: int) -> list[sqlite3.Row]:
        return list(
            self.conn.execute(
                """
                SELECT bjt.*, g.chat_title
                FROM broadcast_job_targets bjt
                LEFT JOIN groups g ON g.chat_id = bjt.target_chat_id
                WHERE bjt.job_id = ?
                ORDER BY bjt.id ASC
                """,
                (job_id,),
            )
        )

    def find_broadcast_job_by_sent_message(self, target_chat_id: int, sent_message_id: int) -> sqlite3.Row | None:
        return self.conn.execute(
            """
            SELECT
                bjt.*,
                bj.creator_user_id,
                bj.scope,
                bj.text,
                bj.message_kind,
                bj.created_at AS job_created_at
            FROM broadcast_job_targets bjt
            JOIN broadcast_jobs bj ON bj.id = bjt.job_id
            WHERE bjt.target_chat_id = ?
              AND bjt.sent_message_id = ?
              AND bjt.status = 'sent'
            ORDER BY bjt.id DESC
            LIMIT 1
            """,
            (target_chat_id, sent_message_id),
        ).fetchone()

    @staticmethod
    def normalize_broadcast_group_name(name: str) -> str:
        return name.strip().lower()

    def create_named_broadcast_group(self, name: str, *, created_by: int, now: datetime) -> sqlite3.Row:
        clean_name = name.strip()
        if not clean_name:
            raise ValueError("Broadcast group name is empty")
        name_norm = self.normalize_broadcast_group_name(clean_name)
        self.conn.execute(
            """
            INSERT INTO broadcast_groups(name, name_norm, created_by, created_at, updated_at)
            VALUES (?, ?, ?, ?, ?)
            ON CONFLICT(name_norm) DO UPDATE SET
                name = excluded.name,
                updated_at = excluded.updated_at
            """,
            (clean_name, name_norm, created_by, now.isoformat(), now.isoformat()),
        )
        self.conn.commit()
        row = self.get_named_broadcast_group(clean_name)
        if row is None:
            raise KeyError(f"Broadcast group {clean_name} is missing")
        return row

    def get_named_broadcast_group(self, name: str) -> sqlite3.Row | None:
        return self.conn.execute(
            "SELECT * FROM broadcast_groups WHERE name_norm = ?",
            (self.normalize_broadcast_group_name(name),),
        ).fetchone()

    def get_named_broadcast_group_by_id(self, group_id: int) -> sqlite3.Row | None:
        return self.conn.execute(
            "SELECT * FROM broadcast_groups WHERE id = ?",
            (group_id,),
        ).fetchone()

    def delete_named_broadcast_group(self, name: str) -> int:
        group = self.get_named_broadcast_group(name)
        if group is not None:
            self.conn.execute("DELETE FROM broadcast_group_permissions WHERE group_id = ?", (group["id"],))
            self.conn.execute("DELETE FROM broadcast_group_members WHERE group_id = ?", (group["id"],))
        cursor = self.conn.execute(
            "DELETE FROM broadcast_groups WHERE name_norm = ?",
            (self.normalize_broadcast_group_name(name),),
        )
        self.conn.commit()
        return cursor.rowcount

    def list_named_broadcast_groups(self) -> list[sqlite3.Row]:
        return list(
            self.conn.execute(
                """
                SELECT bg.*, COUNT(bgm.chat_id) AS member_count
                FROM broadcast_groups bg
                LEFT JOIN broadcast_group_members bgm ON bgm.group_id = bg.id
                GROUP BY bg.id
                ORDER BY bg.updated_at DESC, bg.id ASC
                """
            )
        )

    def add_broadcast_group_members(
        self,
        name: str,
        chat_ids: list[int],
        *,
        now: datetime,
    ) -> tuple[int, list[int], list[int]]:
        group = self.get_named_broadcast_group(name)
        if group is None:
            raise KeyError(f"Broadcast group {name} is missing")
        unique_ids = list(dict.fromkeys(chat_ids))
        known_rows = self.list_broadcast_groups(chat_ids=unique_ids)
        known_ids = {int(row["chat_id"]) for row in known_rows}
        missing_ids = [chat_id for chat_id in unique_ids if chat_id not in known_ids]
        added = 0
        for chat_id in unique_ids:
            if chat_id not in known_ids:
                continue
            cursor = self.conn.execute(
                """
                INSERT OR IGNORE INTO broadcast_group_members(group_id, chat_id, created_at)
                VALUES (?, ?, ?)
                """,
                (group["id"], chat_id, now.isoformat()),
            )
            added += cursor.rowcount
        self.conn.execute(
            "UPDATE broadcast_groups SET updated_at = ? WHERE id = ?",
            (now.isoformat(), group["id"]),
        )
        self.conn.commit()
        return added, sorted(known_ids), missing_ids

    def remove_broadcast_group_members(self, name: str, chat_ids: list[int], *, now: datetime) -> int:
        group = self.get_named_broadcast_group(name)
        if group is None:
            raise KeyError(f"Broadcast group {name} is missing")
        unique_ids = list(dict.fromkeys(chat_ids))
        if not unique_ids:
            return 0
        placeholders = ", ".join("?" for _ in unique_ids)
        cursor = self.conn.execute(
            f"""
            DELETE FROM broadcast_group_members
            WHERE group_id = ? AND chat_id IN ({placeholders})
            """,
            [group["id"], *unique_ids],
        )
        self.conn.execute(
            "UPDATE broadcast_groups SET updated_at = ? WHERE id = ?",
            (now.isoformat(), group["id"]),
        )
        self.conn.commit()
        return cursor.rowcount

    def list_broadcast_group_members(self, name: str) -> list[sqlite3.Row]:
        group = self.get_named_broadcast_group(name)
        if group is None:
            raise KeyError(f"Broadcast group {name} is missing")
        return list(
            self.conn.execute(
                """
                SELECT bgm.chat_id, bgm.created_at, g.chat_title, g.updated_at AS group_updated_at
                FROM broadcast_group_members bgm
                LEFT JOIN groups g ON g.chat_id = bgm.chat_id
                WHERE bgm.group_id = ?
                ORDER BY g.updated_at DESC, bgm.chat_id ASC
                """,
                (group["id"],),
            )
        )

    def target_chat_ids_for_broadcast_group(self, name: str) -> list[int]:
        return [int(row["chat_id"]) for row in self.list_broadcast_group_members(name)]

    def add_broadcast_operator(
        self,
        *,
        user_id: int,
        created_by: int,
        now: datetime,
        username: str | None = None,
        display_name: str | None = None,
        remark: str | None = None,
    ) -> sqlite3.Row:
        self.conn.execute(
            """
            INSERT INTO broadcast_operators(
                user_id,
                username,
                display_name,
                remark,
                status,
                created_by,
                created_at,
                updated_at
            )
            VALUES (?, ?, ?, ?, 'active', ?, ?, ?)
            ON CONFLICT(user_id) DO UPDATE SET
                username = COALESCE(excluded.username, broadcast_operators.username),
                display_name = COALESCE(excluded.display_name, broadcast_operators.display_name),
                remark = COALESCE(excluded.remark, broadcast_operators.remark),
                status = 'active',
                updated_at = excluded.updated_at
            """,
            (
                user_id,
                username,
                display_name,
                remark,
                created_by,
                now.isoformat(),
                now.isoformat(),
            ),
        )
        self.conn.commit()
        row = self.get_broadcast_operator(user_id, active_only=False)
        if row is None:
            raise KeyError(f"Broadcast operator {user_id} is missing")
        return row

    def get_broadcast_operator(self, user_id: int, *, active_only: bool = True) -> sqlite3.Row | None:
        active_clause = "AND status = 'active'" if active_only else ""
        return self.conn.execute(
            f"""
            SELECT * FROM broadcast_operators
            WHERE user_id = ?
            {active_clause}
            """,
            (user_id,),
        ).fetchone()

    def list_broadcast_operators(self, *, active_only: bool = False) -> list[sqlite3.Row]:
        where = "WHERE status = 'active'" if active_only else ""
        return list(
            self.conn.execute(
                f"""
                SELECT * FROM broadcast_operators
                {where}
                ORDER BY status ASC, created_at ASC, user_id ASC
                """
            )
        )

    def list_child_broadcast_operators(self, manager_user_id: int, *, active_only: bool = False) -> list[sqlite3.Row]:
        active_clause = "AND status = 'active'" if active_only else ""
        return list(
            self.conn.execute(
                f"""
                SELECT * FROM broadcast_operators
                WHERE created_by = ?
                {active_clause}
                ORDER BY status ASC, created_at ASC, user_id ASC
                """,
                (manager_user_id,),
            )
        )

    def update_broadcast_operator_remark(self, *, user_id: int, remark: str | None, now: datetime) -> bool:
        cursor = self.conn.execute(
            """
            UPDATE broadcast_operators
            SET remark = ?, updated_at = ?
            WHERE user_id = ?
            """,
            (remark, now.isoformat(), user_id),
        )
        self.conn.commit()
        return cursor.rowcount > 0

    def update_broadcast_operator_features(
        self,
        *,
        user_id: int,
        now: datetime,
        allow_group_broadcast: int,
        allow_direct_send: int,
        allow_manage_operators: int,
        receive_sent_notifications: int,
        receive_reply_notifications: int,
    ) -> bool:
        cursor = self.conn.execute(
            """
            UPDATE broadcast_operators
            SET allow_group_broadcast = ?,
                allow_direct_send = ?,
                allow_manage_operators = ?,
                receive_sent_notifications = ?,
                receive_reply_notifications = ?,
                updated_at = ?
            WHERE user_id = ?
            """,
            (
                1 if allow_group_broadcast else 0,
                1 if allow_direct_send else 0,
                1 if allow_manage_operators else 0,
                1 if receive_sent_notifications else 0,
                1 if receive_reply_notifications else 0,
                now.isoformat(),
                user_id,
            ),
        )
        self.conn.commit()
        return cursor.rowcount > 0

    def disable_broadcast_operator(self, *, user_id: int, now: datetime) -> bool:
        cursor = self.conn.execute(
            """
            UPDATE broadcast_operators
            SET status = 'disabled', updated_at = ?
            WHERE user_id = ? AND status <> 'disabled'
            """,
            (now.isoformat(), user_id),
        )
        self.conn.execute("DELETE FROM broadcast_group_permissions WHERE user_id = ?", (user_id,))
        self.conn.execute("DELETE FROM broadcast_chat_permissions WHERE user_id = ?", (user_id,))
        self.conn.commit()
        return cursor.rowcount > 0

    def list_broadcast_operator_ids_with_feature(self, feature: str) -> set[int]:
        feature_columns = {
            "group_broadcast": "allow_group_broadcast",
            "direct_send": "allow_direct_send",
            "manage_operators": "allow_manage_operators",
            "sent_notifications": "receive_sent_notifications",
            "reply_notifications": "receive_reply_notifications",
        }
        column = feature_columns.get(feature)
        if column is None:
            return set()
        rows = self.conn.execute(
            f"""
            SELECT user_id
            FROM broadcast_operators
            WHERE status = 'active'
              AND {column} = 1
            """
        )
        return {int(row["user_id"]) for row in rows}

    def user_has_any_broadcast_permissions(self, user_id: int) -> bool:
        return self.user_has_broadcast_group_permissions(user_id) or self.user_has_broadcast_chat_permissions(user_id)

    def grant_broadcast_group_permission(
        self,
        group_name: str,
        *,
        user_id: int,
        created_by: int,
        now: datetime,
    ) -> bool:
        group = self.get_named_broadcast_group(group_name)
        if group is None:
            raise KeyError(f"Broadcast group {group_name} is missing")
        cursor = self.conn.execute(
            """
            INSERT OR IGNORE INTO broadcast_group_permissions(user_id, group_id, created_by, created_at)
            VALUES (?, ?, ?, ?)
            """,
            (user_id, group["id"], created_by, now.isoformat()),
        )
        self.conn.commit()
        return cursor.rowcount == 1

    def revoke_broadcast_group_permission(self, group_name: str, *, user_id: int) -> int:
        group = self.get_named_broadcast_group(group_name)
        if group is None:
            raise KeyError(f"Broadcast group {group_name} is missing")
        cursor = self.conn.execute(
            "DELETE FROM broadcast_group_permissions WHERE user_id = ? AND group_id = ?",
            (user_id, group["id"]),
        )
        self.conn.commit()
        return cursor.rowcount

    def user_has_broadcast_group_permissions(self, user_id: int) -> bool:
        row = self.conn.execute(
            "SELECT 1 FROM broadcast_group_permissions WHERE user_id = ? LIMIT 1",
            (user_id,),
        ).fetchone()
        return row is not None

    def user_can_access_broadcast_group(self, user_id: int, group_name: str) -> bool:
        group = self.get_named_broadcast_group(group_name)
        if group is None:
            return False
        row = self.conn.execute(
            """
            SELECT 1 FROM broadcast_group_permissions
            WHERE user_id = ? AND group_id = ?
            LIMIT 1
            """,
            (user_id, group["id"]),
        ).fetchone()
        return row is not None

    def list_named_broadcast_groups_for_user(self, user_id: int) -> list[sqlite3.Row]:
        return list(
            self.conn.execute(
                """
                SELECT bg.*, COUNT(bgm.chat_id) AS member_count
                FROM broadcast_group_permissions p
                JOIN broadcast_groups bg ON bg.id = p.group_id
                LEFT JOIN broadcast_group_members bgm ON bgm.group_id = bg.id
                WHERE p.user_id = ?
                GROUP BY bg.id
                ORDER BY bg.updated_at DESC, bg.id ASC
                """,
                (user_id,),
            )
        )

    def target_chat_ids_for_user_broadcast_groups(self, user_id: int) -> list[int]:
        rows = self.conn.execute(
            """
            SELECT DISTINCT bgm.chat_id
            FROM broadcast_group_permissions p
            JOIN broadcast_group_members bgm ON bgm.group_id = p.group_id
            WHERE p.user_id = ?
            ORDER BY bgm.chat_id ASC
            """,
            (user_id,),
        )
        return [int(row["chat_id"]) for row in rows]

    def grant_broadcast_chat_permission(
        self,
        *,
        chat_id: int,
        user_id: int,
        created_by: int,
        now: datetime,
    ) -> bool:
        self.get_group(chat_id)
        cursor = self.conn.execute(
            """
            INSERT OR IGNORE INTO broadcast_chat_permissions(user_id, chat_id, created_by, created_at)
            VALUES (?, ?, ?, ?)
            """,
            (user_id, chat_id, created_by, now.isoformat()),
        )
        self.conn.commit()
        return cursor.rowcount == 1

    def revoke_broadcast_chat_permission(self, *, chat_id: int, user_id: int) -> int:
        cursor = self.conn.execute(
            "DELETE FROM broadcast_chat_permissions WHERE user_id = ? AND chat_id = ?",
            (user_id, chat_id),
        )
        self.conn.commit()
        return cursor.rowcount

    def user_has_broadcast_chat_permissions(self, user_id: int) -> bool:
        row = self.conn.execute(
            "SELECT 1 FROM broadcast_chat_permissions WHERE user_id = ? LIMIT 1",
            (user_id,),
        ).fetchone()
        return row is not None

    def user_can_access_broadcast_chat(self, user_id: int, chat_id: int) -> bool:
        row = self.conn.execute(
            """
            SELECT 1 FROM broadcast_chat_permissions
            WHERE user_id = ? AND chat_id = ?
            LIMIT 1
            """,
            (user_id, chat_id),
        ).fetchone()
        return row is not None

    def target_chat_ids_for_user_chat_permissions(self, user_id: int) -> list[int]:
        rows = self.conn.execute(
            """
            SELECT chat_id
            FROM broadcast_chat_permissions
            WHERE user_id = ?
            ORDER BY chat_id ASC
            """,
            (user_id,),
        )
        return [int(row["chat_id"]) for row in rows]

    def list_broadcast_group_permissions(self, user_id: int | None = None) -> list[sqlite3.Row]:
        where = ""
        params: list[Any] = []
        if user_id is not None:
            where = "WHERE p.user_id = ?"
            params.append(user_id)
        return list(
            self.conn.execute(
                f"""
                SELECT p.user_id, p.created_by, p.created_at, bg.name, bg.id AS group_id
                FROM broadcast_group_permissions p
                JOIN broadcast_groups bg ON bg.id = p.group_id
                {where}
                ORDER BY p.user_id ASC, bg.name ASC
                """,
                params,
            )
        )

    def list_broadcast_chat_permissions(self, user_id: int | None = None) -> list[sqlite3.Row]:
        where = ""
        params: list[Any] = []
        if user_id is not None:
            where = "WHERE p.user_id = ?"
            params.append(user_id)
        return list(
            self.conn.execute(
                f"""
                SELECT p.user_id, p.chat_id, p.created_by, p.created_at, g.chat_title
                FROM broadcast_chat_permissions p
                LEFT JOIN groups g ON g.chat_id = p.chat_id
                {where}
                ORDER BY p.user_id ASC, p.chat_id ASC
                """,
                params,
            )
        )

    def get_broadcast_replacement_settings(self, now: datetime | None = None) -> sqlite3.Row:
        row = self.conn.execute("SELECT * FROM broadcast_replacement_settings WHERE id = 1").fetchone()
        if row is not None:
            return row
        timestamp = (now or datetime.now()).isoformat()
        self.conn.execute(
            """
            INSERT INTO broadcast_replacement_settings(id, enabled, text, photo, updated_at)
            VALUES (1, 0, NULL, NULL, ?)
            """,
            (timestamp,),
        )
        self.conn.commit()
        return self.conn.execute("SELECT * FROM broadcast_replacement_settings WHERE id = 1").fetchone()

    def update_broadcast_replacement_settings(
        self,
        *,
        now: datetime,
        enabled: int | None = None,
        text: str | None = None,
        photo: str | None = None,
        updated_by: int | None = None,
        clear_text: bool = False,
        clear_photo: bool = False,
    ) -> sqlite3.Row:
        self.get_broadcast_replacement_settings(now)
        fields: dict[str, Any] = {"updated_at": now.isoformat()}
        if enabled is not None:
            fields["enabled"] = enabled
        if text is not None or clear_text:
            fields["text"] = text
        if photo is not None or clear_photo:
            fields["photo"] = photo
        if updated_by is not None:
            fields["updated_by"] = updated_by
        columns = ", ".join(f"{name} = ?" for name in fields)
        values = list(fields.values())
        self.conn.execute(f"UPDATE broadcast_replacement_settings SET {columns} WHERE id = 1", values)
        self.conn.commit()
        return self.get_broadcast_replacement_settings(now)

    def _add_missing_columns(self, table: str, columns: dict[str, str]) -> None:
        existing = {row["name"] for row in self.conn.execute(f"PRAGMA table_info({table})")}
        for name, definition in columns.items():
            if name not in existing:
                self.conn.execute(f"ALTER TABLE {table} ADD COLUMN {name} {definition}")

    def ensure_group(self, chat_id: int, title: str | None, now: datetime) -> sqlite3.Row:
        now_iso = now.isoformat()
        self.conn.execute(
            """
            INSERT INTO groups(chat_id, chat_title, created_at, updated_at)
            VALUES (?, ?, ?, ?)
            ON CONFLICT(chat_id) DO UPDATE SET
                chat_title = excluded.chat_title,
                updated_at = excluded.updated_at
            """,
            (chat_id, title, now_iso, now_iso),
        )
        self.conn.commit()
        return self.get_group(chat_id)

    def get_group(self, chat_id: int) -> sqlite3.Row:
        row = self.conn.execute("SELECT * FROM groups WHERE chat_id = ?", (chat_id,)).fetchone()
        if row is None:
            raise KeyError(f"Group {chat_id} is missing")
        return row

    def update_group(self, chat_id: int, now: datetime, **fields: Any) -> sqlite3.Row:
        if not fields:
            return self.get_group(chat_id)
        fields["updated_at"] = now.isoformat()
        columns = ", ".join(f"{name} = ?" for name in fields)
        values = list(fields.values())
        values.append(chat_id)
        self.conn.execute(f"UPDATE groups SET {columns} WHERE chat_id = ?", values)
        self.conn.commit()
        return self.get_group(chat_id)

    def apply_due_day_cutoff(self, chat_id: int, now: datetime, timezone: tzinfo) -> sqlite3.Row:
        group = self.get_group(chat_id)
        pending = group_value(group, "pending_day_cutoff_hour")
        effective_at = parse_stored_datetime(group_value(group, "pending_day_cutoff_effective_at"), timezone)
        if pending is None or effective_at is None or now.astimezone(timezone) < effective_at:
            return group
        return self.update_group(
            chat_id,
            now,
            day_cutoff_hour=int(pending),
            pending_day_cutoff_hour=None,
            pending_day_cutoff_effective_at=None,
        )

    def set_day_cutoff_hour(self, chat_id: int, now: datetime, new_hour: int, timezone: tzinfo) -> sqlite3.Row:
        group = self.apply_due_day_cutoff(chat_id, now, timezone)
        old_hour = int(group["day_cutoff_hour"])
        if new_hour == old_hour:
            return self.update_group(
                chat_id,
                now,
                pending_day_cutoff_hour=None,
                pending_day_cutoff_effective_at=None,
                open_bill_day_key=None,
                open_bill_begin_at=None,
                open_bill_end_at=None,
            )
        if new_hour < 0 or old_hour < 0:
            return self.update_group(
                chat_id,
                now,
                day_cutoff_hour=new_hour,
                pending_day_cutoff_hour=None,
                pending_day_cutoff_effective_at=None,
                open_bill_day_key=None,
                open_bill_begin_at=None,
                open_bill_end_at=None,
            )

        current_key, current_start, old_end = current_bill_window_for_change(group, now, timezone)
        if not self.period_has_records(chat_id, current_start, old_end):
            return self.update_group(
                chat_id,
                now,
                day_cutoff_hour=new_hour,
                pending_day_cutoff_hour=None,
                pending_day_cutoff_effective_at=None,
                open_bill_day_key=None,
                open_bill_begin_at=None,
                open_bill_end_at=None,
            )

        effective_at = first_cutoff_boundary_at_or_after(old_end, new_hour, timezone)
        return self.update_group(
            chat_id,
            now,
            pending_day_cutoff_hour=new_hour,
            pending_day_cutoff_effective_at=effective_at.isoformat(),
            open_bill_day_key=current_key,
            open_bill_begin_at=current_start.isoformat(),
            open_bill_end_at=effective_at.isoformat(),
        )

    def activate_group(self, chat_id: int, now: datetime) -> sqlite3.Row:
        group = self.get_group(chat_id)
        updates: dict[str, Any] = {"active": 1, "activated_at": now.isoformat()}
        if not group["trial_started_at"]:
            updates["trial_started_at"] = now.isoformat()
            updates["trial_until"] = (now + timedelta(hours=12)).isoformat()
        return self.update_group(chat_id, now, **updates)

    def touch_user(self, chat_id: int, user: TelegramUser, now: datetime) -> None:
        self.conn.execute(
            """
            INSERT INTO users(user_id, username, display_name, last_seen_at)
            VALUES (?, ?, ?, ?)
            ON CONFLICT(user_id) DO UPDATE SET
                username = excluded.username,
                display_name = excluded.display_name,
                last_seen_at = excluded.last_seen_at
            """,
            (user.user_id, user.username, user.display_name, now.isoformat()),
        )
        self.conn.execute(
            """
            INSERT INTO group_members(chat_id, user_id, username, display_name, last_seen_at)
            VALUES (?, ?, ?, ?, ?)
            ON CONFLICT(chat_id, user_id) DO UPDATE SET
                username = excluded.username,
                display_name = excluded.display_name,
                last_seen_at = excluded.last_seen_at
            """,
            (chat_id, user.user_id, user.username, user.display_name, now.isoformat()),
        )
        self.conn.commit()

    def add_operator(
        self,
        chat_id: int,
        user: TelegramUser,
        *,
        added_by: int,
        role: str,
        now: datetime,
    ) -> None:
        if user.user_id:
            self.conn.execute("DELETE FROM operators WHERE chat_id = ? AND user_id = ?", (chat_id, user.user_id))
        if user.username_norm:
            self.conn.execute(
                "DELETE FROM operators WHERE chat_id = ? AND username_norm = ?",
                (chat_id, user.username_norm),
            )
        self.conn.execute(
            """
            INSERT INTO operators(chat_id, user_id, username_norm, username, display_name, role, added_by, created_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?)
            """,
            (
                chat_id,
                user.user_id or None,
                user.username_norm,
                user.username,
                user.display_name,
                role,
                added_by,
                now.isoformat(),
            ),
        )
        self.conn.commit()

    def add_operator_by_username(self, chat_id: int, username: str, *, added_by: int, now: datetime) -> None:
        username_norm = username.lower().lstrip("@")
        self.conn.execute(
            "DELETE FROM operators WHERE chat_id = ? AND username_norm = ?",
            (chat_id, username_norm),
        )
        self.conn.execute(
            """
            INSERT INTO operators(chat_id, username_norm, username, display_name, role, added_by, created_at)
            VALUES (?, ?, ?, ?, 'operator', ?, ?)
            """,
            (chat_id, username_norm, username_norm, f"@{username_norm}", added_by, now.isoformat()),
        )
        self.conn.commit()

    def set_group_owner(self, chat_id: int, user: TelegramUser, *, now: datetime) -> None:
        self.conn.execute("DELETE FROM operators WHERE chat_id = ? AND role = 'owner'", (chat_id,))
        self.add_operator(chat_id, user, added_by=user.user_id, role="owner", now=now)
        self.update_group(chat_id, now, owner_user_id=user.user_id)

    def remove_operator(
        self,
        chat_id: int,
        *,
        user_id: int | None = None,
        username_norm: str | None = None,
    ) -> int:
        if user_id is not None:
            cursor = self.conn.execute(
                "DELETE FROM operators WHERE chat_id = ? AND user_id = ? AND role <> 'owner'",
                (chat_id, user_id),
            )
        elif username_norm:
            cursor = self.conn.execute(
                "DELETE FROM operators WHERE chat_id = ? AND username_norm = ? AND role <> 'owner'",
                (chat_id, username_norm.lower().lstrip("@")),
            )
        else:
            return 0
        self.conn.commit()
        return cursor.rowcount

    def is_owner(self, chat_id: int, user_id: int) -> bool:
        group = self.get_group(chat_id)
        return group["owner_user_id"] == user_id

    def is_operator(self, chat_id: int, user: TelegramUser) -> bool:
        row = self.conn.execute(
            """
            SELECT 1 FROM operators
            WHERE chat_id = ?
              AND ((user_id IS NOT NULL AND user_id = ?)
                   OR (username_norm IS NOT NULL AND username_norm = ?))
            LIMIT 1
            """,
            (chat_id, user.user_id, user.username_norm),
        ).fetchone()
        return row is not None

    def list_operators(self, chat_id: int) -> list[sqlite3.Row]:
        return list(
            self.conn.execute(
                "SELECT * FROM operators WHERE chat_id = ? ORDER BY role DESC, id ASC",
                (chat_id,),
            )
        )

    def insert_record(self, values: dict[str, Any]) -> sqlite3.Row:
        columns = list(values.keys())
        placeholders = ", ".join("?" for _ in columns)
        cursor = self.conn.execute(
            f"INSERT INTO records({', '.join(columns)}) VALUES ({placeholders})",
            [values[column] for column in columns],
        )
        self.conn.commit()
        return self.get_record(cursor.lastrowid)

    def get_record(self, record_id: int) -> sqlite3.Row:
        row = self.conn.execute("SELECT * FROM records WHERE id = ?", (record_id,)).fetchone()
        if row is None:
            raise KeyError(f"Record {record_id} is missing")
        return row

    def set_record_bot_message(self, record_id: int, bot_message_id: int) -> None:
        self.conn.execute("UPDATE records SET bot_message_id = ? WHERE id = ?", (bot_message_id, record_id))
        self.conn.commit()

    def list_records_for_day(self, chat_id: int, day_key: str, *, all_days: bool = False) -> list[sqlite3.Row]:
        if all_days:
            return list(
                self.conn.execute(
                    """
                    SELECT * FROM records
                    WHERE chat_id = ?
                      AND deleted_at IS NULL
                    ORDER BY id ASC
                    """,
                    (chat_id,),
                )
            )
        return list(
            self.conn.execute(
                """
                SELECT * FROM records
                WHERE chat_id = ?
                  AND deleted_at IS NULL
                  AND (day_key = ? OR is_balance = 1)
                ORDER BY id ASC
                """,
                (chat_id, day_key),
            )
        )

    def list_records_for_period(self, chat_id: int, start: datetime, end: datetime) -> list[sqlite3.Row]:
        return list(
            self.conn.execute(
                """
                SELECT * FROM records
                WHERE chat_id = ?
                  AND deleted_at IS NULL
                  AND ((created_at >= ? AND created_at < ?) OR is_balance = 1)
                ORDER BY id ASC
                """,
                (chat_id, start.isoformat(), end.isoformat()),
            )
        )

    def period_has_records(self, chat_id: int, start: datetime, end: datetime) -> bool:
        row = self.conn.execute(
            """
            SELECT 1 FROM records
            WHERE chat_id = ?
              AND deleted_at IS NULL
              AND is_balance = 0
              AND created_at >= ?
              AND created_at < ?
            LIMIT 1
            """,
            (chat_id, start.isoformat(), end.isoformat()),
        ).fetchone()
        return row is not None

    def soft_delete_record(self, chat_id: int, record_id: int, now: datetime, kind: str | None = None) -> int:
        if kind:
            cursor = self.conn.execute(
                """
                UPDATE records SET deleted_at = ?
                WHERE chat_id = ? AND id = ? AND kind = ? AND deleted_at IS NULL
                """,
                (now.isoformat(), chat_id, record_id, kind),
            )
        else:
            cursor = self.conn.execute(
                """
                UPDATE records SET deleted_at = ?
                WHERE chat_id = ? AND id = ? AND deleted_at IS NULL
                """,
                (now.isoformat(), chat_id, record_id),
            )
        self.conn.commit()
        return cursor.rowcount

    def soft_delete_last_kind(self, chat_id: int, kind: str, now: datetime) -> sqlite3.Row | None:
        row = self.conn.execute(
            """
            SELECT * FROM records
            WHERE chat_id = ? AND kind = ? AND deleted_at IS NULL
            ORDER BY id DESC
            LIMIT 1
            """,
            (chat_id, kind),
        ).fetchone()
        if row is None:
            return None
        self.soft_delete_record(chat_id, row["id"], now, kind=kind)
        return row

    def soft_delete_recent_kind(self, chat_id: int, kind: str, count: int, now: datetime) -> list[sqlite3.Row]:
        rows = list(
            self.conn.execute(
                """
                SELECT * FROM records
                WHERE chat_id = ? AND kind = ? AND deleted_at IS NULL
                ORDER BY id DESC
                LIMIT ?
                """,
                (chat_id, kind, count),
            )
        )
        for row in rows:
            self.soft_delete_record(chat_id, row["id"], now, kind=kind)
        return rows

    def soft_delete_day(self, chat_id: int, day_key: str, now: datetime, *, all_days: bool = False) -> int:
        if all_days:
            cursor = self.conn.execute(
                """
                UPDATE records SET deleted_at = ?
                WHERE chat_id = ? AND deleted_at IS NULL AND is_balance = 0
                """,
                (now.isoformat(), chat_id),
            )
        else:
            cursor = self.conn.execute(
                """
                UPDATE records SET deleted_at = ?
                WHERE chat_id = ? AND day_key = ? AND deleted_at IS NULL AND is_balance = 0
                """,
                (now.isoformat(), chat_id, day_key),
            )
        self.conn.commit()
        return cursor.rowcount

    def soft_delete_period(self, chat_id: int, start: datetime, end: datetime, now: datetime) -> int:
        cursor = self.conn.execute(
            """
            UPDATE records SET deleted_at = ?
            WHERE chat_id = ?
              AND deleted_at IS NULL
              AND is_balance = 0
              AND created_at >= ?
              AND created_at < ?
            """,
            (now.isoformat(), chat_id, start.isoformat(), end.isoformat()),
        )
        self.conn.commit()
        return cursor.rowcount

    def soft_delete_all(self, chat_id: int, now: datetime) -> int:
        cursor = self.conn.execute(
            "UPDATE records SET deleted_at = ? WHERE chat_id = ? AND deleted_at IS NULL",
            (now.isoformat(), chat_id),
        )
        self.conn.commit()
        return cursor.rowcount

    def recent_members(self, chat_id: int, limit: int = 200) -> list[sqlite3.Row]:
        return list(
            self.conn.execute(
                """
                SELECT * FROM group_members
                WHERE chat_id = ?
                ORDER BY last_seen_at DESC
                LIMIT ?
                """,
                (chat_id, limit),
            )
        )

    def update_day_exchange_rate(
        self,
        chat_id: int,
        day_key: str,
        exchange_rate: str,
        now: datetime,
        *,
        all_days: bool = False,
    ) -> int:
        if all_days:
            rows = list(
                self.conn.execute(
                    """
                    SELECT * FROM records
                    WHERE chat_id = ? AND deleted_at IS NULL
                    """,
                    (chat_id,),
                )
            )
        else:
            rows = list(
                self.conn.execute(
                    """
                    SELECT * FROM records
                    WHERE chat_id = ? AND day_key = ? AND deleted_at IS NULL
                    """,
                    (chat_id, day_key),
                )
            )
        changed = 0
        for row in rows:
            values = _recalculate_record(row, exchange_rate)
            self.conn.execute(
                """
                UPDATE records
                SET exchange_rate = ?,
                    amount_cny = ?,
                    amount_usdt = ?,
                    commission_cny = ?,
                    net_usdt = ?
                WHERE id = ?
                """,
                (
                    values["exchange_rate"],
                    values["amount_cny"],
                    values["amount_usdt"],
                    values["commission_cny"],
                    values["net_usdt"],
                    row["id"],
                ),
            )
            changed += 1
        self.conn.commit()
        return changed

    def update_period_exchange_rate(
        self,
        chat_id: int,
        start: datetime,
        end: datetime,
        exchange_rate: str,
        now: datetime,
    ) -> int:
        rows = list(
            self.conn.execute(
                """
                SELECT * FROM records
                WHERE chat_id = ?
                  AND deleted_at IS NULL
                  AND created_at >= ?
                  AND created_at < ?
                """,
                (chat_id, start.isoformat(), end.isoformat()),
            )
        )
        changed = 0
        for row in rows:
            values = _recalculate_record(row, exchange_rate)
            self.conn.execute(
                """
                UPDATE records
                SET exchange_rate = ?,
                    amount_cny = ?,
                    amount_usdt = ?,
                    commission_cny = ?,
                    net_usdt = ?
                WHERE id = ?
                """,
                (
                    values["exchange_rate"],
                    values["amount_cny"],
                    values["amount_usdt"],
                    values["commission_cny"],
                    values["net_usdt"],
                    row["id"],
                ),
            )
            changed += 1
        self.conn.commit()
        return changed

    def delete_group_data(self, chat_id: int) -> None:
        self.conn.execute("DELETE FROM records WHERE chat_id = ?", (chat_id,))
        self.conn.execute("DELETE FROM operators WHERE chat_id = ?", (chat_id,))
        self.conn.execute("DELETE FROM group_members WHERE chat_id = ?", (chat_id,))
        self.conn.execute("DELETE FROM broadcast_group_members WHERE chat_id = ?", (chat_id,))
        self.conn.execute("DELETE FROM broadcast_chat_permissions WHERE chat_id = ?", (chat_id,))
        self.conn.execute("DELETE FROM address_whitelist WHERE chat_id = ?", (chat_id,))
        self.conn.execute("DELETE FROM groups WHERE chat_id = ?", (chat_id,))
        self.conn.commit()

    def forget_group(self, chat_id: int) -> None:
        self.conn.execute("DELETE FROM operators WHERE chat_id = ?", (chat_id,))
        self.conn.execute("DELETE FROM group_members WHERE chat_id = ?", (chat_id,))
        self.conn.execute("DELETE FROM broadcast_group_members WHERE chat_id = ?", (chat_id,))
        self.conn.execute("DELETE FROM broadcast_chat_permissions WHERE chat_id = ?", (chat_id,))
        self.conn.execute("DELETE FROM address_whitelist WHERE chat_id = ?", (chat_id,))
        self.conn.execute("DELETE FROM groups WHERE chat_id = ?", (chat_id,))
        self.conn.commit()

    def add_address_whitelist(
        self,
        *,
        chat_id: int,
        network: str,
        address: str,
        label: str | None,
        image_url: str | None,
        created_by: int | None,
        now: datetime,
    ) -> sqlite3.Row:
        self.conn.execute(
            """
            INSERT INTO address_whitelist(
                chat_id,
                network,
                address,
                label,
                image_url,
                enabled,
                created_by,
                created_at,
                updated_at
            )
            VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?)
            ON CONFLICT(chat_id, address) DO UPDATE SET
                network = excluded.network,
                label = excluded.label,
                image_url = excluded.image_url,
                enabled = 1,
                updated_at = excluded.updated_at
            """,
            (
                chat_id,
                network,
                address,
                label,
                image_url,
                created_by,
                now.isoformat(),
                now.isoformat(),
            ),
        )
        self.conn.commit()
        row = self.get_address_whitelist(chat_id, address, enabled_only=False)
        if row is None:
            raise KeyError(f"Address whitelist row is missing: {chat_id} {address}")
        return row

    def remove_address_whitelist(self, chat_id: int, address: str, *, now: datetime | None = None) -> int:
        cursor = self.conn.execute(
            """
            UPDATE address_whitelist
            SET enabled = 0,
                updated_at = COALESCE(?, updated_at)
            WHERE chat_id = ? AND address = ? AND enabled = 1
            """,
            (now.isoformat() if now else None, chat_id, address),
        )
        self.conn.commit()
        return cursor.rowcount

    def get_address_whitelist(
        self,
        chat_id: int,
        address: str,
        *,
        enabled_only: bool = True,
    ) -> sqlite3.Row | None:
        enabled_clause = "AND enabled = 1" if enabled_only else ""
        return self.conn.execute(
            f"""
            SELECT * FROM address_whitelist
            WHERE chat_id = ? AND address = ?
            {enabled_clause}
            """,
            (chat_id, address),
        ).fetchone()

    def list_address_whitelist(
        self,
        chat_id: int | None = None,
        *,
        enabled_only: bool = False,
    ) -> list[sqlite3.Row]:
        where: list[str] = []
        params: list[Any] = []
        if chat_id is not None:
            where.append("w.chat_id = ?")
            params.append(chat_id)
        if enabled_only:
            where.append("w.enabled = 1")
        where_sql = "WHERE " + " AND ".join(where) if where else ""
        return list(
            self.conn.execute(
                f"""
                SELECT w.*, g.chat_title
                FROM address_whitelist w
                LEFT JOIN groups g ON g.chat_id = w.chat_id
                {where_sql}
                ORDER BY w.updated_at DESC, w.id DESC
                """,
                params,
            )
        )

    def add_address_watch(
        self,
        owner_user_id: int,
        network: str,
        address: str,
        label: str | None,
        now: datetime,
    ) -> None:
        self.conn.execute(
            """
            INSERT INTO address_watches(owner_user_id, network, address, label, active, created_at, updated_at)
            VALUES (?, ?, ?, ?, 1, ?, ?)
            ON CONFLICT(owner_user_id, address) DO UPDATE SET
                network = excluded.network,
                label = excluded.label,
                active = 1,
                updated_at = excluded.updated_at
            """,
            (owner_user_id, network, address, label, now.isoformat(), now.isoformat()),
        )
        self.conn.commit()

    def remove_address_watch(self, owner_user_id: int, address: str) -> int:
        cursor = self.conn.execute(
            """
            UPDATE address_watches
            SET active = 0
            WHERE owner_user_id = ? AND address = ? AND active = 1
            """,
            (owner_user_id, address),
        )
        self.conn.commit()
        return cursor.rowcount

    def update_address_watch_label(self, owner_user_id: int, address: str, label: str | None, now: datetime) -> int:
        cursor = self.conn.execute(
            """
            UPDATE address_watches
            SET label = ?, updated_at = ?
            WHERE owner_user_id = ? AND address = ? AND active = 1
            """,
            (label, now.isoformat(), owner_user_id, address),
        )
        self.conn.commit()
        return cursor.rowcount

    def list_address_watches(self, owner_user_id: int) -> list[sqlite3.Row]:
        return list(
            self.conn.execute(
                """
                SELECT * FROM address_watches
                WHERE owner_user_id = ? AND active = 1
                ORDER BY id DESC
                """,
                (owner_user_id,),
            )
        )

    def list_active_address_watch_targets(self) -> list[sqlite3.Row]:
        return list(
            self.conn.execute(
                """
                SELECT
                    w.*,
                    s.watch_income,
                    s.watch_expense,
                    s.notify_trx,
                    s.display_mode,
                    s.min_notify_amount
                FROM address_watches w
                LEFT JOIN address_watch_settings s ON s.owner_user_id = w.owner_user_id
                WHERE w.active = 1
                ORDER BY w.id ASC
                """
            )
        )

    def record_chain_event_notification(
        self,
        *,
        owner_user_id: int,
        address: str,
        tx_hash: str,
        direction: str,
        token_symbol: str,
        block_timestamp: int,
        now: datetime,
    ) -> bool:
        cursor = self.conn.execute(
            """
            INSERT OR IGNORE INTO chain_event_notifications(
                owner_user_id,
                address,
                tx_hash,
                direction,
                token_symbol,
                block_timestamp,
                created_at
            )
            VALUES (?, ?, ?, ?, ?, ?, ?)
            """,
            (
                owner_user_id,
                address,
                tx_hash,
                direction,
                token_symbol,
                block_timestamp,
                now.isoformat(),
            ),
        )
        self.conn.commit()
        return cursor.rowcount == 1

    def get_address_watch_settings(self, owner_user_id: int, now: datetime) -> sqlite3.Row:
        row = self.conn.execute(
            "SELECT * FROM address_watch_settings WHERE owner_user_id = ?",
            (owner_user_id,),
        ).fetchone()
        if row is not None:
            return row
        self.conn.execute(
            """
            INSERT INTO address_watch_settings(owner_user_id, created_at, updated_at)
            VALUES (?, ?, ?)
            """,
            (owner_user_id, now.isoformat(), now.isoformat()),
        )
        self.conn.commit()
        return self.conn.execute(
            "SELECT * FROM address_watch_settings WHERE owner_user_id = ?",
            (owner_user_id,),
        ).fetchone()

    def update_address_watch_settings(self, owner_user_id: int, now: datetime, **fields: Any) -> sqlite3.Row:
        self.get_address_watch_settings(owner_user_id, now)
        if not fields:
            return self.get_address_watch_settings(owner_user_id, now)
        fields["updated_at"] = now.isoformat()
        columns = ", ".join(f"{name} = ?" for name in fields)
        values = list(fields.values())
        values.append(owner_user_id)
        self.conn.execute(f"UPDATE address_watch_settings SET {columns} WHERE owner_user_id = ?", values)
        self.conn.commit()
        return self.get_address_watch_settings(owner_user_id, now)


class ThreadLocalStorage:
    def __init__(self, path: Path):
        self.path = path
        self.local = threading.local()

    def current(self) -> Storage:
        storage = getattr(self.local, "storage", None)
        if storage is None:
            storage = Storage(self.path)
            self.local.storage = storage
        return storage

    def __getattr__(self, name: str) -> Any:
        return getattr(self.current(), name)


def user_from_telegram(raw: dict[str, Any]) -> TelegramUser:
    username = raw.get("username")
    parts = [raw.get("first_name"), raw.get("last_name")]
    display_name = " ".join(part for part in parts if part).strip()
    if not display_name:
        display_name = f"@{username}" if username else str(raw["id"])
    return TelegramUser(user_id=int(raw["id"]), username=username, display_name=display_name)


def group_value(group: Any, key: str, default: Any = None) -> Any:
    if isinstance(group, dict):
        return group.get(key, default)
    try:
        return group[key]
    except (IndexError, KeyError, TypeError):
        return default


def parse_stored_datetime(value: Any, timezone: tzinfo) -> datetime | None:
    if not value:
        return None
    parsed = datetime.fromisoformat(str(value))
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone)
    return parsed.astimezone(timezone)


def current_business_day_key(now: datetime, group: Any, timezone: tzinfo) -> str:
    override = active_open_bill_window(now, group, timezone)
    if override is not None:
        return override[0]
    return business_day_key(now, int(group_value(group, "day_cutoff_hour", 0)), timezone)


def active_open_bill_window(now: datetime, group: Any, timezone: tzinfo) -> tuple[str, datetime, datetime] | None:
    key = group_value(group, "open_bill_day_key")
    begin = parse_stored_datetime(group_value(group, "open_bill_begin_at"), timezone)
    end = parse_stored_datetime(group_value(group, "open_bill_end_at"), timezone)
    if not key or begin is None or end is None:
        return None
    local_now = now.astimezone(timezone)
    if begin <= local_now < end:
        return str(key), begin, end
    return None


def current_bill_window_for_change(group: Any, now: datetime, timezone: tzinfo) -> tuple[str, datetime, datetime]:
    override = active_open_bill_window(now, group, timezone)
    if override is not None:
        return override
    cutoff_hour = int(group_value(group, "day_cutoff_hour", 0))
    key = business_day_key(now, cutoff_hour, timezone)
    begin, end = business_day_range(key, cutoff_hour, timezone)
    return key, begin, end


def bill_window_for_day(group: Any, day_key: str, timezone: tzinfo, cutoff_hour: int | None = None) -> tuple[datetime, datetime]:
    open_key = group_value(group, "open_bill_day_key")
    begin = parse_stored_datetime(group_value(group, "open_bill_begin_at"), timezone)
    end = parse_stored_datetime(group_value(group, "open_bill_end_at"), timezone)
    if open_key == day_key and begin is not None and end is not None:
        return begin, end
    hour = int(group_value(group, "day_cutoff_hour", 0) if cutoff_hour is None else cutoff_hour)
    return business_day_range(day_key, hour, timezone)


def first_cutoff_boundary_at_or_after(moment: datetime, cutoff_hour: int, timezone: tzinfo) -> datetime:
    local = moment.astimezone(timezone)
    candidate = datetime(local.year, local.month, local.day, cutoff_hour, 0, 0, tzinfo=timezone)
    if candidate < local:
        candidate += timedelta(days=1)
    return candidate


def business_day_key(now: datetime, cutoff_hour: int, timezone: tzinfo) -> str:
    local = now.astimezone(timezone)
    if cutoff_hour >= 0 and local.hour < cutoff_hour:
        local = local - timedelta(days=1)
    return local.date().isoformat()


def business_day_range(day_key: str, cutoff_hour: int, timezone: tzinfo) -> tuple[datetime, datetime]:
    year, month, day = [int(part) for part in day_key.split("-")]
    if cutoff_hour < 0:
        cutoff_hour = 0
    start = datetime(year, month, day, cutoff_hour, 0, 0, tzinfo=timezone)
    return start, start + timedelta(days=1)


def _recalculate_record(row: sqlite3.Row, exchange_rate: str) -> dict[str, str]:
    from decimal import Decimal, ROUND_HALF_UP

    amount = Decimal(row["amount"])
    old_amount_cny = Decimal(row["amount_cny"])
    old_amount_usdt = Decimal(row["amount_usdt"])
    rate = Decimal(exchange_rate)
    fee_rate = Decimal(row["fee_rate"])
    if row["currency"] == "USDT":
        amount_usdt = amount
        amount_cny = amount_usdt * rate
    elif old_amount_usdt != 0 and abs(old_amount_cny / old_amount_usdt - Decimal(row["exchange_rate"])) < Decimal("0.0001"):
        amount_cny = amount
        amount_usdt = amount_cny / rate
    else:
        amount_cny = old_amount_cny
        amount_usdt = amount_cny / rate
    commission_cny = amount_cny * fee_rate / Decimal("100")
    net_usdt = (amount_cny - commission_cny) / rate if rate != 0 else Decimal("0")
    quant = Decimal("0.000001")
    return {
        "exchange_rate": str(rate),
        "amount_cny": str(amount_cny.quantize(quant, rounding=ROUND_HALF_UP)),
        "amount_usdt": str(amount_usdt.quantize(quant, rounding=ROUND_HALF_UP)),
        "commission_cny": str(commission_cny.quantize(quant, rounding=ROUND_HALF_UP)),
        "net_usdt": str(net_usdt.quantize(quant, rounding=ROUND_HALF_UP)),
    }
