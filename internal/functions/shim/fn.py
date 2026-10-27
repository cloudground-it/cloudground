# Written by CloudGround: runs one invocation of a function (#143,
# internal/functions SPEC §4). systemd hands over the accepted connection on
# file descriptor 3; one request is read, the handler called, one answer
# written, and the process exits.
import asyncio
import base64
import datetime
import importlib.util
import inspect
import json
import os
import signal
import socket
import sys
import time
import traceback
from http.server import BaseHTTPRequestHandler
from urllib.parse import urlsplit

STARTED = time.monotonic()
NAME = os.environ.get("CG_FUNCTION", "")
TIMEOUT = max(0.25, int(os.environ.get("CG_TIMEOUT_MS", "9750") or 9750) / 1000.0)
MAX_BODY = 6 << 20


def log(status, error=None):
    line = {"time": datetime.datetime.now(datetime.timezone.utc).isoformat(), "status": status,
            "ms": int((time.monotonic() - STARTED) * 1000)}
    if error:
        line["error"] = str(error)[:500]
    sys.stdout.write(json.dumps(line) + "\n")
    sys.stdout.flush()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"
    def version_string(self):
        return "CloudGround-Function"
    sent = False

    def log_message(self, *args):  # nginx logs the request; stderr is for the handler
        pass

    def send(self, status, headers, body):
        if self.sent:
            return
        self.sent = True
        if isinstance(body, str):
            body = body.encode()
        self.send_response(status)
        for k, v in headers.items():
            self.send_header(k, str(v))
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(body)
        self.wfile.flush()

    def fail(self, status, message, err=None):
        log(status, err if err is not None else message)
        if err is not None:
            traceback.print_exception(err, file=sys.stderr)
        self.send(status, {"Content-Type": "text/plain; charset=utf-8"}, message + "\n")

    def answer(self, value):
        if isinstance(value, dict) and "statusCode" in value:
            status = int(value.get("statusCode") or 200)
            if status < 100 or status > 599:
                raise ValueError("statusCode must be 100 to 599")
            headers = {str(k): str(v) for k, v in (value.get("headers") or {}).items()}
            body = value.get("body")
            if body is None:
                body = ""
            if not isinstance(body, str):
                body = json.dumps(body)
            log(status)
            self.send(status, headers, base64.b64decode(body) if value.get("isBase64Encoded") else body)
        elif isinstance(value, str):
            log(200)
            self.send(200, {"Content-Type": "text/plain; charset=utf-8"}, value)
        else:
            log(200)
            self.send(200, {"Content-Type": "application/json"}, json.dumps(value))

    def handle_one(self):
        def on_timeout(signum, frame):
            self.fail(504, "the function took longer than its timeout")
            os._exit(0)

        signal.signal(signal.SIGALRM, on_timeout)
        signal.setitimer(signal.ITIMER_REAL, TIMEOUT)
        length = int(self.headers.get("Content-Length") or 0)
        if length > MAX_BODY:
            return self.fail(413, "the request body is larger than 6 MiB")
        raw = self.rfile.read(length) if length > 0 else b""
        url = urlsplit(self.path)
        prefix = "/.functions/" + NAME
        path = url.path[len(prefix):] if url.path.startswith(prefix) else url.path
        try:
            body, b64 = raw.decode("utf-8"), False
        except UnicodeDecodeError:
            body, b64 = base64.b64encode(raw).decode(), True
        headers = {}
        for k, v in self.headers.items():
            k = k.lower()
            headers[k] = headers[k] + ", " + v if k in headers else v
        src = headers.get("x-cloudground-source")
        event = {"method": self.command, "path": path or "/", "query": url.query, "headers": headers,
                 "body": body, "isBase64Encoded": b64,
                 "source": src if src in ("schedule", "test") else "http"}
        try:
            spec = importlib.util.spec_from_file_location("cg_function", os.environ.get("CG_HANDLER", "main.py"))
            mod = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(mod)
            fn = getattr(mod, "handler", None)
            if not callable(fn):
                raise RuntimeError("the code defines no handler function")
        except BaseException as err:
            return self.fail(500, "the function could not be loaded", err)
        try:
            value = asyncio.run(fn(event)) if inspect.iscoroutinefunction(fn) else fn(event)
            if inspect.isawaitable(value):
                value = asyncio.run(value)
            signal.setitimer(signal.ITIMER_REAL, 0)
            self.answer(value)
        except BaseException as err:
            signal.setitimer(signal.ITIMER_REAL, 0)
            self.fail(500, "the function failed", err)

    do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = do_HEAD = do_OPTIONS = handle_one


class Server:
    """What BaseHTTPRequestHandler asks of its server: nothing but its presence."""


def main():
    conn = socket.socket(fileno=3)
    try:
        Handler(conn, ("", 0), Server())
    finally:
        try:
            conn.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
        conn.close()


if __name__ == "__main__":
    main()
