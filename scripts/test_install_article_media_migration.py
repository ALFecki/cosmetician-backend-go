import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import install_article_media_migration as installer


class ArticleMediaInstallerTests(unittest.TestCase):
    def setUp(self):
        self.root = Path(self.enterContext(tempfile.TemporaryDirectory())).resolve()
        self.server = b"# server backed by the reviewed migration helper\n"
        self.old_helper = b"# helper with article tables\n"
        (self.root / "operation.lock").touch()
        (self.root / "state.json").write_text("{}")
        (self.root / "server.py").write_bytes(self.server)
        (self.root / installer.MODULE).write_bytes(self.old_helper)
        (self.root / "articles-migration.json").write_text('{"reviewed":true}')
        self.enterContext(patch.object(installer.os, "geteuid", return_value=0))
        self.enterContext(patch.object(installer, "SERVER_SHA256", hashlib.sha256(self.server).hexdigest()))
        self.enterContext(patch.object(installer, "HELPER_SHA256", hashlib.sha256(self.old_helper).hexdigest()))
        original_stat = Path.lstat

        def root_stats(path):
            result = original_stat(path)
            if path.is_relative_to(self.root):
                fields = list(result)
                fields[4] = 0
                return os.stat_result(fields)
            return result

        self.enterContext(patch.object(Path, "lstat", root_stats))

    def test_installs_idempotently_and_keeps_the_previous_helper_and_proof(self):
        installer.install(self.root)
        installer.install(self.root)
        self.assertEqual((self.root / "server.py").read_bytes(), self.server)
        self.assertEqual((self.root / installer.SAVED).read_bytes(), self.old_helper)
        self.assertEqual((self.root / "articles-migration.json").read_text(), '{"reviewed":true}')
        self.assertEqual(
            (self.root / installer.MODULE).read_bytes(),
            Path(installer.__file__).with_name(installer.MODULE).read_bytes(),
        )
        self.assertEqual((self.root / installer.MODULE).stat().st_mode & 0o777, 0o600)

    def test_does_not_install_over_an_active_operation_or_prepared_proof(self):
        state = self.root / "state.json"
        for operation in ({"status": "running"}, {"status": "needs-verification"}, {"phase": "recovery-required"}):
            with self.subTest(operation=operation):
                state.write_text(json.dumps({"operation": operation}))
                with self.assertRaisesRegex(ValueError, "current server operation"):
                    installer.install(self.root)
        state.write_text("{}")
        (self.root / installer.PROOF).touch()
        with self.assertRaisesRegex(ValueError, "before staging"):
            installer.install(self.root)
        self.assertEqual((self.root / installer.MODULE).read_bytes(), self.old_helper)
        self.assertFalse((self.root / installer.SAVED).exists())

    def test_rejects_a_different_or_nonregular_live_helper(self):
        helper = self.root / installer.MODULE
        helper.write_bytes(b"# unknown version\n")
        with self.assertRaisesRegex(ValueError, "differs"):
            installer.install(self.root)
        helper.unlink()
        helper.symlink_to(self.root / "server.py")
        with self.assertRaisesRegex(ValueError, "root-owned"):
            installer.install(self.root)
        self.assertFalse((self.root / installer.SAVED).exists())


if __name__ == "__main__":
    unittest.main()
