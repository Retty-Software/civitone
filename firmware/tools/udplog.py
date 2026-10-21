#!/usr/bin/env python3
"""Tail a node's log over Wi-Fi, no USB.

Usage: udplog.py NODE_IP [--port 9997]

The firmware's udp_logger mirrors every ESP_LOGx line to UDP. This asks the
node (command port 9998) to send them to this machine only, prints them until
Ctrl-C, then hands the node back to broadcast. Needs to be on the node's LAN.
"""
import argparse
import re
import socket
import sys

CMD_PORT = 9998
ANSI = re.compile(r"\x1b\[[0-9;]*m")


def local_ip_for(node):
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.connect((node, CMD_PORT))  # no packet sent; picks the outgoing interface
    ip = s.getsockname()[0]
    s.close()
    return ip


def command(node, text):
    c = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    c.settimeout(2)
    c.sendto((text + "\n").encode(), (node, CMD_PORT))
    try:
        return c.recvfrom(256)[0].decode().strip()
    except socket.timeout:
        return None
    finally:
        c.close()


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("node")
    ap.add_argument("--port", type=int, default=9997, help="local port to receive on")
    args = ap.parse_args()

    rx = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    rx.bind(("", args.port))
    me = local_ip_for(args.node)
    reply = command(args.node, f"bind {me} {args.port}")
    if reply is None:
        sys.exit(f"{args.node} didn't answer on UDP {CMD_PORT}: wrong address, or not on this LAN")
    print(f"# {reply} -> {me}:{args.port} ({command(args.node, 'status')})", file=sys.stderr)
    try:
        while True:
            data = rx.recvfrom(4096)[0].decode(errors="replace")
            line = ANSI.sub("", data).rstrip()
            if line:
                print(line, flush=True)
    except KeyboardInterrupt:
        pass
    finally:
        command(args.node, "unbind")


if __name__ == "__main__":
    main()
