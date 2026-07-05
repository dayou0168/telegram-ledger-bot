from __future__ import annotations

import sqlite3
from dataclasses import dataclass
from datetime import datetime, timedelta
import json
from pathlib import Path
from typing import Any
from zoneinfo import ZoneInfo


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
        self.conn = sqlite3.connect(path)
        self.conn.row_factory = sqlite3.Row
        self.conn.execute("PRAGMA foreign_keys = ON")
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
                pin_enabled INTEGER NOT NULL DEFAULT 0,
                realtime_rate INTEGER NOT NULL DEFAULT 0,
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

            CREATE TABLE IF NOT EXISTS broadcast_jobs (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                creator_user_id INTEGER NOT NULL,
                scope TEXT NOT NULL,
                target_chat_ids TEXT NOT NULL,
                text TEXT NOT NULL,
                source_chat_id INTEGER,
                source_message_id INTEGER,
                message_kind TEXT NOT NULL DEFAULT 'text',
                notify_all INTEGER NOT NULL DEFAULT 0,
                status TEXT NOT NULL DEFAULT 'pending',
                success_count INTEGER NOT NULL DEFAULT 0,
                failure_count INTEGER NOT NULL DEFAULT 0,
                created_at TEXT NOT NULL,
                confirmed_at TEXT,
                completed_at TEXT,
                updated_at TEXT
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
            "broadcast_jobs",
            {
                "source_chat_id": "INTEGER",
                "source_message_id": "INTEGER",
                "message_kind": "TEXT NOT NULL DEFAULT 'text'",
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
                notify_all,
                created_at,
                updated_at
            )
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            (
                creator_user_id,
                scope,
                json.dumps(target_chat_ids),
                text,
                source_chat_id,
                source_message_id,
                message_kind,
                1 if notify_all else 0,
                now.isoformat(),
                now.isoformat(),
            ),
        )
        self.conn.commit()
        return self.get_broadcast_job(cursor.lastrowid)

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

    def delete_named_broadcast_group(self, name: str) -> int:
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

    def delete_group_data(self, chat_id: int) -> None:
        self.conn.execute("DELETE FROM records WHERE chat_id = ?", (chat_id,))
        self.conn.execute("DELETE FROM operators WHERE chat_id = ?", (chat_id,))
        self.conn.execute("DELETE FROM group_members WHERE chat_id = ?", (chat_id,))
        self.conn.execute("DELETE FROM broadcast_group_members WHERE chat_id = ?", (chat_id,))
        self.conn.execute("DELETE FROM groups WHERE chat_id = ?", (chat_id,))
        self.conn.commit()

    def forget_group(self, chat_id: int) -> None:
        self.conn.execute("DELETE FROM operators WHERE chat_id = ?", (chat_id,))
        self.conn.execute("DELETE FROM group_members WHERE chat_id = ?", (chat_id,))
        self.conn.execute("DELETE FROM broadcast_group_members WHERE chat_id = ?", (chat_id,))
        self.conn.execute("DELETE FROM groups WHERE chat_id = ?", (chat_id,))
        self.conn.commit()

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


def user_from_telegram(raw: dict[str, Any]) -> TelegramUser:
    username = raw.get("username")
    parts = [raw.get("first_name"), raw.get("last_name")]
    display_name = " ".join(part for part in parts if part).strip()
    if not display_name:
        display_name = f"@{username}" if username else str(raw["id"])
    return TelegramUser(user_id=int(raw["id"]), username=username, display_name=display_name)


def business_day_key(now: datetime, cutoff_hour: int, timezone: ZoneInfo) -> str:
    local = now.astimezone(timezone)
    if cutoff_hour >= 0 and local.hour < cutoff_hour:
        local = local - timedelta(days=1)
    return local.date().isoformat()


def business_day_range(day_key: str, cutoff_hour: int, timezone: ZoneInfo) -> tuple[datetime, datetime]:
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
