"""Install catalog migration support against the exact reviewed VPS operations source."""

import argparse
import fcntl
import hashlib
import json
import os
import tempfile
from pathlib import Path

BASE_SHA256 = "56ad9d32f30a18a9ec330956fc6e45b6c9603365018785187ac1d4b4ded1e2c5"
MODULE = "catalog_price_migration.py"
REPLACEMENTS = (
    (
        "from zoneinfo import ZoneInfo\n",
        "from zoneinfo import ZoneInfo\n"
        "import catalog_price_migration\n",
    ),
    (
        "            check_migrations(release[\"go_migrations\"], self.go_migration_ledger())\n",
        "            catalog_price_migration.check_release_schema(self, release)\n",
    ),
    (
        "        if old.get(\"backend_language\") == \"go\":\n"
        "            check_migrations(go_migrations, old[\"go_migrations\"])\n"
        "            check_migrations(go_migrations, self.go_migration_ledger())\n"
        "            return None\n",
        "        if old.get(\"backend_language\") == \"go\":\n"
        "            catalog_price_migration.check_deployment(self, old, candidate)\n"
        "            return None\n",
    ),
    (
        "    def go_migration_ledger(self):\n",
        "    def container_running(self, name):\n"
        "        return inspect_container(name)[\"State\"][\"Running\"]\n\n"
        "    def go_migration_ledger(self):\n",
    ),
    (
        "            if cutover is not None:\n"
        "                self.phase(\"preparing-go-database\")\n",
        "            catalog_price_migration.apply_migration(self, old, candidate)\n"
        "            if cutover is not None:\n"
        "                self.phase(\"preparing-go-database\")\n",
    ),
    (
        "        if self.config.get(\"go_env_file\"):\n",
        "        config_files.append(str(self.root / \"catalog_price_migration.py\"))\n"
        "        catalog_proof = self.root / catalog_price_migration.PROOF\n"
        "        if catalog_proof.exists():\n"
        "            catalog_price_migration.trusted_proof(self)\n"
        "            config_files.extend([\n"
        "                str(catalog_proof),\n"
        "                str(self.root / \"go-migrations\" / catalog_price_migration.MIGRATION),\n"
        "            ])\n"
        "        if self.config.get(\"go_env_file\"):\n",
    ),
)


def patched_source(original):
    if hashlib.sha256(original).hexdigest() != BASE_SHA256:
        raise ValueError("Installed server.py differs from the reviewed VPS source")
    source = original.decode("utf-8")
    for before, after in REPLACEMENTS:
        if source.count(before) != 1:
            raise ValueError("Reviewed operations patch location changed")
        source = source.replace(before, after, 1)
    compile(source, "server.py", "exec")
    return source.encode("utf-8")


def atomic_write(path, content, mode):
    with tempfile.NamedTemporaryFile(dir=path.parent, prefix="." + path.name + "-", delete=False) as temporary:
        pending = Path(temporary.name)
        try:
            os.fchmod(temporary.fileno(), mode)
            temporary.write(content)
            temporary.flush()
            os.fsync(temporary.fileno())
            os.replace(pending, path)
        finally:
            pending.unlink(missing_ok=True)
    descriptor = os.open(path.parent, os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def install(root):
    if os.geteuid() != 0:
        raise ValueError("Installing the server helper requires root")
    target = root / "server.py"
    for path in (target, root / "operation.lock"):
        if path.is_symlink() or not path.is_file() or path.stat().st_uid != 0 or path.stat().st_mode & 0o022:
            raise ValueError("Expected root-owned regular operations files")
    with (root / "operation.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        original = target.read_bytes()
        output = patched_source(original)
        state = json.loads((root / "state.json").read_text())
        operation = state.get("operation", {})
        if operation.get("status") in {"running", "needs-verification"} or operation.get("phase") == "recovery-required":
            raise ValueError("Wait for the current server operation before installing")
        helper = Path(__file__).with_name(MODULE).read_bytes()
        compile(helper, MODULE, "exec")
        saved = root / "server.py.before-catalog-price-cents"
        if saved.exists() and (saved.is_symlink() or saved.read_bytes() != original):
            raise ValueError("The saved operations source differs from this installation")
        atomic_write(saved, original, 0o600)
        atomic_write(root / MODULE, helper, 0o600)
        atomic_write(target, output, target.stat().st_mode & 0o777)
    print("Catalog price migration support installed")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path("/opt/skviderm"))
    parser.add_argument("--check", type=Path)
    args = parser.parse_args()
    if args.check:
        output = patched_source(args.check.read_bytes())
        print(hashlib.sha256(output).hexdigest())
    else:
        install(args.root)


if __name__ == "__main__":
    main()
