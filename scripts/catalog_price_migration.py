"""Explicit approved additive Go migrations for the VPS release helper."""

import hashlib
import json
import re
from datetime import datetime, timedelta, timezone
from pathlib import Path

MIGRATION = "0003_catalog_price_cents.sql"
PROOF = "catalog-price-migration.json"
APPROVALS = {
    MIGRATION: (PROOF, "catalog_price_operation", "SELECT, INSERT", "applying-catalog-price-migration"),
    "0004_articles.sql": (
        "articles-migration.json", "article", "SELECT, INSERT, UPDATE", "applying-articles-migration",
    ),
    "0005_article_media.sql": (
        "article-media-migration.json", "article", "SELECT, INSERT, UPDATE", "applying-article-media-migration",
    ),
}
SHA256 = re.compile(r"[a-f0-9]{64}\Z")
COMMIT = re.compile(r"[a-f0-9]{40}\Z")
IMAGE = re.compile(r"sha256:[a-f0-9]{64}\Z")
MIGRATION_NAME = re.compile(r"[0-9]{4}_[a-z0-9_]+\.sql\Z")
ROLE = re.compile(r"[a-z_][a-z0-9_]{0,62}\Z")
TRANSACTION_TIMEOUT_MS = 60_000
STATEMENT_TIMEOUT_MS = 55_000
START_WINDOW_SECONDS = 30


def trusted_proof(operations, migration=MIGRATION):
    if migration not in APPROVALS:
        raise ValueError("Unapproved Go migration")
    path = operations.private_go_file(
        operations.root / APPROVALS[migration][0], "Go migration proof"
    )
    proof = json.loads(path.read_text())
    fields = {
        "schema", "migration", "sql_sha256", "old_image", "new_image",
        "old_commit", "new_commit", "before", "after", "checks",
    }
    if not isinstance(proof, dict) or proof.keys() != fields:
        raise ValueError("Invalid Go migration proof")
    before, after = proof["before"], proof["after"]
    if (
        proof["schema"] != 1
        or proof["migration"] != migration
        or not isinstance(before, dict)
        or not before
        or not isinstance(after, dict)
        or migration in before
        or not isinstance(proof["sql_sha256"], str)
        or not SHA256.fullmatch(proof["sql_sha256"])
        or after != {**before, migration: proof["sql_sha256"]}
        or any(
            not isinstance(name, str) or not MIGRATION_NAME.fullmatch(name)
            or not isinstance(digest, str) or not SHA256.fullmatch(digest)
            for name, digest in before.items()
        )
        or any(
            not isinstance(proof[field], str) or not pattern.fullmatch(proof[field])
            for pattern, names in (
                (COMMIT, ("old_commit", "new_commit")),
                (IMAGE, ("old_image", "new_image")),
            )
            for field in names
        )
        or proof["old_image"] == proof["new_image"]
        or proof["old_commit"] == proof["new_commit"]
        or proof["checks"] != {
            "old_runtime": True, "new_runtime": True, "migration": True
        }
    ):
        raise ValueError("Invalid Go migration compatibility proof")
    return proof


def installed_proofs(operations):
    return [
        trusted_proof(operations, name)
        for name, (filename, *_rest) in APPROVALS.items()
        if (operations.root / filename).exists() or (operations.root / filename).is_symlink()
    ]


def recovery_files(operations):
    files = [str(operations.private_go_file(
        operations.root / "catalog_price_migration.py", "Go migration helper"
    ))]
    for proof in installed_proofs(operations):
        trusted_sql(operations, proof)
        files.extend([
            str(operations.root / APPROVALS[proof["migration"]][0]),
            str(operations.root / "go-migrations" / proof["migration"]),
        ])
    return files


def matches_release(release, proof, version):
    ledger = "before" if version == "old" else "after"
    return (
        release.get("backend_language") == "go"
        and release.get("image") == proof[version + "_image"]
        and release.get("backend_commit") == proof[version + "_commit"]
        and release.get("go_migrations") == proof[ledger]
    )


def check_release_schema(operations, release):
    actual = operations.go_migration_ledger()
    if release["go_migrations"] == actual:
        return
    for proof in installed_proofs(operations):
        if matches_release(release, proof, "old") and actual == proof["after"]:
            return
    raise ValueError("Go image is not approved for the actual schema")


def check_deployment(operations, old, candidate):
    actual = operations.go_migration_ledger()
    if old["go_migrations"] == candidate["go_migrations"] == actual:
        return
    proofs = installed_proofs(operations)
    for proof in proofs:
        if actual not in (proof["before"], proof["after"]) or not matches_release(old, proof, "old"):
            continue
        if matches_release(candidate, proof, "old") and actual == proof["after"]:
            return proof
        if matches_release(candidate, proof, "new"):
            migration_role(operations)
            trusted_sql(operations, proof)
            return proof
    if all(actual not in (proof["before"], proof["after"]) for proof in proofs):
        raise ValueError("Actual Go ledger differs from the approved migration proofs")
    raise ValueError("Go release pair differs from the approved images and ledgers")


def migration_role(operations):
    role = operations.config.get("go_role", "")
    if not isinstance(role, str) or not ROLE.fullmatch(role):
        raise ValueError("Configure the reviewed Go database role for the migration")
    return role


def trusted_sql(operations, proof):
    path = operations.private_go_file(
        operations.root / "go-migrations" / proof["migration"],
        "Go migration SQL",
    )
    data = path.read_bytes()
    if hashlib.sha256(data).hexdigest() != proof["sql_sha256"]:
        raise ValueError("Go migration SQL differs from the approved digest")
    return data.decode("utf-8")


def ledger_assertion(expected):
    literal = json.dumps(expected, sort_keys=True, separators=(",", ":"))
    return (
        "DO $catalog_price_ledger$\n"
        "BEGIN\n"
        "    IF (SELECT COALESCE(jsonb_object_agg(filename, sha256), '{}'::jsonb)\n"
        "        FROM public.skviderm_go_migrations) IS DISTINCT FROM "
        "'" + literal + "'::jsonb THEN\n"
        "        RAISE EXCEPTION 'Go migration ledger does not match';\n"
        "    END IF;\n"
        "END $catalog_price_ledger$;\n"
    )


def apply_migration(operations, old, candidate):
    if (
        old.get("backend_language") != "go"
        or candidate.get("backend_language") != "go"
        or old["go_migrations"] == candidate["go_migrations"]
    ):
        return
    proof = check_deployment(operations, old, candidate)
    if operations.go_migration_ledger() == proof["after"]:
        return
    operation = operations.state.get("operation", {})
    backup = operations.state.get("last_backup")
    if (
        operation.get("command") != "deploy"
        or operation.get("status") != "running"
        or not isinstance(backup, str)
        or operation.get("backup") != backup
        or operation.get("candidate", {}).get("image") != candidate["image"]
        or not Path(operations.config["gate"]).is_file()
    ):
        raise ValueError("Go migration requires the gated deployment and encrypted backup")
    if operations.container_running(old["container"]):
        raise ValueError("Stop the current application before the Go migration")
    sql = trusted_sql(operations, proof)
    role = migration_role(operations)
    deadline = (datetime.now(timezone.utc) + timedelta(seconds=START_WINDOW_SECONDS)).isoformat()
    start_guard = (
        "DO $catalog_price_deadline$ BEGIN "
        "IF clock_timestamp() >= '" + deadline + "'::timestamptz THEN "
        "RAISE EXCEPTION 'Go migration start deadline passed'; "
        "END IF; END $catalog_price_deadline$;\n"
    )
    _, table, permissions, phase = APPROVALS[proof["migration"]]
    operations.phase(phase)
    operations.database(
        "BEGIN;\n"
        "SET LOCAL search_path = public;\n"
        "SET LOCAL lock_timeout = '5s';\n"
        + f"SET LOCAL statement_timeout = '{STATEMENT_TIMEOUT_MS}ms';\n"
        + f"SET LOCAL transaction_timeout = '{TRANSACTION_TIMEOUT_MS}ms';\n"
        + start_guard
        + "LOCK TABLE public.skviderm_go_migrations IN ACCESS EXCLUSIVE MODE;\n"
        + start_guard
        + ledger_assertion(proof["before"])
        + sql
        + '\nGRANT ' + permissions + ' ON public.' + table + ' TO "' + role + '";\n'
        + "INSERT INTO public.skviderm_go_migrations(filename, sha256) VALUES ('"
        + proof["migration"] + "', '" + proof["sql_sha256"] + "');\n"
        + ledger_assertion(proof["after"])
        + "COMMIT;\n"
    )
    if operations.go_migration_ledger() != proof["after"]:
        raise ValueError("Go migration did not produce the approved ledger")
