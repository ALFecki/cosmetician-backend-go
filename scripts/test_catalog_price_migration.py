import copy
import hashlib
import importlib.util
import json
import os
import subprocess
import tempfile
import time
from concurrent.futures import ThreadPoolExecutor
import unittest
import uuid
from pathlib import Path
from unittest.mock import patch

import catalog_price_migration as migration
from install_catalog_price_migration import patched_source

OLD_IMAGE = "sha256:" + "a" * 64
NEW_IMAGE = "sha256:" + "b" * 64
OLD_COMMIT = "a" * 40
NEW_COMMIT = "b" * 40
BEFORE = {
    "0001_cart_add_operation.sql": "c" * 64,
    "0002_promotion_draft.sql": "d" * 64,
}
RUST_LEDGER = {"20241118142949": "e" * 96}
SQL = (
    "CREATE TABLE public.catalog_price_migration_marker(id integer PRIMARY KEY);\n"
    "CREATE TABLE public.catalog_price_operation(id integer PRIMARY KEY);"
)
BASELINE = os.environ.get("SKVIDERM_OPERATIONS_BASELINE")
DSN = os.environ.get("SKVIDERM_CATALOG_MIGRATION_TEST_DSN")
ARTICLE_MIGRATION = "0004_articles.sql"
ARTICLE_PROOF = "articles-migration.json"
ARTICLE_SQL = (Path(__file__).resolve().parent.parent / "migrations" / ARTICLE_MIGRATION).read_text()


def install_article_proof(operations):
    article = {
        **operations.new, "image": "sha256:" + "e" * 64,
        "backend_commit": "e" * 40, "container": "article",
        "go_migrations": {
            **operations.after, ARTICLE_MIGRATION: hashlib.sha256(ARTICLE_SQL.encode()).hexdigest(),
        },
    }
    proof = {
        **operations.proof, "migration": ARTICLE_MIGRATION,
        "sql_sha256": article["go_migrations"][ARTICLE_MIGRATION],
        "old_image": operations.new["image"], "old_commit": operations.new["backend_commit"],
        "new_image": article["image"], "new_commit": article["backend_commit"],
        "before": operations.after, "after": article["go_migrations"],
    }
    (operations.root / ARTICLE_PROOF).write_text(json.dumps(proof))
    (operations.root / "go-migrations" / ARTICLE_MIGRATION).write_text(ARTICLE_SQL)
    return article


class LocalOperations:
    def __init__(self, root):
        self.root = root
        self.config = {"gate": str(root / "api-maintenance"), "go_role": "postgres"}
        self.sql = SQL
        self.before = copy.deepcopy(BEFORE)
        self.after = {**self.before, migration.MIGRATION: hashlib.sha256(self.sql.encode()).hexdigest()}
        self.old = {
            "backend_language": "go", "image": OLD_IMAGE, "backend_commit": OLD_COMMIT,
            "go_migrations": self.before, "safe_redis": True, "container": "old", "migrations": RUST_LEDGER,
        }
        self.new = {
            **self.old, "image": NEW_IMAGE, "backend_commit": NEW_COMMIT,
            "go_migrations": self.after, "container": "new",
        }
        self.ledger = copy.deepcopy(self.before)
        self.running = {"old": False, "new": False}
        self.state = {
            "last_backup": "20261123T120000Z",
            "operation": {
                "command": "deploy", "status": "running", "candidate": self.new,
                "backup": "20261123T120000Z",
            },
        }
        self.install_proof()
        Path(self.config["gate"]).touch()

    def install_proof(self):
        self.after = {**self.before, migration.MIGRATION: hashlib.sha256(self.sql.encode()).hexdigest()}
        self.new["go_migrations"] = self.after
        self.proof = {
            "schema": 1, "migration": migration.MIGRATION,
            "sql_sha256": self.after[migration.MIGRATION],
            "old_image": OLD_IMAGE, "new_image": NEW_IMAGE,
            "old_commit": OLD_COMMIT, "new_commit": NEW_COMMIT,
            "before": self.before, "after": self.after,
            "checks": {"old_runtime": True, "new_runtime": True, "migration": True},
        }
        (self.root / migration.PROOF).write_text(json.dumps(self.proof))
        sql = self.root / "go-migrations" / migration.MIGRATION
        sql.parent.mkdir(exist_ok=True)
        sql.write_text(self.sql)

    def private_go_file(self, path, label):
        if path.is_symlink() or not path.is_file():
            raise ValueError(label)
        metadata = path.stat()
        if metadata.st_uid != os.geteuid() or metadata.st_mode & 0o022:
            raise ValueError(label)
        return path

    def go_migration_ledger(self):
        return self.ledger

    def container_running(self, name):
        return self.running[name]

    def phase(self, phase):
        self.state["operation"]["phase"] = phase


class MigrationProofTests(unittest.TestCase):
    def setUp(self):
        self.root = Path(self.enterContext(tempfile.TemporaryDirectory()))
        self.operations = LocalOperations(self.root)

    def test_only_exact_approved_pair_can_extend_the_ledger(self):
        operations = self.operations
        migration.check_deployment(operations, operations.old, operations.new)
        for field, value in (
            ("image", "sha256:" + "f" * 64),
            ("backend_commit", "f" * 40),
            ("go_migrations", {**operations.after, "0004_unapproved.sql": "f" * 64}),
        ):
            with self.subTest(field=field):
                with self.assertRaises(ValueError):
                    migration.check_deployment(operations, operations.old, {**operations.new, field: value})
        with self.assertRaises(ValueError):
            migration.check_deployment(
                operations, {**operations.old, "backend_commit": "f" * 40}, operations.new
            )

    def test_role_identifiers_are_checked_before_the_application_is_stopped(self):
        operations = self.operations
        operations.config["go_role"] = 'application"; DROP TABLE person; --'
        with self.assertRaisesRegex(ValueError, "database role"):
            migration.check_deployment(operations, operations.old, operations.new)

    def test_existing_migrations_cannot_be_changed_or_removed(self):
        for change in ("replace", "remove", "extra"):
            proof = copy.deepcopy(self.operations.proof)
            if change == "replace":
                proof["after"]["0001_cart_add_operation.sql"] = "f" * 64
            elif change == "remove":
                del proof["after"]["0002_promotion_draft.sql"]
            else:
                proof["after"]["0004_extra.sql"] = "f" * 64
            (self.root / migration.PROOF).write_text(json.dumps(proof))
            with self.subTest(change=change):
                with self.assertRaisesRegex(ValueError, "compatibility"):
                    migration.check_deployment(self.operations, self.operations.old, self.operations.new)

    def test_untrusted_proof_sql_and_unrelated_actual_schema_are_rejected(self):
        operations = self.operations
        path = self.root / migration.PROOF
        path.chmod(0o666)
        with self.assertRaises(ValueError):
            migration.check_deployment(operations, operations.old, operations.new)
        path.chmod(0o600)
        path.unlink()
        path.symlink_to(self.root / "not-a-proof")
        with self.assertRaises(ValueError):
            migration.check_deployment(operations, operations.old, operations.new)
        path.unlink()
        operations.install_proof()
        (self.root / "go-migrations" / migration.MIGRATION).write_text("wrong SQL")
        with self.assertRaisesRegex(ValueError, "digest"):
            migration.check_deployment(operations, operations.old, operations.new)
        operations.install_proof()
        operations.ledger = {**operations.before, "0004_wrong.sql": "f" * 64}
        with self.assertRaisesRegex(ValueError, "Actual Go ledger"):
            migration.check_deployment(operations, operations.old, operations.new)

    def test_same_schema_releases_keep_the_exact_match_rule(self):
        operations = self.operations
        (self.root / migration.PROOF).unlink()
        unchanged = {**operations.old, "image": "sha256:" + "f" * 64, "backend_commit": "f" * 40}
        migration.check_deployment(operations, operations.old, unchanged)
        migration.check_release_schema(operations, unchanged)
        operations.ledger = operations.after
        with self.assertRaises(ValueError):
            migration.check_deployment(operations, operations.old, unchanged)

    def test_old_runtime_and_frontend_only_release_work_on_expanded_schema(self):
        operations = self.operations
        operations.ledger = operations.after
        migration.check_release_schema(operations, operations.old)
        migration.check_deployment(
            operations, operations.old, {**operations.old, "frontend_commit": "f" * 40}
        )
        migration.check_deployment(operations, operations.old, operations.new)
        with self.assertRaises(ValueError):
            migration.check_release_schema(
                operations, {**operations.old, "image": "sha256:" + "f" * 64}
            )
        (self.root / migration.PROOF).unlink()
        later = {**operations.new, "image": "sha256:" + "f" * 64, "backend_commit": "f" * 40}
        migration.check_deployment(operations, operations.new, later)

    def test_migration_requires_gate_stopped_runtime_and_current_backup(self):
        operations = self.operations
        mutations = (
            lambda: Path(operations.config["gate"]).unlink(),
            lambda: operations.running.update(old=True),
            lambda: operations.state["operation"].pop("backup"),
            lambda: operations.state["operation"].update(status="failed"),
        )
        for mutate in mutations:
            with self.subTest(mutate=mutate):
                Path(operations.config["gate"]).touch()
                operations.running["old"] = False
                operations.state["operation"].update(backup="20261123T120000Z", status="running")
                mutate()
                with self.assertRaises(ValueError):
                    migration.apply_migration(operations, operations.old, operations.new)


class ArticleMigrationProofTests(unittest.TestCase):
    def setUp(self):
        self.root = Path(self.enterContext(tempfile.TemporaryDirectory()))
        self.operations = LocalOperations(self.root)
        self.article = install_article_proof(self.operations)

    def test_each_transition_requires_its_own_exact_proof(self):
        operations = self.operations
        migration.check_deployment(operations, operations.old, operations.new)
        operations.ledger = operations.after
        migration.check_deployment(operations, operations.new, self.article)
        for field, value in (
            ("image", "sha256:" + "f" * 64),
            ("backend_commit", "f" * 40),
            ("go_migrations", {**self.article["go_migrations"], "0005_unapproved.sql": "f" * 64}),
        ):
            with self.subTest(field=field), self.assertRaises(ValueError):
                migration.check_deployment(operations, operations.new, {**self.article, field: value})

    def test_price_rollback_survives_and_article_rollback_is_not_transitive(self):
        operations = self.operations
        operations.ledger = operations.after
        migration.check_release_schema(operations, operations.old)
        operations.ledger = self.article["go_migrations"]
        migration.check_release_schema(operations, operations.new)
        migration.check_deployment(operations, operations.new, operations.new)
        migration.check_deployment(operations, operations.new, self.article)
        with self.assertRaises(ValueError):
            migration.check_release_schema(operations, operations.old)
        later = {**self.article, "image": "sha256:" + "f" * 64, "backend_commit": "f" * 40}
        migration.check_deployment(operations, self.article, later)

    def test_article_proof_and_sql_cannot_authorize_other_migrations(self):
        operations = self.operations
        operations.ledger = operations.after
        path = self.root / ARTICLE_PROOF
        proof = json.loads(path.read_text())
        proof["migration"] = migration.MIGRATION
        path.write_text(json.dumps(proof))
        with self.assertRaises(ValueError):
            migration.check_deployment(operations, operations.new, self.article)
        install_article_proof(operations)
        (self.root / "go-migrations" / ARTICLE_MIGRATION).write_text("unreviewed SQL")
        with self.assertRaisesRegex(ValueError, "digest"):
            migration.check_deployment(operations, operations.new, self.article)

    def test_recovery_archive_keeps_both_approved_proofs_and_sql(self):
        (self.root / "catalog_price_migration.py").write_text("# migration helper")
        files = migration.recovery_files(self.operations)
        for relative in (
            "catalog_price_migration.py", migration.PROOF, ARTICLE_PROOF,
            "go-migrations/" + migration.MIGRATION, "go-migrations/" + ARTICLE_MIGRATION,
        ):
            self.assertIn(str(self.root / relative), files)


@unittest.skipUnless(DSN, "set SKVIDERM_CATALOG_MIGRATION_TEST_DSN for isolated PostgreSQL")
class PostgreSQLMigrationTests(unittest.TestCase):
    def setUp(self):
        self.root = Path(self.enterContext(tempfile.TemporaryDirectory()))
        self.operations = LocalOperations(self.root)
        self.schema = "catalog_migration_" + uuid.uuid4().hex
        self.psql = os.environ.get("PSQL", "psql")
        self.execute(f"CREATE SCHEMA {self.schema}")
        self.addCleanup(self.execute, f"DROP SCHEMA {self.schema} CASCADE")
        self.execute(f"CREATE TABLE {self.schema}.skviderm_go_migrations(filename text PRIMARY KEY, sha256 text NOT NULL)")
        for filename, digest in BEFORE.items():
            self.execute(
                f"INSERT INTO {self.schema}.skviderm_go_migrations VALUES ('{filename}', '{digest}')"
            )
        self.operations.database = self.execute_isolated
        self.operations.go_migration_ledger = self.read_ledger

    def execute(self, sql):
        result = subprocess.run(
            [self.psql, "-XqAt", "-v", "ON_ERROR_STOP=1", DSN, "-c", sql],
            capture_output=True, text=True, check=False, timeout=20,
        )
        if result.returncode:
            raise RuntimeError("Test PostgreSQL request failed")
        return result.stdout.strip()

    def execute_isolated(self, sql):
        return self.execute(
            sql.replace("public.", self.schema + ".")
            .replace("search_path = public", "search_path = " + self.schema)
        )

    def read_ledger(self):
        return dict(
            line.split("|") for line in self.execute(
                f"SELECT filename,sha256 FROM {self.schema}.skviderm_go_migrations ORDER BY filename"
            ).splitlines()
        )

    def test_valid_sql_ledger_retry_and_old_image_recovery(self):
        operations = self.operations
        migration.apply_migration(operations, operations.old, operations.new)
        self.assertEqual(self.read_ledger(), operations.after)
        self.assertEqual(self.execute(f"SELECT count(*) FROM {self.schema}.catalog_price_migration_marker"), "0")
        migration.apply_migration(operations, operations.old, operations.new)
        migration.check_release_schema(operations, operations.old)

    def test_only_receipt_read_and_insert_are_granted_to_the_configured_role(self):
        operations = self.operations
        role = "catalog_price_role_" + uuid.uuid4().hex
        self.execute(f"CREATE ROLE {role} NOLOGIN")
        try:
            operations.config["go_role"] = role
            migration.apply_migration(operations, operations.old, operations.new)
            for permission, expected in (("SELECT", "t"), ("INSERT", "t"), ("UPDATE", "f"), ("DELETE", "f")):
                actual = self.execute(
                    f"SELECT has_table_privilege('{role}', '{self.schema}.catalog_price_operation', '{permission}')"
                )
                self.assertEqual(actual, expected, permission)
        finally:
            self.execute(f"DROP OWNED BY {role}; DROP ROLE {role}")

    def test_sql_failure_rolls_back_schema_and_ledger_together(self):
        operations = self.operations
        operations.sql = SQL + "\nSELECT 1/0;"
        operations.install_proof()
        with self.assertRaises(RuntimeError):
            migration.apply_migration(operations, operations.old, operations.new)
        self.assertEqual(self.read_ledger(), operations.before)
        self.assertEqual(
            self.execute(f"SELECT to_regclass('{self.schema}.catalog_price_migration_marker') IS NULL"), "t"
        )
        migration.check_release_schema(operations, operations.old)

    def test_transaction_timeout_bounds_multiple_individually_short_statements(self):
        operations = self.operations
        operations.sql = SQL + "\nSELECT pg_sleep(0.08); SELECT pg_sleep(0.08); SELECT pg_sleep(0.08);"
        operations.install_proof()
        with (
            patch.object(migration, "TRANSACTION_TIMEOUT_MS", 150),
            patch.object(migration, "STATEMENT_TIMEOUT_MS", 100),
        ):
            with self.assertRaises(RuntimeError):
                migration.apply_migration(operations, operations.old, operations.new)
        self.assertEqual(self.read_ledger(), operations.before)
        self.assertEqual(
            self.execute(f"SELECT to_regclass('{self.schema}.catalog_price_migration_marker') IS NULL"), "t"
        )

    def test_delayed_detached_execution_cannot_start_after_its_deadline(self):
        operations = self.operations
        with patch.object(migration, "START_WINDOW_SECONDS", -1):
            with self.assertRaises(RuntimeError):
                migration.apply_migration(operations, operations.old, operations.new)
        self.assertEqual(self.read_ledger(), operations.before)
        self.assertEqual(
            self.execute(f"SELECT to_regclass('{self.schema}.catalog_price_migration_marker') IS NULL"), "t"
        )

    def test_recovery_ledger_read_waits_until_the_migration_finishes(self):
        operations = self.operations
        operations.sql = SQL + "\nSELECT pg_sleep(1.2);"
        operations.install_proof()
        with ThreadPoolExecutor(max_workers=1) as pool:
            running = pool.submit(migration.apply_migration, operations, operations.old, operations.new)
            for _ in range(30):
                locked = self.execute(
                    "SELECT EXISTS (SELECT 1 FROM pg_locks WHERE "
                    f"relation='{self.schema}.skviderm_go_migrations'::regclass AND granted "
                    "AND mode IN ('ExclusiveLock','AccessExclusiveLock'))"
                )
                if locked == "t":
                    break
                time.sleep(0.02)
            else:
                self.fail("migration never locked the Go ledger")
            self.assertEqual(self.read_ledger(), operations.after)
            running.result(timeout=10)

    def test_unknown_commit_acknowledgement_is_resolved_from_actual_ledger(self):
        operations = self.operations

        def commit_then_lose_connection(sql):
            self.execute_isolated(sql)
            raise ConnectionError("commit acknowledgement lost")

        operations.database = commit_then_lose_connection
        with self.assertRaises(ConnectionError):
            migration.apply_migration(operations, operations.old, operations.new)
        self.assertEqual(self.read_ledger(), operations.after)
        migration.check_release_schema(operations, operations.old)
        operations.database = lambda _: self.fail("committed migration was executed twice")
        migration.apply_migration(operations, operations.old, operations.new)

    def test_two_proofs_apply_articles_and_recover_without_reverting_prices(self):
        operations = self.operations
        article = install_article_proof(operations)
        migration.apply_migration(operations, operations.old, operations.new)
        migration.check_release_schema(operations, operations.old)
        role = "articles_role_" + uuid.uuid4().hex
        self.execute(f"CREATE ROLE {role} NOLOGIN")
        try:
            operations.config["go_role"] = role
            operations.state["operation"]["candidate"] = article
            migration.apply_migration(operations, operations.new, article)
            self.assertEqual(self.read_ledger(), article["go_migrations"])
            self.assertEqual(self.execute(f"SELECT count(*) FROM {self.schema}.article"), "0")
            self.execute(
                f"INSERT INTO {self.schema}.article(title,summary,body) VALUES ('Published', '', 'Retained text')"
            )
            migration.check_release_schema(operations, operations.new)
            migration.check_deployment(operations, operations.new, operations.new)
            migration.apply_migration(operations, operations.new, article)
            self.assertEqual(self.execute(f"SELECT body FROM {self.schema}.article"), "Retained text")
            self.assertEqual(
                self.execute(f"SELECT to_regclass('{self.schema}.catalog_price_operation') IS NOT NULL"), "t"
            )
            for permission, expected in (("SELECT", "t"), ("INSERT", "t"), ("UPDATE", "t"), ("DELETE", "f")):
                actual = self.execute(
                    f"SELECT has_table_privilege('{role}', '{self.schema}.article', '{permission}')"
                )
                self.assertEqual(actual, expected, permission)
        finally:
            self.execute(f"DROP OWNED BY {role}; DROP ROLE {role}")


@unittest.skipUnless(BASELINE, "set SKVIDERM_OPERATIONS_BASELINE to the exact reviewed VPS script")
class PatchedOperationsTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        from install_articles_migration import patched_source as articles_source
        cls.output = articles_source(patched_source(Path(BASELINE).read_bytes()))
        cls.directory = tempfile.TemporaryDirectory()
        module_path = Path(cls.directory.name) / "server.py"
        module_path.write_bytes(cls.output)
        spec = importlib.util.spec_from_file_location("catalog_price_server_under_test", module_path)
        cls.server = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(cls.server)

    @classmethod
    def tearDownClass(cls):
        cls.directory.cleanup()

    def setUp(self):
        self.root = Path(self.enterContext(tempfile.TemporaryDirectory()))
        local = LocalOperations(self.root)
        server = self.server

        class SimulatedOperations(server.Operations):
            def __init__(self):
                self.__dict__.update(local.__dict__)
                self.state["current"], self.state["previous"] = self.old, None
                self.state_path = self.root / "state.json"
                self.running["old"] = True
                self.started = []
                self.writes = 0
                self.lose_ack = False
                self.fail_new = False

            def check_go_environment(self):
                return

            def migration_ledger(self):
                return RUST_LEDGER

            def go_migration_ledger(self):
                return self.ledger

            def check_disk(self):
                return

            def prepare(self, request):
                if request.get("frontend_only"):
                    return {**self.old, "frontend_commit": "f" * 40}
                return self.new

            def preserve_assets(self, _):
                return

            def gate(self, enabled):
                if enabled:
                    Path(self.config["gate"]).touch()
                else:
                    Path(self.config["gate"]).unlink(missing_ok=True)

            def hold_gate(self):
                self.gate(True)

            def stop_backend(self, release):
                self.running[release["container"]] = False

            def snapshot(self, release):
                if self.running[release["container"]]:
                    raise AssertionError("backup while application running")
                self.state["last_backup"] = "20261123T120000Z"
                self.phase("backup-complete", backup=self.state["last_backup"])

            def phase(self, phase, **fields):
                self.state["operation"].update(phase=phase, **fields)

            def database(self, sql):
                if any(self.running.values()) or not Path(self.config["gate"]).is_file():
                    raise AssertionError("SQL applied with application running or API open")
                if not self.state["operation"].get("backup"):
                    raise AssertionError("SQL applied without encrypted backup")
                self.writes += 1
                self.ledger = copy.deepcopy(self.after)
                if self.lose_ack:
                    raise ConnectionError("commit acknowledgement lost")

            def switch_frontend(self, release):
                self.frontend = release["backend_commit"]

            def ready(self):
                if self.running[self.new["container"]] and self.fail_new:
                    raise RuntimeError("candidate readiness failed")

            def public_smoke(self):
                return

            def retention(self):
                return

        self.operations = SimulatedOperations()
        Path(self.operations.config["gate"]).unlink()

        def inspect(name):
            return {"State": {"Running": self.operations.running[name]}}

        def run(arguments, **_):
            if arguments[:2] == ["docker", "start"]:
                name = arguments[2]
                self.operations.running[name] = True
                self.operations.started.append(name)
            return ""

        self.enterContext(patch.object(server, "inspect_container", side_effect=inspect))
        self.enterContext(patch.object(server, "run", side_effect=run))

    def test_snapshot_always_includes_the_imported_helper_without_a_proof(self):
        (self.root / migration.PROOF).unlink()
        archived = self.snapshot_archive()
        self.assertNotIn(str(self.root / migration.PROOF), archived)

    def test_snapshot_keeps_both_installed_proofs_and_sql(self):
        install_article_proof(self.operations)
        archived = self.snapshot_archive()
        for relative in (
            migration.PROOF, ARTICLE_PROOF,
            "go-migrations/" + migration.MIGRATION, "go-migrations/" + ARTICLE_MIGRATION,
        ):
            self.assertIn(str(self.root / relative), archived)

    def snapshot_archive(self):
        operations = self.operations
        operations.running["old"] = False
        operations.old["frontend"] = str(self.root / "frontend")
        Path(operations.old["frontend"]).mkdir()
        (self.root / "backups").mkdir()
        module = self.root / "catalog_price_migration.py"
        module.write_text("# installed migration helper")
        source = self.root / "server.py"
        source.write_bytes(self.output)
        operations.config.update(
            postgres="fixture-postgres", database="fixture", redis="fixture-redis",
            minio_data=str(self.root / "minio"), recovery_files=[str(source)],
            backup_recipient="age1fixture",
        )
        archived = []

        def archive_run(arguments, **_):
            if arguments[0] == "tar":
                target = Path(arguments[arguments.index("-cf") + 1]) if "-cf" in arguments else Path(arguments[2])
                target.write_bytes(b"test archive")
                if target.name == "config.tar":
                    archived.extend(arguments[arguments.index("--") + 1:])
            elif arguments[:2] == ["docker", "cp"]:
                Path(arguments[-1]).write_bytes(b"test Redis snapshot")
            elif arguments[0] == "age":
                Path(arguments[arguments.index("-o") + 1]).write_bytes(b"test encrypted backup")
            return ""

        with (
            patch.object(self.server, "run", side_effect=archive_run),
            patch.object(operations, "redis", return_value="0"),
        ):
            self.server.Operations.snapshot(operations, operations.old)
        self.assertIn(str(source), archived)
        self.assertIn(str(module), archived)
        return archived

    def test_patch_refuses_other_server_source(self):
        with self.assertRaisesRegex(ValueError, "differs"):
            patched_source(Path(BASELINE).read_bytes() + b"\n")

    def test_actual_deploy_rollback_and_ordinary_after_release(self):
        operations = self.operations
        operations.deploy({"id": "migration"})
        self.assertEqual(operations.ledger, operations.after)
        self.assertEqual(operations.writes, 1)
        self.assertEqual(operations.started, ["new"])
        self.assertEqual(operations.state["current"]["backend_commit"], NEW_COMMIT)
        operations.rollback()
        self.assertEqual(operations.started, ["new", "old"])
        self.assertEqual(operations.ledger, operations.after)
        self.assertFalse(Path(operations.config["gate"]).exists())

    def test_two_proof_deployment_rollback_and_frontend_only_release(self):
        operations = self.operations
        article = install_article_proof(operations)
        operations.deploy({"id": "prices"})
        operations.rollback()
        self.assertEqual(operations.ledger, operations.after)
        operations.deploy({"id": "prices-retry"})
        operations.old, operations.new = operations.new, article
        operations.after = article["go_migrations"]
        operations.running["article"] = False
        operations.deploy({"id": "articles"})
        self.assertEqual(operations.writes, 2)
        self.assertEqual(operations.state["current"]["backend_commit"], "e" * 40)
        operations.rollback()
        self.assertEqual(operations.ledger, article["go_migrations"])
        self.assertEqual(operations.state["current"]["backend_commit"], NEW_COMMIT)
        operations.deploy({"id": "frontend-only", "frontend_only": True})
        operations.deploy({"id": "articles-retry"})
        self.assertEqual(operations.writes, 2)
        self.assertFalse(Path(operations.config["gate"]).exists())

    def test_article_commit_ack_failure_recovers_then_frontend_only_and_retry_succeed(self):
        operations = self.operations
        article = install_article_proof(operations)
        operations.deploy({"id": "prices"})
        operations.old, operations.new = operations.new, article
        operations.after = article["go_migrations"]
        operations.running["article"] = False
        operations.lose_ack = True
        with self.assertRaises(ConnectionError):
            operations.deploy({"id": "articles"})
        self.assertEqual(operations.ledger, article["go_migrations"])
        self.assertEqual(operations.state["current"]["backend_commit"], NEW_COMMIT)
        self.assertTrue(operations.running["new"])
        self.assertFalse(Path(operations.config["gate"]).exists())
        operations.deploy({"id": "frontend-only", "frontend_only": True})
        operations.lose_ack = False
        operations.deploy({"id": "articles-retry"})
        self.assertEqual(operations.writes, 2)
        self.assertEqual(operations.state["current"]["backend_commit"], "e" * 40)

    def test_commit_ack_failure_recovers_then_frontend_only_and_retry_succeed(self):
        operations = self.operations
        operations.lose_ack = True
        with self.assertRaises(ConnectionError):
            operations.deploy({"id": "first"})
        self.assertEqual(operations.state["current"]["backend_commit"], OLD_COMMIT)
        self.assertEqual(operations.ledger, operations.after)
        self.assertEqual(operations.started, ["old"])
        self.assertFalse(Path(operations.config["gate"]).exists())
        operations.deploy({"id": "frontend", "frontend_only": True})
        self.assertEqual(operations.writes, 1)
        operations.lose_ack = False
        operations.deploy({"id": "retry"})
        self.assertEqual(operations.writes, 1)
        self.assertEqual(operations.state["current"]["backend_commit"], NEW_COMMIT)

    def test_candidate_failure_recovers_old_image_on_after_schema(self):
        operations = self.operations
        operations.fail_new = True
        with self.assertRaisesRegex(RuntimeError, "readiness"):
            operations.deploy({"id": "bad-candidate"})
        self.assertEqual(operations.started, ["new", "old"])
        self.assertTrue(operations.running["old"])
        self.assertFalse(operations.running["new"])
        self.assertEqual(operations.state["current"]["backend_commit"], OLD_COMMIT)
        self.assertFalse(Path(operations.config["gate"]).exists())

    def test_rejected_candidate_leaves_live_app_and_ledger_untouched(self):
        operations = self.operations
        operations.new["backend_commit"] = "f" * 40
        with self.assertRaises(ValueError):
            operations.deploy({"id": "wrong"})
        self.assertTrue(operations.running["old"])
        self.assertEqual(operations.ledger, operations.before)
        self.assertEqual(operations.writes, 0)
        self.assertEqual(operations.started, [])
        self.assertFalse(Path(operations.config["gate"]).exists())


if __name__ == "__main__":
    unittest.main()
