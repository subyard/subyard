#!/usr/bin/env python3
"""Bounded Linux/X11 single-process probe; run only in a leased disposable VM."""
import argparse
import json
import os
from pathlib import Path
import statistics
import subprocess
import time


def sample(pid):
    proc = Path('/proc') / str(pid)
    fields = (proc / 'stat').read_text().rsplit(')', 1)[1].split()
    memory = {}
    for line in (proc / 'smaps_rollup').read_text().splitlines():
        if line.startswith(('Rss:', 'Pss:')):
            key, value, _ = line.split()
            memory[key[:-1].lower() + '_kib'] = int(value)
    children = set()
    for thread in (proc / 'task').iterdir():
        children.update((thread / 'children').read_text().split())
    if children:
        raise RuntimeError('Single-process scope violated: child processes present')
    cpu = list(map(int, Path('/proc/stat').read_text().splitlines()[0].split()[1:9]))
    return dict(monotonic=time.monotonic(), ticks=int(fields[11]) + int(fields[12]),
                start_ticks=int(fields[19]), guest_cpu_total=sum(cpu),
                guest_cpu_idle=cpu[3], guest_cpu_steal=cpu[7],
                guest_cpu_pressure=Path('/proc/pressure/cpu').read_text().strip(),
                **memory)


def window_state(window, minimized):
    state = subprocess.check_output(['xprop', '-id', window, '_NET_WM_STATE'], text=True)
    info = subprocess.check_output(['xwininfo', '-id', window], text=True)
    if ('_NET_WM_STATE_HIDDEN' in state) != minimized:
        raise RuntimeError('Unexpected minimized state: ' + state)
    expected = 'IsUnMapped' if minimized else 'IsViewable'
    if 'Map State: ' + expected not in info:
        raise RuntimeError('Unexpected map state: ' + info)
    return state.strip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--pid', type=int, required=True)
    parser.add_argument('--window', required=True)
    parser.add_argument('--state', choices=['visible', 'minimized'], required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    minimized = args.state == 'minimized'
    window_state(args.window, minimized)
    time.sleep(30)
    first = sample(args.pid)
    rows = [first]
    for _ in range(120):
        time.sleep(1)
        row = sample(args.pid)
        if row['start_ticks'] != first['start_ticks']:
            raise RuntimeError('Process identity changed')
        rows.append(row)
    state = window_state(args.window, minimized)
    elapsed = rows[-1]['monotonic'] - first['monotonic']
    summary = dict(pid=args.pid, start_ticks=first['start_ticks'], state=args.state,
                   samples=len(rows) - 1, missing_samples=0, seconds=elapsed,
                   cpu_percent=100 * (rows[-1]['ticks'] - first['ticks']) /
                   os.sysconf('SC_CLK_TCK') / elapsed, window_state=state)
    guest_ticks = rows[-1]['guest_cpu_total'] - first['guest_cpu_total']
    for key in ['guest_cpu_idle', 'guest_cpu_steal']:
        summary[key + '_percent'] = 100 * (rows[-1][key] - first[key]) / guest_ticks
    for key in ['rss_kib', 'pss_kib']:
        values = sorted(row[key] / 1024 for row in rows[1:])
        summary[key.replace('_kib', '_mib')] = dict(
            median=statistics.median(values), p95=values[113], maximum=max(values))
    args.output.write_text(json.dumps(dict(summary=summary, samples=rows), indent=2) + '\n')
    print(json.dumps(summary), flush=True)


if __name__ == '__main__':
    main()
