import hashlib
import io
import json
import re
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

with patch.dict(sys.modules, {"server": SimpleNamespace()}):
    import github_deploy


RELEASE = "20261119T120000Z-abcdef01"
BACKEND = "a" * 40
FRONTEND = "b" * 40
RUN_ID = "123"
BACKUP = "20261119T120001Z"


def backend_artifacts(*, commit=BACKEND, run_id=RUN_ID):
    migrations = json.dumps({"0001_legacy_carts.sql": "c" * 64}).encode()
    image = b"backend image"
    return (
        json.dumps({
            "schema": 1,
            "component": "backend",
            "backend_language": "go",
            "repository": "ALFecki/cosmetician-backend-go",
            "build_source": "github-actions",
            "commit": commit,
            "run_id": run_id,
            "container_verified": True,
            "files": {
                "migrations.json": hashlib.sha256(migrations).hexdigest(),
                "image.tar.gz": hashlib.sha256(image).hexdigest(),
            },
        }).encode(),
        migrations,
        image,
    )


def frontend_artifacts():
    archive = b"frontend archive"
    return (
        json.dumps({
            "schema": 1,
            "component": "frontend",
            "repository": "ALFecki/cosmetician-frontend",
            "build_source": "github-actions",
            "commit": FRONTEND,
            "run_id": RUN_ID,
            "files": {"frontend.tar.gz": hashlib.sha256(archive).hexdigest()},
        }).encode(),
        archive,
    )


def frames(*artifacts):
    return b"".join(str(len(artifact)).encode() + b"\n" + artifact for artifact in artifacts)


def load_manifest(path, component, commit, *, backend_language):
    manifest = json.loads(path.read_text())
    if manifest["component"] != component or manifest["commit"] != commit:
        raise ValueError("Artifact does not match requested component or commit")
    if component == "backend" and manifest["backend_language"] != backend_language:
        raise ValueError("Unexpected artifact backend language")
    return manifest


def write_json(path, value):
    path.write_text(json.dumps(value))


class GitHubDeploymentGatewayTest(unittest.TestCase):
    def setUp(self):
        self.root = Path(self.enterContext(tempfile.TemporaryDirectory()))
        for name in ("incoming", "results", "releases"):
            (self.root / name).mkdir()
        self.release = self.root / "incoming" / RELEASE
        self.result = self.root / "results" / f"{RELEASE}.json"
        self.state = self.root / "state.json"
        write_json(self.state, {"current": {"backend_language": "go"}})
        self.server = SimpleNamespace(
            RELEASE_ID=re.compile(r"[0-9]{8}T[0-9]{6}Z(?:-[a-f0-9]{8})?"),
            load_manifest=load_manifest,
            write_json=write_json,
            run=Mock(),
        )
        self.enterContext(patch.object(github_deploy, "server", self.server))
        self.backend_submit = f"submit-backend {RELEASE} {BACKEND} {RUN_ID}"
        self.paired_submit = f"submit {RELEASE} {BACKEND} {FRONTEND} {RUN_ID}"
        self.status = f"status {RELEASE}"

    def dispatch(self, command, data=b"", *, backend_only=False):
        if backend_only:
            return github_deploy.dispatch(command, io.BytesIO(data), self.root, backend_only=True)
        return github_deploy.dispatch(command, io.BytesIO(data), self.root)

    def write_success(self, *, frontend=FRONTEND, phase="complete", backup=BACKUP):
        write_json(self.result, {
            "status": "success",
            "state": {
                "current": {
                    "id": RELEASE, "backend_language": "go",
                    "backend_commit": BACKEND, "frontend_commit": frontend,
                },
                "operation": {
                    "id": RELEASE, "command": "deploy", "status": "success",
                    "phase": phase, "backup": backup,
                },
                "last_backup": BACKUP,
            },
        })

    def test_probe_scope_comes_from_forced_command_argv(self):
        for args, expected in (
            ([], {"protocol": 1}),
            (["--backend-only"], {"protocol": 1, "scope": "backend"}),
        ):
            with (
                self.subTest(args=args),
                patch.object(sys, "argv", ["github_deploy.py", *args]),
                patch.object(sys, "stdout", new_callable=io.StringIO) as output,
                patch.object(sys, "stdin", SimpleNamespace(buffer=io.BytesIO())),
                patch.dict(github_deploy.os.environ, {"SSH_ORIGINAL_COMMAND": "probe"}),
                patch.object(github_deploy.os, "geteuid", return_value=0),
                patch.object(github_deploy.os, "umask"),
                patch.object(github_deploy.signal, "signal"),
                patch.object(github_deploy.signal, "alarm"),
            ):
                github_deploy.main()
                self.assertEqual(json.loads(output.getvalue()), expected)

    def test_backend_submit_stages_three_frames_and_cleans_just_the_backend_archive(self):
        artifacts = backend_artifacts()
        self.assertEqual(
            self.dispatch(self.backend_submit, frames(*artifacts), backend_only=True),
            {"id": RELEASE, "status": "running"},
        )
        self.assertEqual(json.loads((self.release / "request.json").read_text()), {
            "id": RELEASE,
            "backend_language": "go",
            "backend_commit": BACKEND,
            "github_actions_run_id": RUN_ID,
        })
        self.assertEqual(sorted(
            str(path.relative_to(self.release))
            for path in self.release.rglob("*") if path.is_file()
        ), ["backend/image.tar.gz", "backend/manifest.json", "backend/migrations.json", "request.json"])
        for name, artifact in zip(("manifest.json", "migrations.json", "image.tar.gz"), artifacts):
            self.assertEqual((self.release / "backend" / name).read_bytes(), artifact)
        self.assertEqual(self.server.run.call_args.args[0], [
            "/usr/bin/systemd-run", "--quiet",
            "--unit=skviderm-operation-20261119t120000z-abcdef01",
            "--property=Type=exec", "--property=UMask=0077", "--property=Nice=10",
            f"--property=ExecStopPost=/usr/bin/rm -f -- {self.release}/backend/image.tar.gz",
            "/usr/bin/python3", str(self.root / "server.py"), "deploy",
            str(self.release / "request.json"), "--result", str(self.result),
        ])

    def test_keys_can_only_submit_their_own_release_type(self):
        for command, backend_only in (
            (self.backend_submit, False),
            (self.paired_submit, True),
            (f"--backend-only {self.backend_submit}", False),
            (f"submit-backend {RELEASE} {BACKEND} {FRONTEND} {RUN_ID}", True),
        ):
            with self.subTest(command=command, backend_only=backend_only):
                with self.assertRaisesRegex(ValueError, "Invalid GitHub deployment command"):
                    self.dispatch(command, backend_only=backend_only)
        self.server.run.assert_not_called()

    def test_backend_submit_rejects_incomplete_or_extra_frames(self):
        payload = frames(*backend_artifacts())
        for data, error in (
            (payload[:-1], "Truncated artifact frame"),
            (payload + frames(*frontend_artifacts()), "Extra artifact input"),
            (b"65537\n" + payload, "Invalid artifact frame size"),
        ):
            with self.subTest(error=error), self.assertRaisesRegex(ValueError, error):
                self.dispatch(self.backend_submit, data, backend_only=True)
        self.assertFalse(self.release.exists())
        self.server.run.assert_not_called()

    def test_backend_submit_requires_matching_run_and_commit(self):
        for artifacts, error in (
            (backend_artifacts(run_id="124"), "Unexpected GitHub Actions provenance"),
            (backend_artifacts(commit="c" * 40), "does not match"),
        ):
            with self.subTest(error=error), self.assertRaisesRegex(ValueError, error):
                self.dispatch(self.backend_submit, frames(*artifacts), backend_only=True)
        self.assertFalse(self.release.exists())
        self.server.run.assert_not_called()

    def test_backend_submit_rejects_a_rust_runtime(self):
        write_json(self.state, {"current": {"backend_language": "rust"}})
        with self.assertRaisesRegex(ValueError, "requires a current Go release"):
            self.dispatch(self.backend_submit, frames(*backend_artifacts()), backend_only=True)
        self.assertFalse(self.release.exists())
        self.server.run.assert_not_called()

    def test_backend_status_reports_the_actual_frontend_and_hides_it_from_paired_key(self):
        self.dispatch(self.backend_submit, frames(*backend_artifacts()), backend_only=True)
        actual_frontend = "c" * 40
        self.write_success(frontend=actual_frontend)
        self.assertEqual(self.dispatch(self.status, backend_only=True), {
            "id": RELEASE, "status": "success", "phase": "complete", "last_backup": BACKUP,
            "backend_commit": BACKEND, "frontend_commit": actual_frontend,
        })
        with self.assertRaises(ValueError):
            self.dispatch(self.status)

    def test_backend_status_requires_complete_success_metadata(self):
        self.dispatch(self.backend_submit, frames(*backend_artifacts()), backend_only=True)
        for fields in (
            {"frontend": ""},
            {"phase": "candidate-ready"},
            {"backup": "20261119T120002Z"},
        ):
            with self.subTest(fields=fields), self.assertRaises(ValueError):
                self.write_success(**fields)
                self.dispatch(self.status, backend_only=True)

    def test_paired_key_still_submits_and_reads_paired_release(self):
        self.assertEqual(
            self.dispatch(self.paired_submit, frames(*backend_artifacts(), *frontend_artifacts())),
            {"id": RELEASE, "status": "running"},
        )
        request = json.loads((self.release / "request.json").read_text())
        self.assertEqual(request["frontend_commit"], FRONTEND)
        cleanup = f"{self.release}/backend/image.tar.gz {self.release}/frontend/frontend.tar.gz"
        self.assertIn("--property=ExecStopPost=/usr/bin/rm -f -- " + cleanup, self.server.run.call_args.args[0])
        self.write_success()
        self.assertEqual(self.dispatch(self.status), {
            "id": RELEASE, "status": "success", "phase": "complete", "last_backup": BACKUP,
            "backend_commit": BACKEND, "frontend_commit": FRONTEND,
        })
        with self.assertRaises(ValueError):
            self.dispatch(self.status, backend_only=True)


if __name__ == "__main__":
    unittest.main()
