"""Run inside a server container: show that it can reach nothing but its egress proxy."""

import socket
import sys

PROXY_HOST, PROXY_PORT = sys.argv[1], int(sys.argv[2])
ALLOWED_HOST = sys.argv[3]
HOST_PORTS = (22, 80, 443, 2375, 3000, 5000, 8080, 8123, 9000, 9090, 9443)
failed = False


def connects(host, port):
    try:
        with socket.create_connection((host, port), timeout=3):
            return True
    except OSError:
        return False


def proxy_connect(target):
    with socket.create_connection((PROXY_HOST, PROXY_PORT), timeout=10) as conn:
        conn.sendall(f"CONNECT {target}:443 HTTP/1.1\r\nHost: {target}:443\r\n\r\n".encode())
        return conn.recv(256).decode(errors="replace").split("\r\n", 1)[0]


def check(label, ok):
    global failed
    print(("OK   " if ok else "FAIL ") + label)
    failed = failed or not ok


check("no direct route to the internet", not connects("1.1.1.1", 443))

try:
    socket.getaddrinfo("example.com", 443)
    resolved = True
except OSError:
    resolved = False
check("outside names do not resolve", not resolved)

own = socket.gethostbyname(socket.gethostname())
gateway = own.rsplit(".", 1)[0] + ".1"
open_ports = [port for port in HOST_PORTS if connects(gateway, port)]
check(f"no host port answers on {gateway} ({open_ports or 'none'})", not open_ports)

status = proxy_connect("example.com")
check(f"proxy refuses a host off the list ({status})", " 200" not in status)
status = proxy_connect(ALLOWED_HOST)
check(f"proxy allows {ALLOWED_HOST} ({status})", " 200" in status)

sys.exit(1 if failed else 0)
