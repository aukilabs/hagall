#!/usr/bin/env python3
"""Read-only relay preflight. Python 3.10+, standard library only.

Run `python3 check_relay.py --help`. No credentials are opened or transmitted.
"""
import argparse
import base64
import hashlib
import http.client
import ipaddress
import json
import os
import re
import secrets
import socket
import ssl
import stat
import sys
import time

TIMEOUT = 4
LIMIT = 16384
PEER = re.compile(r"^[A-Za-z0-9]+$")
HOST = re.compile(r"^(?=.{1,253}$)[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)+$")


class CheckFailed(Exception):
    pass


def report(label, status):
    print(f"{status}: {label}")
    if status == "FAIL":
        if "metadata" in label:
            hint = "Check the file path, owner UID, size and private permissions. Reject symbolic links."
        elif "readiness" in label:
            hint = "Check relay startup and the private admin port in the relay network namespace."
        elif "Peer ID" in label:
            hint = "Check both advertised transports, DNS names, ports and the same persistent Peer ID."
        elif "session" in label or "gate" in label:
            hint = "Check DDS enrollment, DMS connectivity, drain state and the configured booking gate."
        elif "DNS" in label:
            hint = "Check that the hostname resolves to public IPv4 addresses."
        elif "TLS" in label:
            hint = "Check the certificate, hostname, TLS listener and WebSocket Upgrade forwarding."
        else:
            hint = "Check the advertised port, listener, firewall and NAT forwarding from an external machine."
        print(f"  Action: {hint}")
    return status == "FAIL"


def metadata(path, uid):
    try:
        info = os.lstat(path)
        if not stat.S_ISREG(info.st_mode) or info.st_size < 1 or info.st_size > 65536:
            raise CheckFailed()
        if info.st_uid != uid or info.st_mode & 0o077 or not info.st_mode & 0o400:
            raise CheckFailed()
    except (OSError, CheckFailed):
        return False
    return True


def admin_get(port, path):
    # Fixed loopback origin, fixed paths, no URL parser, proxy, redirects or body logging.
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=TIMEOUT)
    try:
        conn.request("GET", path)
        response = conn.getresponse()
        body = response.read(LIMIT + 1)
        if len(body) > LIMIT:
            raise CheckFailed()
        return response.status, body
    except (OSError, http.client.HTTPException, TimeoutError):
        raise CheckFailed() from None
    finally:
        conn.close()


def valid_addresses(addresses, peer):
    if not isinstance(addresses, list) or not addresses or not isinstance(peer, str) or not PEER.fullmatch(peer):
        return False
    transports = set()
    seen = set()
    for address in addresses:
        if not isinstance(address, str) or address in seen:
            return False
        seen.add(address)
        match = re.fullmatch(r"/dns4/([^/]+)/tcp/([0-9]+)(/wss)?/p2p/" + re.escape(peer), address)
        if not match or not HOST.fullmatch(match[1]) or not 1 <= int(match[2]) <= 65535:
            return False
        if match[1].endswith((".local", ".test", ".localhost")) or ip_literal(match[1]):
            return False
        transports.add(bool(match[3]))
    return transports == {False, True}


def ip_literal(host):
    try:
        ipaddress.ip_address(host)
        return True
    except ValueError:
        return False


def local(args):
    failed = False
    for label, path in (("registration file metadata", args.registration),
                        ("wallet file metadata", args.wallet), ("libp2p file metadata", args.libp2p)):
        failed |= report(label, "PASS" if metadata(path, args.uid) else "FAIL")
    try:
        ready, _ = admin_get(args.admin_port, "/readyz")
        ready_ok = ready == 200
    except CheckFailed:
        ready_ok = False
    failed |= report("local readiness", "PASS" if ready_ok else "FAIL")
    dms = None
    try:
        status, body = admin_get(args.admin_port, "/relay-info")
        info = json.loads(body) if status == 200 else None
        dms = info.get("dms") if isinstance(info, dict) else None
        addr_ok = isinstance(info, dict) and valid_addresses(info.get("addresses"), info.get("peer_id"))
        session_ok = isinstance(dms, dict) and dms.get("session_present") is True and dms.get("draining") is False
    except (CheckFailed, ValueError, TypeError):
        addr_ok = session_ok = False
    failed |= report("advertised TCP and WSS Peer ID", "PASS" if addr_ok else "FAIL")
    failed |= report("DDS/DMS session present and not draining", "PASS" if session_ok else "FAIL")
    try:
        accepting, _ = admin_get(args.admin_port, "/accepting")
        accept_ok = accepting == 200 and dms.get("accepting_bookings") is True if isinstance(dms, dict) else False
    except CheckFailed:
        accept_ok = False
    gate_status = "PASS" if accept_ok else ("FAIL" if args.require_accepting else "NOT TESTED")
    failed |= report("booking gate (optional until public checks pass)", gate_status)
    report("public reachability, booked circuit and payment", "NOT TESTED")
    return 1 if failed else 0


def ipv4_addresses(host):
    try:
        addresses = sorted({entry[4][0] for entry in socket.getaddrinfo(host, None, socket.AF_INET, socket.SOCK_STREAM)})
        if not addresses or any(not ipaddress.ip_address(a).is_global for a in addresses):
            raise CheckFailed()
        return addresses
    except (OSError, ValueError):
        raise CheckFailed() from None


def tcp_connect(address, port):
    try:
        with socket.create_connection((address, port), timeout=TIMEOUT):
            return
    except OSError:
        raise CheckFailed() from None


def wss_upgrade(address, host, port):
    key = base64.b64encode(secrets.token_bytes(16)).decode("ascii")
    expected = base64.b64encode(hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode("ascii")).digest()).decode("ascii")
    try:
        with socket.create_connection((address, port), timeout=TIMEOUT) as raw:
            with ssl.create_default_context().wrap_socket(raw, server_hostname=host) as tls:
                deadline = time.monotonic() + TIMEOUT
                request = (f"GET / HTTP/1.1\r\nHost: {host}:{port}\r\nUpgrade: websocket\r\n"
                           f"Connection: Upgrade\r\nSec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n")
                tls.sendall(request.encode("ascii"))
                data = bytearray()
                while b"\r\n\r\n" not in data and len(data) < LIMIT:
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        raise CheckFailed()
                    tls.settimeout(remaining)
                    chunk = tls.recv(min(1024, LIMIT - len(data)))
                    if not chunk:
                        raise CheckFailed()
                    data.extend(chunk)
                if b"\r\n\r\n" not in data:
                    raise CheckFailed()
                header = bytes(data).split(b"\r\n\r\n", 1)[0]
                lines = header.split(b"\r\n")
                if not lines or lines[0].split(b" ", 2)[:2] != [b"HTTP/1.1", b"101"]:
                    raise CheckFailed()
                fields = {}
                for line in lines[1:]:
                    name, sep, value = line.partition(b":")
                    if not sep or not re.fullmatch(rb"[!#$%&'*+.^_`|~0-9A-Za-z-]+", name):
                        raise CheckFailed()
                    name = name.lower()
                    if name in fields:
                        raise CheckFailed()
                    fields[name] = value.strip()
                if (fields.get(b"sec-websocket-accept") != expected.encode("ascii")
                        or fields.get(b"upgrade", b"").lower() != b"websocket"
                        or b"upgrade" not in [part.strip().lower() for part in fields.get(b"connection", b"").split(b",")]):
                    raise CheckFailed()
    except (OSError, ssl.SSLError, ValueError):
        raise CheckFailed() from None


def public(args):
    failed = False
    try:
        addresses = ipv4_addresses(args.host)
        report("public IPv4 DNS", "PASS")
    except CheckFailed:
        addresses = []
        failed |= report("public IPv4 DNS", "FAIL")
    for label, port, websocket in (("native TCP connection (not Noise or circuit)", args.tcp_port, False),
                                   ("verified TLS and WSS upgrade", args.wss_port, True)):
        if not addresses:
            failed |= report(label, "FAIL")
            continue
        try:
            for addr in addresses:
                if websocket:
                    wss_upgrade(addr, args.host, port)
                else:
                    tcp_connect(addr, port)
            report(label, "PASS")
        except CheckFailed:
            failed |= report(label, "FAIL")
    report("DDS reservation, authorized booked circuit and payment", "NOT TESTED")
    return 1 if failed else 0


def port(value):
    number = int(value)
    if not 1 <= number <= 65535:
        raise argparse.ArgumentTypeError("port must be 1..65535")
    return number


def main(argv=None):
    parser = argparse.ArgumentParser(description="Read-only relay preflight. No credentials are read.")
    sub = parser.add_subparsers(dest="scope", required=True)
    loc = sub.add_parser("local", help="Run on relay host; admin binds to 127.0.0.1 only")
    for name in ("registration", "wallet", "libp2p"):
        loc.add_argument("--" + name, required=True, help="identity file path; metadata only")
    loc.add_argument("--uid", type=int, default=os.getuid(), help="expected file owner UID (default: current UID)")
    loc.add_argument("--admin-port", type=port, default=9090, help="loopback admin port")
    loc.add_argument("--require-accepting", action="store_true", help="fail if the booking gate is not accepting")
    pub = sub.add_parser("public", help="Run from a separate external machine")
    pub.add_argument("--host", required=True, help="lowercase public DNS name")
    pub.add_argument("--tcp-port", required=True, type=port)
    pub.add_argument("--wss-port", required=True, type=port)
    args = parser.parse_args(argv)
    if args.scope == "public" and (not HOST.fullmatch(args.host) or ip_literal(args.host) or args.host.endswith((".local", ".test", ".localhost"))):
        parser.error("host must be a lowercase public DNS name")
    if args.scope == "local" and args.uid < 0:
        parser.error("uid must be nonnegative")
    return local(args) if args.scope == "local" else public(args)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (KeyboardInterrupt, Exception):
        # Never print exception strings: socket and TLS errors may include host names.
        print("FAIL: preflight interrupted or unavailable")
        sys.exit(1)
