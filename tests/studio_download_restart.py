"""Exercise a real studio crash/restart and verified range-resume using local fixtures.

No upstream model or engine is downloaded or run. The fixture engine only marks
installation as satisfied; model bytes have a real SHA-256 checked by fornax.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.request import Request, urlopen


def wait_for(check, timeout=10):
    until = time.monotonic() + timeout
    while time.monotonic() < until:
        result = check()
        if result:
            return result
        time.sleep(.05)
    raise AssertionError("timed out waiting for studio/download state")


def run(binary):
    payload = b"fornax-resume-fixture\n" * 100000
    digest = hashlib.sha256(payload).hexdigest()
    ranges = []
    first = True

    class Fixture(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_HEAD(self):
            self.send_response(200)
            self.send_header("Content-Length", str(len(payload)))
            self.send_header("x-linked-size", str(len(payload)))
            self.send_header("x-linked-etag", digest)
            self.end_headers()

        def do_GET(self):
            nonlocal first
            if self.path.startswith("/api/models/"):
                body = b'{"siblings":[{"rfilename":"model-Q4_K_M.gguf"}]}'
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return
            header = self.headers.get("Range", "")
            start = int(header.removeprefix("bytes=").split("-")[0]) if header else 0
            ranges.append(start)
            self.send_response(206 if header else 200)
            self.send_header("Content-Length", str(len(payload)-start))
            if header:
                self.send_header("Content-Range", f"bytes {start}-{len(payload)-1}/{len(payload)}")
            self.end_headers()
            if first:
                first = False
                self.wfile.write(payload[:1048576])
                self.wfile.flush()
                # Release the stalled fixture when the resumed request arrives.
                while len(ranges) < 2:
                    time.sleep(.05)
                return
            self.wfile.write(payload[start:])

    fixture = ThreadingHTTPServer(("127.0.0.1", 0), Fixture)
    fixture.daemon_threads = True
    threading.Thread(target=fixture.serve_forever, daemon=True).start()
    with tempfile.TemporaryDirectory(prefix="fornax-restart-") as temp:
        home = Path(temp)
        engine = home / "engine/llama-cpu"
        engine.mkdir(parents=True)
        (engine / "llama-server").write_text("fixture engine: never executed")
        (engine / "installed").write_text("local test fixture")
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        base = f"http://127.0.0.1:{port}"
        task_env = {**os.environ, "FORNAX_HOME": str(home), "FORNAX_BACKEND": "cpu", "HF_ENDPOINT": f"http://127.0.0.1:{fixture.server_port}"}
        log = open(home / "studio.log", "wb")
        process = None
        children = []

        def start():
            return subprocess.Popen([str(binary), "studio", "-no-open", "-port", str(port)], env=task_env, stdout=log, stderr=log)

        def ready():
            try:
                return urlopen(base+"/healthz", timeout=.5).read() == b"fornax studio"
            except OSError:
                return False

        def api(path, body=None):
            key = (home / "server.key").read_text().strip()
            req = Request(base+path, data=json.dumps(body).encode() if body is not None else None,
                          headers={"Cookie": "fornax_studio="+key, "Content-Type": "application/json"})
            with urlopen(req, timeout=5) as response:
                return json.load(response)

        def part_file():
            return next((p for p in home.rglob("*.part") if p.stat().st_size >= 1048576), None)

        def child_stopped(pid):
            stat = Path(f"/proc/{pid}/stat")
            return not stat.exists() or stat.read_text().split()[2] == "Z"

        try:
            process = start()
            wait_for(ready)
            request = {"ref": "hf:org/Fixture/model-Q4_K_M.gguf"}
            api("/api/hub/downloads", request)
            partial = wait_for(part_file)
            old_size = partial.stat().st_size
            child_ids = subprocess.check_output(["ps", "--ppid", str(process.pid), "-o", "pid="], text=True).split()
            children = [int(pid) for pid in child_ids]
            assert children, "studio did not spawn a download child"
            process.kill()  # Deliberate crash, not the graceful shutdown path.
            process.wait(timeout=5)
            for pid in children:
                wait_for(lambda: child_stopped(pid), timeout=3)
            assert partial.stat().st_size == old_size
            process = start()
            wait_for(ready)
            downloads = json.loads((home / "studio/downloads.json").read_text())["downloads"]
            assert downloads[0]["state"] == "running"  # Disk records intent from before the crash.
            # The restored state is published to viewers as interrupted.
            with urlopen(Request(base+"/api/events", headers={"Cookie": "fornax_studio="+(home / "server.key").read_text().strip()}), timeout=5) as response:
                event = response.readline().decode()
                assert '"state":"interrupted"' in event, event
            api("/api/hub/downloads", request)

            def completed():
                entries = json.loads((home / "studio/downloads.json").read_text())["downloads"]
                return entries and entries[0]["state"] == "done"

            wait_for(completed)
            assert ranges[-1] == old_size, (ranges, old_size)
            files = list((home / "models").rglob("model-Q4_K_M.gguf"))
            assert len(files) == 1 and hashlib.sha256(files[0].read_bytes()).hexdigest() == digest
            assert len(api("/api/models")) == 1
            print("Studio crash/restart passed: orphan child stopped, partial bytes retained, Range resumed at saved offset, SHA-256 verified, installed model visible.")
        finally:
            if process and process.poll() is None:
                process.terminate()
                process.wait(timeout=5)
            for pid in children:
                if not child_stopped(pid):
                    os.kill(pid, signal.SIGKILL)
            log.close()
            fixture.shutdown()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, default=Path("./fornax"))
    args = parser.parse_args()
    if not Path("/proc/self/stat").exists():
        parser.error("this process-lifecycle check requires Linux /proc")
    run(args.binary.resolve())
