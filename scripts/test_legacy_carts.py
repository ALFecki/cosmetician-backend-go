import hashlib
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import legacy_carts


OWNER = "c4da3696-e5a7-4974-9c88-6764f9771558"
ORDER = "50857feb-e319-4312-8639-be1fd248d5e9"
VARIANT = "1400d6f9-0455-46aa-a469-df116e769640"
KEY = f"person:{OWNER}:order"


def cached_order(*quantities):
    return {
        "id": ORDER,
        "personId": OWNER,
        "status": "NotChecked",
        "promoCodeId": None,
        "products": [
            {"variant": {"id": VARIANT}, "quantity": quantity}
            for quantity in quantities
        ],
        "contactSnapshot": {"phone": "test-only"},
    }


def snapshot_bytes(*records):
    lines = b"".join(json.dumps(record).encode() + b"\n" for record in records)
    footer = {"count": len(records), "sha256": hashlib.sha256(lines).hexdigest()}
    return lines + json.dumps(footer).encode() + b"\n"


class LegacyCartSnapshotTest(unittest.TestCase):
    def test_normalizes_duplicate_variants_and_keeps_only_cart_fields(self):
        order = legacy_carts.read_record(KEY, cached_order(2, 3))["order"]
        self.assertEqual(order["products"], [{"variant": {"id": VARIANT}, "quantity": 5}])
        self.assertNotIn("contactSnapshot", order)

    def test_rejects_invalid_quantity_and_owner(self):
        for quantities in ((0,), (True,), (99, 1)):
            with self.subTest(quantities=quantities), self.assertRaisesRegex(ValueError, "quantity"):
                legacy_carts.read_record(KEY, cached_order(*quantities))
        with self.assertRaisesRegex(ValueError, "does not match its owner"):
            legacy_carts.read_record(f"person:{ORDER}:order", cached_order())

    def test_rejects_truncated_snapshot_before_running_psql(self):
        data = snapshot_bytes({"key": KEY, "order": cached_order(1)})
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "snapshot.jsonl"
            path.write_bytes(data.splitlines(keepends=True)[0])
            with patch("legacy_carts.subprocess.run") as run:
                with self.assertRaisesRegex(ValueError, "incomplete"):
                    legacy_carts.import_snapshot(path, True)
                run.assert_not_called()

    def test_does_not_publish_snapshot_when_a_key_disappears(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "snapshot.jsonl"
            with patch("legacy_carts.redis_output", side_effect=[KEY + "\n", "null"]):
                with self.assertRaisesRegex(ValueError, "disappeared"):
                    legacy_carts.save_snapshot(path, [])
            self.assertFalse(path.exists())

    def test_snapshot_export_is_private_and_refuses_to_overwrite(self):
        record = cached_order(1, 2)
        redis_replies = [KEY + "\n", json.dumps(json.dumps(record))] * 2
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "snapshot.jsonl"
            with patch("legacy_carts.redis_output", side_effect=redis_replies), patch("builtins.print"):
                legacy_carts.save_snapshot(path, [])
                before = path.read_bytes()
                with self.assertRaises(FileExistsError):
                    legacy_carts.save_snapshot(path, [])
            self.assertEqual(path.read_bytes(), before)
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            self.assertEqual(list(legacy_carts.snapshot_records(path))[0]["products"][0]["quantity"], 3)


if __name__ == "__main__":
    unittest.main()
