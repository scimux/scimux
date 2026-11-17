#!/usr/bin/env python3
"""Exercise the synthetic native fixture through an actual pseudo-terminal."""
import json
import os
import pty
import select
import subprocess
import tempfile
import time
from pathlib import Path

fixture = Path(__file__).with_name('claude')
with tempfile.TemporaryDirectory() as home:
    settings = Path(home) / 'settings.json'
    settings.write_text(json.dumps({'hooks': {name: [{'hooks': [{'command': 'true'}]}]
                                              for name in ('SessionStart', 'Stop')}}))
    master, slave = pty.openpty()
    counter = str(Path(home) / 'counter')
    process = subprocess.Popen([str(fixture), '--session-id', 'synthetic', '--settings', str(settings)],
                               stdin=slave, stdout=slave, stderr=slave,
                               env={**os.environ, 'HOME': home, 'SCIMUX_SYNTHETIC_COUNTER': counter})
    os.close(slave)
    try:
        output = b''
        deadline = time.monotonic() + 8
        while b'Synthetic Claude ready' not in output and time.monotonic() < deadline:
            if select.select([master], [], [], .2)[0]:
                output += os.read(master, 4096)
        assert b'Synthetic Claude ready' in output, output
        text = '  quoted α\nsecond line  '
        os.write(master, b'\x1b[200~' + text.replace('\n', '\r').encode() + b'\x1b[201~\r')
        deadline = time.monotonic() + 8
        while not os.path.exists(counter) and time.monotonic() < deadline:
            time.sleep(.05)
        rows = [json.loads(line) for line in Path(counter).read_text().splitlines()]
        assert rows == [{'text': text}], rows
        attempts = [json.loads(line) for line in Path(counter + '.attempts').read_text().splitlines()]
        assert attempts == rows, attempts
        print('native PTY: exact UTF-8 whitespace and one submission: PASS')
    finally:
        process.terminate()
        process.wait(timeout=5)
        os.close(master)
