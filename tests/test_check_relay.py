"""Offline regression checks for the read-only operator preflight."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
import ssl
import subprocess
import shutil
from unittest import mock
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from threading import Thread

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("check_relay", ROOT / "scripts/check_relay.py")
assert SPEC is not None and SPEC.loader is not None
relay = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(relay)


class Admin(BaseHTTPRequestHandler):
    ready = 200
    accepting = 503
    info = {"peer_id": "QmExample", "addresses": [
        "/dns4/relay.example.com/tcp/4001/p2p/QmExample",
        "/dns4/relay.example.com/tcp/443/wss/p2p/QmExample"],
        "dms": {"session_present": True, "accepting_bookings": False, "draining": False}}

    def do_GET(self):
        if self.path == "/readyz":
            status, body = self.ready, b"ok"
        elif self.path == "/relay-info":
            status, body = 200, json.dumps(self.info).encode()
        elif self.path == "/accepting":
            status, body = self.accepting, b"private-secret-no-print"
        else:
            status, body = 302, b"redirect"
        self.send_response(status)
        self.send_header("Location", "http://not-visited.example/" if status == 302 else "/")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, format, *args):
        pass


class CheckTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.paths = []
        for name in ("registration", "wallet", "libp2p"):
            path = Path(self.tmp.name) / name
            path.write_bytes(b"NOT_A_REAL_KEY")
            path.chmod(0o600)
            self.paths.append(str(path))
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Admin)
        self.thread = Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.args = ["local", "--registration", self.paths[0], "--wallet", self.paths[1],
                     "--libp2p", self.paths[2], "--admin-port", str(self.server.server_port)]

    def run_check(self, args):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            result = relay.main(args)
        self.assertNotIn("NOT_A_REAL_KEY", out.getvalue())
        self.assertNotIn("private-secret-no-print", out.getvalue())
        return result, out.getvalue()

    def test_local_success_and_optional_booking_gate(self):
        code, text = self.run_check(self.args)
        self.assertEqual(code, 0)
        self.assertIn("NOT TESTED: booking gate", text)
        self.assertIn("NOT TESTED: public reachability", text)

    def test_missing_bad_permissions_and_symlink(self):
        Path(self.paths[0]).unlink()
        Path(self.paths[1]).chmod(0o644)
        Path(self.paths[2]).unlink()
        Path(self.paths[2]).symlink_to(self.paths[1])
        code, text = self.run_check(self.args)
        self.assertEqual(code, 1)
        self.assertEqual(text.count("FAIL: "), 3)

    def test_readiness_draining_and_bad_addresses(self):
        info = dict(Admin.info, addresses=["/dns4/relay.example.com/tcp/4001/p2p/Wrong"],
                    dms={"session_present": True, "draining": True})
        with mock.patch.object(Admin, "ready", 503), mock.patch.object(Admin, "info", info):
            code, text = self.run_check(self.args)
        self.assertEqual(code, 1)
        self.assertIn("FAIL: local readiness", text)
        self.assertIn("FAIL: DDS/DMS", text)
        self.assertIn("FAIL: advertised", text)

    def test_no_redirects(self):
        with mock.patch.object(Admin, "ready", 302):
            code, text = self.run_check(self.args)
        self.assertEqual(code, 1)
        self.assertIn("FAIL: local readiness", text)
        self.assertNotIn("not-visited.example", text)

    def test_public_contract_and_failures(self):
        args = ["public", "--host", "relay.example.com", "--tcp-port", "4001", "--wss-port", "443"]
        with mock.patch.object(relay, "ipv4_addresses", return_value=["8.8.8.8"]), \
             mock.patch.object(relay, "tcp_connect") as tcp, mock.patch.object(relay, "wss_upgrade") as wss:
            code, text = self.run_check(args)
            self.assertEqual(code, 0)
            self.assertIn("NOT TESTED: DDS reservation", text)
            tcp.assert_called_once_with("8.8.8.8", 4001)
            wss.assert_called_once_with("8.8.8.8", "relay.example.com", 443)
        with mock.patch.object(relay, "ipv4_addresses", return_value=["8.8.8.8"]), \
             mock.patch.object(relay, "tcp_connect", side_effect=relay.CheckFailed), \
             mock.patch.object(relay, "wss_upgrade", side_effect=relay.CheckFailed):
            code, text = self.run_check(args)
            self.assertEqual(code, 1)
            self.assertEqual(text.count("FAIL: "), 2)

    def test_dns_refusal_and_timeout_contract(self):
        with mock.patch.object(relay.socket, "getaddrinfo", return_value=[(None, None, None, None, ("127.0.0.1", 0))]):
            with self.assertRaises(relay.CheckFailed):
                relay.ipv4_addresses("relay.example.com")
        with mock.patch.object(relay.socket, "create_connection", side_effect=TimeoutError("secret URL")):
            with self.assertRaises(relay.CheckFailed):
                relay.tcp_connect("8.8.8.8", 4001)

    def test_wss_http200_wrong_accept_bad_tls_and_timeout(self):
        class Sock:
            response = b""
            def __enter__(self):
                return self
            def __exit__(self, *_):
                pass
            def settimeout(self, *_):
                pass
            def sendall(self, request):
                self.request = request
            def recv(self, *_):
                return self.response

        raw = Sock()
        tls = Sock()
        ctx = mock.Mock()
        ctx.wrap_socket.return_value = tls
        with mock.patch.object(relay.socket, "create_connection", return_value=raw), \
             mock.patch.object(relay.ssl, "create_default_context", return_value=ctx):
            for response in (b"HTTP/1.1 200 OK\r\n\r\n", b"HTTP/1.1 101 Switching Protocols\r\nSec-WebSocket-Accept: wrong\r\n\r\n"):
                tls.response = response
                with self.assertRaises(relay.CheckFailed):
                    relay.wss_upgrade("8.8.8.8", "relay.example.com", 443)
            import base64
            import hashlib
            key = b"the sample nonce"
            accept = base64.b64encode(hashlib.sha1(base64.b64encode(key) + b"258EAFA5-E914-47DA-95CA-C5AB0DC85B11").digest())
            tls.response = (b"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"
                            b"Connection: keep-alive, Upgrade\r\nSec-WebSocket-Accept: " + accept + b"\r\n\r\n")
            with mock.patch.object(relay.secrets, "token_bytes", return_value=key):
                relay.wss_upgrade("8.8.8.8", "relay.example.com", 443)
            ctx.wrap_socket.assert_called_with(raw, server_hostname="relay.example.com")
            valid_response = tls.response
            for bad_name in (b" Sec-WebSocket-Accept:", b"Sec-WebSocket-Accept :"):
                tls.response = valid_response.replace(b"Sec-WebSocket-Accept:", bad_name)
                with mock.patch.object(relay.secrets, "token_bytes", return_value=key), self.assertRaises(relay.CheckFailed):
                    relay.wss_upgrade("8.8.8.8", "relay.example.com", 443)
            ctx.wrap_socket.side_effect = relay.ssl.SSLError("secret URL")
            with self.assertRaises(relay.CheckFailed):
                relay.wss_upgrade("8.8.8.8", "relay.example.com", 443)
            ctx.wrap_socket.side_effect = TimeoutError("secret URL")
            with self.assertRaises(relay.CheckFailed):
                relay.wss_upgrade("8.8.8.8", "relay.example.com", 443)

    def test_upstream_owns_operator_guide_and_script(self):
        self.assertIn("docs/operator-manual.md", (ROOT / "README.md").read_text())
        guide = (ROOT / "docs/operator-manual.md").read_text()
        self.assertIn("../scripts/check_relay.py", guide)
        self.assertNotIn("/operators/relay/check_relay.py", guide)
        self.assertTrue((ROOT / "scripts/check_relay.py").is_file())

    def test_require_accepting_and_multiple_addresses(self):
        code, text = self.run_check(self.args + ["--require-accepting"])
        self.assertEqual(code, 1)
        self.assertIn("FAIL: booking gate", text)
        self.assertIn("Action:", text)
        with mock.patch.object(Admin, "accepting", 200), mock.patch.object(
            Admin, "info", dict(Admin.info, dms=dict(Admin.info["dms"], accepting_bookings=True))
        ):
            self.assertEqual(self.run_check(self.args + ["--require-accepting"])[0], 0)
        addresses = Admin.info["addresses"] + ["/dns4/backup.example.com/tcp/4443/wss/p2p/QmExample"]
        self.assertTrue(relay.valid_addresses(addresses, "QmExample"))
        for bad in ("/dns4/127.0.0.1/tcp/4001/p2p/QmExample",
                    "/dns4/relay.example.com/tcp/0/p2p/QmExample",
                    "/dns4/relay.test/tcp/4001/p2p/QmExample"):
            self.assertFalse(relay.valid_addresses([bad, addresses[1]], "QmExample"))

    @unittest.skipUnless(shutil.which("openssl"), "OpenSSL is required for the local TLS fixture")
    def test_real_tls_upgrade_and_certificate_rejection(self):
        import base64
        import hashlib
        directory = Path(self.tmp.name)
        cert, key = directory / "cert.pem", directory / "key.pem"
        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
                        "-keyout", str(key), "-out", str(cert), "-days", "1",
                        "-subj", "/CN=relay.example.com", "-addext",
                        "subjectAltName=DNS:relay.example.com"],
                       check=True, capture_output=True)

        class Upgrade(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"
            wrong_case = False
            def do_GET(self):
                value = base64.b64encode(hashlib.sha1(
                    (self.headers["Sec-WebSocket-Key"] +
                     "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode()
                self.send_response_only(101, "Upgrade")
                self.send_header("Upgrade", "WebSocket")
                self.send_header("Connection", "Upgrade")
                self.send_header("Sec-WebSocket-Accept", value.swapcase() if self.wrong_case else value)
                self.end_headers()
            def log_message(self, format, *args):
                pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), Upgrade)
        server_context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        server_context.load_cert_chain(cert, key)
        server.socket = server_context.wrap_socket(server.socket, server_side=True)
        Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        trusted = ssl.create_default_context(cafile=str(cert))
        with mock.patch.object(relay.ssl, "create_default_context", return_value=trusted):
            relay.wss_upgrade("127.0.0.1", "relay.example.com", server.server_port)
            with self.assertRaises(relay.CheckFailed):
                relay.wss_upgrade("127.0.0.1", "wrong.example.com", server.server_port)
            with mock.patch.object(Upgrade, "wrong_case", True), self.assertRaises(relay.CheckFailed):
                relay.wss_upgrade("127.0.0.1", "relay.example.com", server.server_port)
        untrusted = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        with mock.patch.object(relay.ssl, "create_default_context", return_value=untrusted):
            with self.assertRaises(relay.CheckFailed):
                relay.wss_upgrade("127.0.0.1", "relay.example.com", server.server_port)


if __name__ == "__main__":
    unittest.main()
