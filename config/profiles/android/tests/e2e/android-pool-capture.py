#!/usr/bin/env python3
"""Test-only observation of the existing viewer relay; never retain media payloads."""
import importlib.util
import json
import re
import struct
import sys
import threading


class VideoObservation:
    def __init__(self):
        self.header = bytearray()
        self.remaining = 64  # scrcpy device metadata, after the existing dummy byte
        self.stage = 'device'
        self.metadata = 'pending'
        self.frames = 0
        self.configuration = False
        self.dimensions = None

    def feed(self, data):
        while data and self.stage != 'malformed':
            size = min(len(data), self.remaining)
            if self.stage in ('codec', 'header'):
                self.header.extend(data[:size])
            data = data[size:]
            self.remaining -= size
            if self.remaining:
                continue
            if self.stage == 'device':
                self.stage, self.remaining = 'codec', 4
            elif self.stage == 'codec':
                codec = bytes(self.header)
                self.header.clear()
                if codec not in (b'h264', b'h265', b'\x00av1'):
                    self.stage, self.metadata = 'malformed', 'malformed'
                    continue
                self.stage, self.remaining = 'header', 12
            elif self.stage == 'header':
                # Pinned scrcpy v4.1: codec ID is four bytes; session headers
                # carry flags/width/height, media headers carry flags+PTS/size.
                # https://github.com/Genymobile/scrcpy/blob/v4.1/app/src/demuxer.c
                header = bytes(self.header)
                self.header.clear()
                if header[0] & 0x80:
                    flags, width, height = struct.unpack('>III', header)
                    if flags not in (0x80000000, 0x80000001) or not (1 <= width <= 8192 and 1 <= height <= 8192):
                        self.stage, self.metadata = 'malformed', 'malformed'
                        continue
                    self.dimensions = (width, height)
                    first_session = self.metadata == 'pending'
                    self.metadata = 'complete'
                    self.remaining = 12
                    if first_session:
                        self.emit('running', 'unknown', 'unknown')
                    continue
                timestamp, length = struct.unpack('>QI', header)
                if self.metadata != 'complete' or not 1 <= length <= 16 * 1024 * 1024:
                    self.stage = 'malformed'
                    continue
                self.configuration = bool(timestamp & (1 << 62))
                self.stage, self.remaining = 'payload', length
            else:
                if not self.configuration:
                    self.frames += 1
                    if self.frames == 1:
                        self.emit('running', 'unknown', 'unknown')
                self.stage, self.remaining = 'header', 12

    def emit(self, result, child_exit, relay):
        print('android-capture ' + json.dumps(dict(metadata=self.metadata,
              complete_frames=self.frames, stream='malformed' if self.stage == 'malformed' else 'observed',
              result=result, child_exit=child_exit, relay=relay), sort_keys=True), flush=True)


class ObservingSocket:
    def __init__(self, stream, observation):
        self.stream, self.observation = stream, observation

    def __getattr__(self, name):
        return getattr(self.stream, name)

    def sendall(self, data):
        self.stream.sendall(data)
        self.observation.feed(data)


def main():
    spec = importlib.util.spec_from_file_location('android_client', '/usr/local/lib/subyard-android/client.py')
    client = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(client)
    observation = VideoObservation()
    original_start, original_bridge = client.start_viewer_relay, client.bridge
    video, relays = [], []
    lock = threading.Lock()

    def start(*args):
        server = original_start(*args)
        relays.append(server)
        original_request = server.process_request

        def process_request(request, address):
            with lock:
                if not video:
                    video.append(request)
            return original_request(request, address)
        server.process_request = process_request
        return server

    def bridge(local, channel):
        if video and local is video[0]:
            local = ObservingSocket(local, observation)
        return original_bridge(local, channel)

    client.start_viewer_relay, client.bridge = start, bridge
    result, child_exit = 'failed', 'unknown'
    try:
        child_exit = client.main(sys.argv[1:])
        result = 'passed' if child_exit == 0 else 'failed'
        return child_exit
    except client.Error as exc:
        return 3 if exc.code == 'busy' else 4 if exc.code == 'timeout' else 1
    except (OSError, ValueError):
        return 1
    finally:
        failure = relays[0].failure if relays else None
        match = re.match(r'^(forward lookup|server socket|device transport|initial media byte) after ', failure or '')
        relay = match[1].replace(' ', '_') if match else 'unknown' if failure or not relays else 'no_failure_observed'
        observation.emit(result, child_exit, relay)


if __name__ == '__main__':
    raise SystemExit(main())
