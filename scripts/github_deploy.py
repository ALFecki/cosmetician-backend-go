#!/usr/bin/env python3
"""Restricted SSH entry point for the GitHub Actions production deploy job."""

import fcntl
import json
import os
import re
import signal
import subprocess
import sys
from pathlib import Path
from tempfile import TemporaryDirectory

import server

ROOT = Path("/opt/skviderm")
COMMIT = re.compile(r"[a-f0-9]{40}")
RUN_ID = re.compile(r"[1-9][0-9]{0,19}")
BACKUP_ID = re.compile(r"[0-9]{8}T[0-9]{6}Z")
BACKEND_FRAMES = (
    ("backend/manifest.json", 64 * 1024),
    ("backend/migrations.json", 1024**2),
    ("backend/image.tar.gz", 2 * 1024**3),
)
FRONTEND_FRAMES = (
    ("frontend/manifest.json", 64 * 1024),
    ("frontend/frontend.tar.gz", 500 * 1024**2),
)


def receive(stream, staging, frames):
    for name, maximum in frames:
        header = stream.readline(len(str(maximum)) + 2)
        if not re.fullmatch(rb"[1-9][0-9]*\n", header) or int(header) > maximum:
            raise ValueError("Invalid artifact frame size")
        remaining = int(header)
        destination = staging / name
        destination.parent.mkdir(mode=0o700, exist_ok=True)
        with destination.open("xb") as output:
            while remaining:
                chunk = stream.read(min(remaining, 1024**2))
                if not chunk:
                    raise ValueError("Truncated artifact frame")
                output.write(chunk)
                remaining -= len(chunk)
    if stream.read(1):
        raise ValueError("Extra artifact input")


def unit_name(identifier):
    return "skviderm-operation-" + identifier.lower()


def check_current_release(current, *, frontend_only):
    if current.get("backend_language") != "go":
        raise ValueError("GitHub Actions deployment requires a current Go release")
    if frontend_only:
        server.require_safe_release(current)


def submit(root, stream, identifier, backend, frontend, run_id):
    incoming = root / "incoming"
    release = incoming / identifier
    result = root / "results" / f"{identifier}.json"
    if any(path.exists() for path in (release, result, root / "releases" / identifier)):
        raise ValueError("Release ID already in use")
    frontend_only = backend is None
    current = json.loads((root / "state.json").read_text())["current"]
    check_current_release(current, frontend_only=frontend_only)

    frames = []
    components = []
    archives = []
    request = {
        "id": identifier,
        "backend_language": "go",
        "github_actions_run_id": run_id,
    }
    if backend is not None:
        frames.extend(BACKEND_FRAMES)
        components.append(("backend", backend))
        archives.append(str(release / "backend" / "image.tar.gz"))
        request["backend_commit"] = backend
    if frontend is not None:
        frames.extend(FRONTEND_FRAMES)
        components.append(("frontend", frontend))
        archives.append(str(release / "frontend" / "frontend.tar.gz"))
        request["frontend_commit"] = frontend

    with TemporaryDirectory(dir=incoming, prefix=".github-actions-") as directory:
        staging = Path(directory)
        receive(stream, staging, frames)
        for component, commit in components:
            manifest = server.load_manifest(
                staging / component / "manifest.json", component, commit,
                backend_language="go",
            )
            if (
                manifest.get("build_source") != "github-actions"
                or str(manifest.get("run_id")) != run_id
            ):
                raise ValueError("Unexpected GitHub Actions provenance")
        server.write_json(staging / "request.json", request)
        with (staging / "request.json").open("rb+") as request_file:
            fcntl.flock(request_file, fcntl.LOCK_EX)
            if release.exists():
                raise ValueError("Release ID already in use")
            staging.rename(release)
            if frontend_only:
                worker = [str(root / "github_deploy.py"), "--apply-frontend", identifier]
            else:
                worker = [
                    str(root / "server.py"), "deploy", str(release / "request.json"),
                    "--result", str(result),
                ]
            server.run([
                "/usr/bin/systemd-run",
                "--quiet",
                "--unit=" + unit_name(identifier),
                "--property=Type=exec",
                "--property=UMask=0077",
                "--property=Nice=10",
                "--property=ExecStopPost=/usr/bin/rm -f -- " + " ".join(archives),
                "/usr/bin/python3",
                *worker,
            ], timeout=30)
    return {"id": identifier, "status": "running"}


def apply_frontend(root, identifier):
    if not server.RELEASE_ID.fullmatch(identifier):
        raise ValueError("Invalid release ID")
    result_path = root / "results" / f"{identifier}.json"
    result = {"status": "failed"}
    try:
        with server.operation_lock(root):
            operations = server.Operations(root)
            operations.ensure_idle()
            check_current_release(operations.state["current"], frontend_only=True)
            incoming = (root / "incoming").resolve()
            request_path = (incoming / identifier / "request.json").resolve()
            if not request_path.is_relative_to(incoming):
                raise ValueError("Request must be inside incoming directory")
            request = json.loads(request_path.read_text())
            if (
                set(request) != {"id", "backend_language", "frontend_commit", "github_actions_run_id"}
                or request["id"] != identifier
                or request["backend_language"] != "go"
                or not COMMIT.fullmatch(request["frontend_commit"])
                or not RUN_ID.fullmatch(request["github_actions_run_id"])
            ):
                raise ValueError("Invalid frontend-only release request")
            operations.deploy(request)
            result.update(status=operations.state["operation"]["status"], state=operations.state)
    except Exception as exc:
        result["error"] = str(exc)
        raise
    finally:
        server.write_json(result_path, result)


def result_status(result_path, request):
    try:
        result = json.loads(result_path.read_text())
    except FileNotFoundError:
        return None
    status = result.get("status")
    identifier = request["id"]
    reply = {"id": identifier, "status": status}
    if status == "failed":
        return reply
    if status not in ("success", "needs-verification"):
        raise ValueError("Invalid deployment result")

    state = result["state"]
    operation = state["operation"]
    if (
        operation.get("id") != identifier
        or operation.get("command") != "deploy"
        or operation.get("status") != status
    ):
        raise ValueError("Result belongs to a different operation")
    phase = "complete" if status == "success" else "awaiting-restore-verification"
    if operation.get("phase") == phase:
        reply["phase"] = phase
    backup = operation.get("backup")
    if (
        isinstance(backup, str)
        and BACKUP_ID.fullmatch(backup)
        and backup == state.get("last_backup")
    ):
        reply["last_backup"] = backup
    if status == "success":
        current = state["current"]
        backend = current.get("backend_commit")
        frontend = current.get("frontend_commit")
        expected_backend = request.get("backend_commit")
        expected_frontend = request.get("frontend_commit")
        if (
            current.get("id") != identifier
            or current.get("backend_language") != "go"
            or (expected_backend is not None and backend != expected_backend)
            or (expected_frontend is not None and frontend != expected_frontend)
        ):
            raise ValueError("Result does not match the requested release")
        if expected_backend is None or expected_frontend is None:
            component = "frontend" if expected_backend is None else "backend"
            retained_commit = backend if expected_backend is None else frontend
            if (
                not isinstance(retained_commit, str)
                or not COMMIT.fullmatch(retained_commit)
                or "phase" not in reply
                or "last_backup" not in reply
            ):
                raise ValueError(f"Incomplete {component}-only deployment result")
        reply.update(
            backend_commit=backend,
            frontend_commit=frontend,
        )
    return reply


def status(root, identifier, *, backend_only=False):
    request_path = root / "incoming" / identifier / "request.json"
    try:
        request_file = request_path.open()
    except FileNotFoundError:
        raise ValueError("Unknown GitHub Actions release") from None
    with request_file:
        request = json.load(request_file)
        has_frontend = "frontend_commit" in request
        if has_frontend == backend_only:
            raise ValueError("Unknown GitHub Actions release")
        if (
            request.get("id") != identifier
            or request.get("backend_language") != "go"
            or not RUN_ID.fullmatch(request.get("github_actions_run_id", ""))
            or (
                (backend_only or "backend_commit" in request)
                and not COMMIT.fullmatch(request.get("backend_commit", ""))
            )
            or (has_frontend and not COMMIT.fullmatch(request["frontend_commit"]))
        ):
            raise ValueError("Invalid GitHub Actions release request")
        try:
            fcntl.flock(request_file, fcntl.LOCK_SH | fcntl.LOCK_NB)
        except BlockingIOError:
            return {"id": identifier, "status": "running"}

    result = root / "results" / f"{identifier}.json"
    reply = result_status(result, request)
    if reply is not None:
        return reply
    unit = subprocess.run(
        [
            "/usr/bin/systemctl", "show", "--property=LoadState",
            "--property=ActiveState", unit_name(identifier),
        ],
        capture_output=True, text=True, timeout=15, check=False,
    )
    properties = dict(
        line.split("=", 1) for line in unit.stdout.splitlines() if "=" in line
    )
    active = properties.get("ActiveState")
    if unit.returncode == 0 and active in {
        "active", "activating", "deactivating", "reloading"
    }:
        reply = {"id": identifier, "status": "running"}
        operation = json.loads((root / "state.json").read_text()).get("operation") or {}
        phase = operation.get("phase")
        if operation.get("id") == identifier and isinstance(phase, str) and re.fullmatch(r"[a-z-]{1,48}", phase):
            reply["phase"] = phase
        return reply
    if properties.get("LoadState") != "not-found" and (
        unit.returncode or active not in {"failed", "inactive"}
    ):
        raise RuntimeError("Unable to determine deployment unit status")

    # A completed transient unit can disappear between the result read and systemctl.
    return result_status(result, request) or {"id": identifier, "status": "failed"}


def dispatch(command, stream, root, *, backend_only=False):
    if command == "probe":
        if backend_only:
            return {"protocol": 1, "scope": "backend"}
        return {"protocol": 1}
    fields = command.split(" ")
    if len(fields) >= 2 and server.RELEASE_ID.fullmatch(fields[1]):
        verb, identifier, *arguments = fields
        if verb == "status" and not arguments:
            return status(root, identifier, backend_only=backend_only)
        if backend_only and verb == "submit-backend" and len(arguments) == 2:
            backend, run_id = arguments
            if COMMIT.fullmatch(backend) and RUN_ID.fullmatch(run_id):
                return submit(root, stream, identifier, backend, None, run_id)
        if not backend_only and verb == "submit-frontend" and len(arguments) == 2:
            frontend, run_id = arguments
            if COMMIT.fullmatch(frontend) and RUN_ID.fullmatch(run_id):
                return submit(root, stream, identifier, None, frontend, run_id)
        if not backend_only and verb == "submit" and len(arguments) == 3:
            backend, frontend, run_id = arguments
            if (
                COMMIT.fullmatch(backend)
                and COMMIT.fullmatch(frontend)
                and RUN_ID.fullmatch(run_id)
            ):
                return submit(root, stream, identifier, backend, frontend, run_id)
    raise ValueError("Invalid GitHub deployment command")


def timeout(_signum, _frame):
    raise TimeoutError("GitHub deployment command deadline exceeded")


def main():
    args = sys.argv[1:]
    os.umask(0o077)
    if os.geteuid() != 0:
        raise RuntimeError("GitHub deployment gateway requires root")
    if args not in ([], ["--backend-only"]):
        if len(args) != 2 or args[0] != "--apply-frontend":
            raise ValueError("Invalid GitHub deployment gateway arguments")
        apply_frontend(ROOT, args[1])
        return
    signal.signal(signal.SIGALRM, timeout)
    signal.alarm(15 * 60)
    reply = dispatch(
        os.environ.get("SSH_ORIGINAL_COMMAND", ""), sys.stdin.buffer, ROOT,
        backend_only=args == ["--backend-only"],
    )
    print(json.dumps(reply))


if __name__ == "__main__":
    try:
        main()
    except Exception:
        print("GitHub deployment request failed", file=sys.stderr)
        raise SystemExit(1)
