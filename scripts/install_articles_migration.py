"""Install articles approval support on the exact reviewed catalog-price VPS helper."""

import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path

from install_catalog_price_migration import MODULE, atomic_write

BASE_SHA256 = "95359919e9a042519ad071196e62092043b33cbf2be538458fd16468924fb875"
HELPER_BASE_SHA256 = "ec73a15cdd3b5e4898ef66ce267e611de724e7d04bd82a7452d72bb683220525"
OLD_SNAPSHOT = (
    '        config_files.append(str(self.root / "catalog_price_migration.py"))\n'
    '        catalog_proof = self.root / catalog_price_migration.PROOF\n'
    '        if catalog_proof.exists():\n'
    '            catalog_price_migration.trusted_proof(self)\n'
    '            config_files.extend([\n'
    '                str(catalog_proof),\n'
    '                str(self.root / "go-migrations" / catalog_price_migration.MIGRATION),\n'
    '            ])\n'
)


def patched_source(original):
    if hashlib.sha256(original).hexdigest() != BASE_SHA256:
        raise ValueError("Installed server.py differs from the reviewed catalog-price source")
    source = original.decode("utf-8")
    if source.count(OLD_SNAPSHOT) != 1:
        raise ValueError("Reviewed operations snapshot location changed")
    source = source.replace(
        OLD_SNAPSHOT, "        config_files.extend(catalog_price_migration.recovery_files(self))\n", 1
    )
    compile(source, "server.py", "exec")
    return source.encode("utf-8")


def install(root):
    if os.geteuid() != 0:
        raise ValueError("Installing the server helper requires root")
    target, helper = root / "server.py", root / MODULE
    saved, saved_helper = root / "server.py.before-articles", root / (MODULE + ".before-articles")
    for path in (target, helper, root / "operation.lock", saved, saved_helper):
        if path.is_symlink() or (path.exists() and (
            not path.is_file() or path.stat().st_uid != 0 or path.stat().st_mode & 0o022
        )) or (path in (target, helper, root / "operation.lock") and not path.is_file()):
            raise ValueError("Expected root-owned regular operations files")
    with (root / "operation.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        article_proof = root / "articles-migration.json"
        if article_proof.exists() or article_proof.is_symlink():
            raise ValueError("Install and verify both operations files before staging the articles proof")
        original, installed_helper = target.read_bytes(), helper.read_bytes()
        output = patched_source(original)
        new_helper = Path(__file__).with_name(MODULE).read_bytes()
        compile(new_helper, MODULE, "exec")
        if hashlib.sha256(installed_helper).hexdigest() != HELPER_BASE_SHA256:
            if installed_helper != new_helper or not saved_helper.is_file():
                raise ValueError("Installed migration helper differs from the reviewed source")
            installed_helper = saved_helper.read_bytes()
            if hashlib.sha256(installed_helper).hexdigest() != HELPER_BASE_SHA256:
                raise ValueError("Saved migration helper differs from the reviewed source")
        operation = json.loads((root / "state.json").read_text()).get("operation", {})
        if operation.get("status") in {"running", "needs-verification"} or operation.get("phase") == "recovery-required":
            raise ValueError("Wait for the current server operation before installing")
        for path, content in ((saved, original), (saved_helper, installed_helper)):
            if path.exists() and path.read_bytes() != content:
                raise ValueError("Saved operations source differs from this installation")
            atomic_write(path, content, 0o600)
        atomic_write(helper, new_helper, 0o600)
        atomic_write(target, output, target.stat().st_mode & 0o777)
    print("Articles migration support installed")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path("/opt/skviderm"))
    parser.add_argument("--check", type=Path)
    args = parser.parse_args()
    if args.check:
        print(hashlib.sha256(patched_source(args.check.read_bytes())).hexdigest())
    else:
        install(args.root)


if __name__ == "__main__":
    main()
