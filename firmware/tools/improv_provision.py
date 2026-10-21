#!/usr/bin/env python3
"""Send WiFi credentials to a node over Improv Serial.

Usage: improv_provision.py PORT SSID PASSWORD

Improv Serial packet: "IMPROV" | version(1) | type | length | data | checksum
type 0x03 = RPC; RPC command 0x01 = send WiFi settings.
Checksum = sum of all preceding bytes, mod 256.
https://www.improv-wifi.com/serial/
"""
import sys
import time

import serial  # pyserial (installed with esptool)

HEADER = b"IMPROV"
VERSION = 1
TYPE_STATE, TYPE_ERROR, TYPE_RPC, TYPE_RPC_RESULT = 0x01, 0x02, 0x03, 0x04
STATES = {0x02: "ready (no wifi)", 0x03: "provisioning", 0x04: "provisioned"}
ERRORS = {0x00: "none", 0x01: "invalid packet", 0x02: "unknown command", 0x03: "unable to connect", 0xFF: "unknown"}


def packet(ptype: int, data: bytes) -> bytes:
    body = HEADER + bytes([VERSION, ptype, len(data)]) + data
    return body + bytes([sum(body) & 0xFF])


def wifi_rpc(ssid: str, password: str) -> bytes:
    s, p = ssid.encode(), password.encode()
    payload = bytes([len(s)]) + s + bytes([len(p)]) + p
    return packet(TYPE_RPC, bytes([0x01, len(payload)]) + payload)


def read_packets(port, seconds: float):
    buf = b""
    end = time.time() + seconds
    while time.time() < end:
        buf += port.read(256)
        while True:
            i = buf.find(HEADER)
            if i < 0 or len(buf) < i + 9:
                break
            ptype, length = buf[i + 7], buf[i + 8]
            if len(buf) < i + 10 + length:
                break
            data = buf[i + 9 : i + 9 + length]
            buf = buf[i + 10 + length :]
            yield ptype, data


def main():
    if len(sys.argv) != 4:
        sys.exit(__doc__)
    port_name, ssid, password = sys.argv[1:]
    with serial.Serial(port_name, 115200, timeout=0.2) as port:
        port.dtr = False
        port.rts = False
        time.sleep(0.3)
        port.reset_input_buffer()
        port.write(wifi_rpc(ssid, password))
        print(f"sent credentials for {ssid!r}, waiting up to 40 s")
        for ptype, data in read_packets(port, 40):
            if ptype == TYPE_STATE:
                print("state:", STATES.get(data[0], hex(data[0])))
            elif ptype == TYPE_ERROR:
                if data[0] == 0x00:  # "no error", sent when state clears
                    continue
                print("error:", ERRORS.get(data[0], hex(data[0])))
                sys.exit(1)
            elif ptype == TYPE_RPC_RESULT:
                # data: command, total_len, then length-prefixed strings (redirect URL)
                urls, j = [], 2
                while j < len(data):
                    n = data[j]
                    urls.append(data[j + 1 : j + 1 + n].decode(errors="replace"))
                    j += 1 + n
                print("provisioned:", ", ".join(urls) or "(no URL)")
                return
        sys.exit("no result from device")


if __name__ == "__main__":
    main()
