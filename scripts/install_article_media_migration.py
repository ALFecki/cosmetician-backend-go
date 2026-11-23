"""Update the migration helper on a VPS already prepared for articles."""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import stat

from install_catalog_price_migration import MODULE, atomic_write

SERVER_SHA256 = "649f3a1bf657de1c0832f584387aa2ec389856e500165423878d90450eecdb29"
HELPER_SHA256 = "4cdb243ea4386dcf05aa64c45f10521ddcdb6527120816c85d394153e7ea7fa2"
SAVED = MODULE + ".before-article-media"
PROOF = "article-media-migration.json"


def owned(path, file_type):
    info = path.lstat()
    if not file_type(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o022:
        raise ValueError(f"Expected root-owned operations file or directory: {path.name}")


def install(root):
    if os.geteuid() != 0:
        raise ValueError("Installing the server helper requires root")
    owned(root, stat.S_ISDIR)
    if root.resolve() != root:
        raise ValueError("Pass the absolute path of the operations directory")
    compiled_helper = Path(__file__).with_name(MODULE).read_bytes()
    compile(compiled_helper, MODULE, "exec")
    server, helper, lock, state, saved = (
        root / name for name in ("server.py", MODULE, "operation.lock", "state.json", SAVED)
    )
    owned(lock, stat.S_ISREG)
    with lock.open("a") as handle:
        fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        for path in (server, helper, state):
            owned(path, stat.S_ISREG)
        if hashlib.sha256(server.read_bytes()).hexdigest() != SERVER_SHA256:
            raise ValueError("Installed server.py differs from the reviewed article operations source")
        operation = json.loads(state.read_text()).get("operation", {})
        if (
            operation.get("status") in {"running", "needs-verification"}
            or operation.get("phase") == "recovery-required"
        ):
            raise ValueError("Wait for the current server operation before installing")
        original = helper.read_bytes()
        if saved.exists() or saved.is_symlink():
            owned(saved, stat.S_ISREG)
            if hashlib.sha256(saved.read_bytes()).hexdigest() != HELPER_SHA256:
                raise ValueError("Saved migration helper differs from the reviewed source")
            if original == compiled_helper:
                return
        if hashlib.sha256(original).hexdigest() != HELPER_SHA256:
            raise ValueError("Installed migration helper differs from the reviewed source")
        proof = root / PROOF
        if proof.exists() or proof.is_symlink():
            raise ValueError("Install and verify the helper before staging the article media proof")
        if not saved.exists():
            atomic_write(saved, original, 0o600)
        atomic_write(helper, compiled_helper, 0o600)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path("/opt/skviderm"))
    install(parser.parse_args().root)
    print("Article media migration support installed")
