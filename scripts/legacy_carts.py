#!/usr/bin/env python3

import argparse
import csv
import hashlib
import io
import json
import os
import re
import subprocess
import sys
from pathlib import Path
from tempfile import NamedTemporaryFile
from uuid import UUID


def uuid_text(value):
    if not isinstance(value, str):
        raise ValueError("invalid UUID in snapshot")
    try:
        parsed = UUID(value)
    except ValueError:
        raise ValueError("invalid UUID in snapshot") from None
    if not parsed.int:
        raise ValueError("invalid UUID in snapshot")
    return str(parsed)


def read_record(key, order):
    if not isinstance(key, str) or not isinstance(order, dict):
        raise ValueError("invalid cart snapshot record")
    person_id = uuid_text(order.get("personId"))
    if key != f"person:{person_id}:order":
        raise ValueError("Redis cart key does not match its owner")
    status = order.get("status")
    if status not in ("NotChecked", "Checked", "Processing", "InDelivery", "Delivered", "Completed", "Cancelled"):
        raise ValueError("invalid cart status in snapshot")
    promo_id = order.get("promoCodeId")
    products = order.get("products")
    if not isinstance(products, list) or len(products) > 100:
        raise ValueError("invalid number of products in cart snapshot")
    quantities = {}
    for line in products:
        if not isinstance(line, dict) or not isinstance(line.get("variant"), dict):
            raise ValueError("invalid product in cart snapshot")
        variant_id = uuid_text(line["variant"].get("id"))
        quantity = line.get("quantity")
        if type(quantity) is not int or not 1 <= quantity <= 99:
            raise ValueError("invalid quantity in cart snapshot")
        quantities[variant_id] = quantities.get(variant_id, 0) + quantity
        if quantities[variant_id] > 99:
            raise ValueError("invalid total quantity in cart snapshot")
    return {
        "key": key,
        "order": {
            "id": uuid_text(order.get("id")),
            "personId": person_id,
            "status": status,
            "promoCodeId": uuid_text(promo_id) if promo_id is not None else None,
            "products": [
                {"variant": {"id": variant_id}, "quantity": quantity}
                for variant_id, quantity in sorted(quantities.items())
            ],
        },
    }


def redis_output(args, command):
    result = subprocess.run(
        ["redis-cli", "-e", *args, *command], capture_output=True, text=True, timeout=30
    )
    if result.returncode:
        raise ValueError("Redis command failed")
    return result.stdout


def save_snapshot(path, redis_args):
    keys = set(redis_output(redis_args, ["--raw", "--scan", "--pattern", "person:*:order"]).splitlines())
    digest = hashlib.sha256()
    count = 0
    with NamedTemporaryFile(dir=path.parent, prefix=".legacy-carts-") as snapshot:
        for key in sorted(keys):
            person_id = key.removeprefix("person:").removesuffix(":order")
            if key != f"person:{uuid_text(person_id)}:order":
                raise ValueError("invalid Redis cart key")
            cached = json.loads(redis_output(redis_args, ["--json", "GET", key]))
            if cached is None:
                raise ValueError("a Redis cart disappeared during snapshot export")
            if not isinstance(cached, str):
                raise ValueError("invalid Redis cart value")
            order = json.loads(cached)
            record = read_record(key, order)
            line = json.dumps(record, separators=(",", ":")).encode() + b"\n"
            digest.update(line)
            snapshot.write(line)
            count += 1
        footer = {"count": count, "sha256": digest.hexdigest()}
        snapshot.write(json.dumps(footer).encode() + b"\n")
        snapshot.flush()
        os.fsync(snapshot.fileno())
        os.link(snapshot.name, path)
    print(f"Snapshot saved: {count} Redis carts")


def snapshot_records(path):
    lines = path.read_bytes().splitlines(keepends=True)
    if not lines:
        raise ValueError("snapshot is empty; use the snapshot command even if Redis has no carts")
    records, footer = lines[:-1], lines[-1]
    checksum = hashlib.sha256()
    for raw in records:
        checksum.update(raw)
    if json.loads(footer) != {"count": len(records), "sha256": checksum.hexdigest()}:
        raise ValueError("snapshot is incomplete or its checksum does not match")

    seen_keys = set()
    seen_orders = set()
    for line_number, raw in enumerate(records, start=1):
        try:
            row = json.loads(raw)
            if not isinstance(row, dict):
                raise ValueError("invalid cart snapshot record")
            record = read_record(row.get("key"), row.get("order"))
            if record["key"] in seen_keys:
                raise ValueError("duplicate Redis cart key")
            if record["order"]["id"] in seen_orders:
                raise ValueError("duplicate order ID in snapshot")
            seen_keys.add(record["key"])
            seen_orders.add(record["order"]["id"])
        except ValueError as error:
            message = "invalid JSON" if isinstance(error, json.JSONDecodeError) else str(error)
            raise ValueError(f"snapshot line {line_number}: {message}") from None
        yield record["order"]


def import_snapshot(path, apply_import):
    rows = io.StringIO()
    writer = csv.writer(rows, lineterminator="\n")
    for order in snapshot_records(path):
        items = [
            {"variant_id": product["variant"]["id"], "quantity": product["quantity"]}
            for product in order["products"]
        ]
        writer.writerow([order["personId"], order["id"], order["status"], order["promoCodeId"], json.dumps(items)])

    stage = """BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';
CREATE TEMP TABLE legacy_cart_snapshot (
    person_id uuid PRIMARY KEY,
    order_id uuid UNIQUE NOT NULL,
    status text NOT NULL,
    promo_code_id uuid,
    items jsonb NOT NULL
);
COPY legacy_cart_snapshot FROM STDIN WITH (FORMAT csv);
"""
    sql = Path(__file__).with_name("legacy_carts.sql").read_text()
    result = subprocess.run(
        ["psql", "-X", "-qAt", "-F", ",", "-w", "-v", "ON_ERROR_STOP=1",
         "-v", f"apply_import={'true' if apply_import else 'false'}"],
        input=stage + rows.getvalue() + "\\.\n" + sql,
        capture_output=True, text=True, timeout=60,
    )
    if result.returncode:
        for reason in (
            "snapshot order or owner does not match PostgreSQL",
            "multiple open carts for one owner",
            "snapshot candidate is not a draft",
            "snapshot references a missing variant or promo code",
        ):
            if reason in result.stderr:
                raise ValueError(reason)
        raise ValueError("PostgreSQL import failed; check database connection, permissions and schema")

    counts = result.stdout.strip()
    if not re.fullmatch(r"[0-9]+,[0-9]+,[0-9]+", counts):
        raise ValueError("unexpected PostgreSQL import summary")
    imported, preserved, skipped = map(int, counts.split(","))
    action = "Import committed" if apply_import else "Dry run"
    print(f"{action}: Redis carts={imported}, PostgreSQL-only carts={preserved}, skipped Redis snapshots={skipped}")


def main():
    parser = argparse.ArgumentParser(description="Prepare and import a Redis snapshot of legacy SkViDerm carts.")
    subcommands = parser.add_subparsers(dest="command", required=True)
    snapshot = subcommands.add_parser("snapshot")
    snapshot.add_argument("file", type=Path)
    snapshot.add_argument("redis_args", nargs=argparse.REMAINDER, help="redis-cli connection options after --")
    restore = subcommands.add_parser("import", help="use PGHOST, PGPORT, PGDATABASE, PGUSER and PGPASSFILE")
    restore.add_argument("file", type=Path)
    restore.add_argument("--apply", action="store_true", help="commit writes after pausing legacy cart writes")
    args = parser.parse_args()
    try:
        if args.command == "snapshot":
            redis_args = args.redis_args
            if redis_args[:1] == ["--"]:
                redis_args = redis_args[1:]
            save_snapshot(args.file, redis_args)
        else:
            import_snapshot(args.file, args.apply)
    except (json.JSONDecodeError, UnicodeError):
        print("Legacy cart operation failed: invalid JSON or text encoding", file=sys.stderr)
        return 1
    except subprocess.TimeoutExpired:
        print("Legacy cart operation timed out", file=sys.stderr)
        return 1
    except FileExistsError:
        print("Snapshot destination already exists", file=sys.stderr)
        return 1
    except OSError:
        print("Legacy cart operation failed: check local file and command availability", file=sys.stderr)
        return 1
    except ValueError as error:
        print(f"Legacy cart operation failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
