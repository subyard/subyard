#!/usr/bin/env python3
"""Test-only observation of the existing viewer relay; never retain media payloads."""
import importlib.util
import contextlib
import json
import os
from pathlib import Path
import re
import select
import signal
import struct
import subprocess
import sys
import threading
import time
import zlib


def framebuffer_observation(environment):
    """One post-child read on the still-owned lease; discard image bytes."""
    result = dict(status='unavailable', bytes=0, width=0, height=0,
                  started_at_ms=time.time_ns() // 1000000, finished_at_ms=0,
                  memory_total_bytes=-1, memory_available_bytes=-1, swap_used_bytes=-1)
    # Observer-visible guest counters, not the physical pool owner's memory.
    try:
        memory = dict((key, int(number) * 1024) for key, number in re.findall(
            r'^(MemTotal|MemAvailable|SwapTotal|SwapFree):\s+([0-9]{1,15}) kB$', Path('/proc/meminfo').read_text(), re.M))
        result.update(memory_total_bytes=memory.get('MemTotal', -1), memory_available_bytes=memory.get('MemAvailable', -1),
                      swap_used_bytes=memory['SwapTotal'] - memory['SwapFree'] if {'SwapTotal', 'SwapFree'} <= set(memory) else -1)
    except (OSError, ValueError):
        pass
    process = None
    header, tail = b'', b''
    deadline = time.monotonic() + 10
    try:
        process = subprocess.Popen(
            ['/srv/cache/android-sdk/platform-tools/adb', 'exec-out', 'screencap', '-p'],
            env=dict(os.environ, **environment), stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, start_new_session=True)
        result['status'] = 'timeout'
        while time.monotonic() < deadline:
            if not select.select([process.stdout], [], [], max(0, deadline - time.monotonic()))[0]:
                break
            data = os.read(process.stdout.fileno(), 65536)
            if not data:
                code = process.wait(timeout=max(0.01, deadline - time.monotonic()))
                result['status'] = 'command_failed' if code else 'malformed'
                if (not code and result['bytes'] > 45 and len(header) == 33
                        and header[:16] == b'\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR'
                        and zlib.crc32(header[12:29]) == int.from_bytes(header[29:33], 'big')
                        and tail == b'\x00\x00\x00\x00IEND\xaeB`\x82'):
                    width, height = struct.unpack('>II', header[16:24])
                    if 1 <= width <= 8192 and 1 <= height <= 8192:
                        result.update(status='png_complete', width=width, height=height)
                break
            result['bytes'] += len(data)
            if result['bytes'] > 16 * 1024 * 1024:
                result['status'] = 'limit'
                break
            header += data[:max(0, 33 - len(header))]
            tail = (tail + data)[-12:]
    except subprocess.TimeoutExpired:
        result['status'] = 'timeout'
    except (OSError, ValueError):
        result['status'] = 'unavailable'
    finally:
        if process is not None:
            if process.poll() is None:
                with contextlib.suppress(ProcessLookupError):
                    os.killpg(process.pid, signal.SIGKILL)
            try:
                process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                result['status'] = 'timeout'
            process.stdout.close()
        result['finished_at_ms'] = time.time_ns() // 1000000
    return result


class VideoObservation:
    def __init__(self):
        self.header = bytearray()
        self.remaining = 64  # scrcpy device metadata, after the existing dummy byte
        self.stage = 'device'
        self.metadata = 'pending'
        self.frames = 0
        self.configuration = False
        self.dimensions = None
        self.timing = None

    def observe_context(self, values):
        # Only public numeric allocation identity; never retain endpoints or tokens.
        fields = ('SUBYARD_EMU_SLOT', 'SUBYARD_EMU_GENERATION', 'SUBYARD_EMU_API')
        if not all(isinstance(values.get(key), str) and re.fullmatch(r'[0-9]{1,10}', values[key]) for key in fields):
            return
        slot, generation, api = (int(values[key]) for key in fields)
        if 1 <= slot <= 999 and 1 <= generation <= 2147483647 and 1 <= api <= 99:
            self.timing = dict(slot=slot, generation=generation, api=api,
                               context_at_ms=time.time_ns() // 1000000,
                               metadata_at_ms=0, first_frame_at_ms=0, finished_at_ms=0)

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
                        if self.timing is not None:
                            self.timing['metadata_at_ms'] = time.time_ns() // 1000000
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
                        if self.timing is not None:
                            self.timing['first_frame_at_ms'] = time.time_ns() // 1000000
                        self.emit('running', 'unknown', 'unknown')
                self.stage, self.remaining = 'header', 12

    def emit(self, result, child_exit, relay):
        print('android-capture ' + json.dumps(dict(metadata=self.metadata,
              complete_frames=self.frames, stream='malformed' if self.stage == 'malformed' else 'observed',
              result=result, child_exit=child_exit, relay=relay), sort_keys=True), flush=True)

    def finish(self):
        if self.timing is not None:
            self.timing['finished_at_ms'] = time.time_ns() // 1000000
            print('android-capture-timing ' + json.dumps(self.timing, sort_keys=True), flush=True)


class ObservingSocket:
    def __init__(self, stream, observation):
        self.stream, self.observation = stream, observation

    def __getattr__(self, name):
        return getattr(self.stream, name)

    def sendall(self, data):
        self.stream.sendall(data)
        self.observation.feed(data)


NATIVE_HELPER = '/opt/subyard-e2e-native-debug.py'
WINDOW_HELPER = '/opt/subyard-e2e-view-window.py'


def native_popen(original, command, *args, **kwargs):
    # The production client selects scrcpy through its existing pinned PATH.
    if isinstance(command, (list, tuple)) and command and command[0] == 'scrcpy':
        command = [sys.executable, NATIVE_HELPER, '--gdb', '/usr/bin/gdb', '--', *command]
    return original(command, *args, **kwargs)


def window_popen(original, command, *args, **kwargs):
    if isinstance(command, (list, tuple)) and command and command[0] == 'scrcpy':
        command = [sys.executable, WINDOW_HELPER, '--', '/opt/subyard-e2e-scrcpy/scrcpy', *command[1:]]
    return original(command, *args, **kwargs)


def main():
    native_debug = sys.argv[1:2] == ['--native-debug']
    arguments = sys.argv[2:] if native_debug else sys.argv[1:]
    spec = importlib.util.spec_from_file_location('android_client', '/usr/local/lib/subyard-android/client.py')
    client = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(client)
    observation = VideoObservation()
    original_start, original_bridge = client.start_viewer_relay, client.bridge
    original_context = client.context
    original_relay, original_rpc = client.start_relay, client.rpc
    video, relays = [], []
    selected_token, environment, observed_framebuffer = None, None, False
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

    def context(*args):
        nonlocal environment
        values = original_context(*args)
        environment = values
        observation.observe_context(values)
        return values

    def relay(token, *args):
        nonlocal selected_token
        selected_token = token
        return original_relay(token, *args)

    def rpc(operation, **values):
        nonlocal observed_framebuffer
        if (operation == 'release' and selected_token is not None and values.get('token') == selected_token
                and environment is not None and observation.metadata == 'complete'
                and observation.frames == 0 and not observed_framebuffer):
            observed_framebuffer = True
            try:
                evidence = framebuffer_observation(environment)
                print('android-framebuffer ' + json.dumps(evidence, sort_keys=True), flush=True)
            except Exception:
                # Observation must never prevent the original owned lease release.
                pass
            finally:
                result = original_rpc(operation, **values)
            return result
        return original_rpc(operation, **values)

    client.start_viewer_relay, client.bridge, client.context = start, bridge, context
    client.start_relay, client.rpc = relay, rpc
    original_popen = client.subprocess.Popen
    wrap = native_popen if native_debug else window_popen
    client.subprocess.Popen = lambda command, *args, **kwargs: wrap(original_popen, command, *args, **kwargs)
    result, child_exit = 'failed', 'unknown'
    try:
        child_exit = client.main(arguments)
        result = 'passed' if child_exit == 0 else 'failed'
        return child_exit
    except client.Error as exc:
        return 3 if exc.code == 'busy' else 4 if exc.code == 'timeout' else 1
    except (OSError, ValueError):
        return 1
    finally:
        client.subprocess.Popen = original_popen
        failure = relays[0].failure if relays else None
        match = re.match(r'^(forward lookup|server socket|device transport|initial media byte) after ', failure or '')
        relay = match[1].replace(' ', '_') if match else 'unknown' if failure or not relays else 'no_failure_observed'
        observation.emit(result, child_exit, relay)
        observation.finish()


if __name__ == '__main__':
    raise SystemExit(main())
