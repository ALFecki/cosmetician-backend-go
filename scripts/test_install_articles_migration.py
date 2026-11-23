import hashlib
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import install_articles_migration as installer
from install_catalog_price_migration import patched_source as catalog_source

BASELINE = os.environ.get("SKVIDERM_OPERATIONS_BASELINE")


@unittest.skipUnless(BASELINE, "set SKVIDERM_OPERATIONS_BASELINE to the reviewed VPS script")
class ArticlesInstallerTests(unittest.TestCase):
    def setUp(self):
        self.root = Path(self.enterContext(tempfile.TemporaryDirectory()))
        self.original = catalog_source(Path(BASELINE).read_bytes())
        (self.root / "server.py").write_bytes(self.original)
        self.helper = b"# approved previous helper\n"
        (self.root / installer.MODULE).write_bytes(self.helper)
        (self.root / "operation.lock").touch()
        self.write_state({})
        self.enterContext(patch.object(installer.os, "geteuid", return_value=0))
        self.enterContext(patch.object(installer, "HELPER_BASE_SHA256", hashlib.sha256(self.helper).hexdigest()))
        original_stat = Path.stat

        def root_owned_stat(path, *args, **kwargs):
            metadata = original_stat(path, *args, **kwargs)
            if path.is_relative_to(self.root):
                values = list(metadata)
                values[4] = 0
                return os.stat_result(values)
            return metadata

        self.enterContext(patch.object(Path, "stat", root_owned_stat))

    def write_state(self, operation):
        (self.root / "state.json").write_text(json.dumps({"operation": operation}))

    def test_installs_new_snapshot_support_and_keeps_both_original_files(self):
        installer.install(self.root)
        self.assertEqual((self.root / "server.py.before-articles").read_bytes(), self.original)
        self.assertEqual((self.root / (installer.MODULE + ".before-articles")).read_bytes(), self.helper)
        self.assertNotEqual((self.root / "server.py").read_bytes(), self.original)
        self.assertEqual(
            (self.root / installer.MODULE).read_bytes(),
            Path(installer.__file__).with_name(installer.MODULE).read_bytes(),
        )

    def test_partial_install_can_resume_with_verified_saved_helper(self):
        new_helper = Path(installer.__file__).with_name(installer.MODULE).read_bytes()
        (self.root / installer.MODULE).write_bytes(new_helper)
        (self.root / (installer.MODULE + ".before-articles")).write_bytes(self.helper)
        installer.install(self.root)
        self.assertNotEqual((self.root / "server.py").read_bytes(), self.original)

    def test_rejects_changed_server_or_helper_before_writing(self):
        for filename in ("server.py", installer.MODULE):
            with self.subTest(filename=filename):
                (self.root / "server.py").write_bytes(self.original)
                (self.root / installer.MODULE).write_bytes(self.helper)
                path = self.root / filename
                path.write_bytes(path.read_bytes() + b"# unreviewed change\n")
                original = path.read_bytes()
                with self.assertRaisesRegex(ValueError, "differs"):
                    installer.install(self.root)
                self.assertEqual(path.read_bytes(), original)
                self.assertFalse((self.root / "server.py.before-articles").exists())

    def test_active_operations_and_unsafe_helper_files_are_rejected(self):
        for operation in (
            {"status": "running"}, {"status": "needs-verification"}, {"phase": "recovery-required"},
        ):
            with self.subTest(operation=operation):
                self.write_state(operation)
                with self.assertRaisesRegex(ValueError, "current server operation"):
                    installer.install(self.root)
        self.write_state({})
        path = self.root / installer.MODULE
        path.chmod(0o666)
        with self.assertRaisesRegex(ValueError, "root-owned"):
            installer.install(self.root)
        path.chmod(0o600)
        path.unlink()
        path.symlink_to(self.root / "server.py")
        with self.assertRaisesRegex(ValueError, "root-owned"):
            installer.install(self.root)
        self.assertEqual((self.root / "server.py").read_bytes(), self.original)

    def test_staged_articles_proof_is_rejected_before_either_file_changes(self):
        proof = self.root / "articles-migration.json"
        proof.write_text("{}")
        with self.assertRaisesRegex(ValueError, "before staging"):
            installer.install(self.root)
        self.assertEqual((self.root / "server.py").read_bytes(), self.original)
        self.assertEqual((self.root / installer.MODULE).read_bytes(), self.helper)
        self.assertFalse((self.root / "server.py.before-articles").exists())


if __name__ == "__main__":
    unittest.main()
