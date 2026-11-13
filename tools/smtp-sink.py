#!/usr/bin/env python3
"""Tiny local SMTP sink for development: accepts any mail on 127.0.0.1:2525 and prints it."""
import asyncio

async def handle(reader, writer):
    async def send(line):
        writer.write((line + "\r\n").encode())
        await writer.drain()

    await send("220 sink ready")
    in_data = False
    lines = []
    while True:
        raw = await reader.readline()
        if not raw:
            break
        line = raw.decode(errors="replace").rstrip("\r\n")
        if in_data:
            if line == ".":
                in_data = False
                print("=" * 60)
                print("\n".join(lines))
                print("=" * 60, flush=True)
                lines = []
                await send("250 OK queued")
            else:
                lines.append(line[1:] if line.startswith("..") else line)
            continue
        cmd = line.upper()
        if cmd.startswith("EHLO"):
            writer.write(b"250-sink\r\n250 8BITMIME\r\n")
            await writer.drain()
        elif cmd.startswith("DATA"):
            in_data = True
            await send("354 end with <CRLF>.<CRLF>")
        elif cmd.startswith("QUIT"):
            await send("221 bye")
            break
        else:
            await send("250 OK")
    writer.close()

async def main():
    server = await asyncio.start_server(handle, "127.0.0.1", 2525)
    print("SMTP sink listening on 127.0.0.1:2525", flush=True)
    async with server:
        await server.serve_forever()

asyncio.run(main())
