#!/usr/bin/python3
"""Fixed read-only NVIDIA query, atomic node_exporter textfile output."""
import csv
import io
import math
import os
from pathlib import Path
import subprocess
import tempfile
import time

OUTPUT = Path('/var/lib/gpu-exporter/gpu.prom')
ARGV = ['/usr/bin/nvidia-smi',
        '--query-gpu=uuid,name,utilization.gpu,utilization.encoder,utilization.decoder,memory.used,memory.total',
        '--format=csv,noheader,nounits']
NAMES = ['plex_gpu_util_pct', 'plex_gpu_encoder_pct', 'plex_gpu_decoder_pct',
         'plex_gpu_mem_used_bytes', 'plex_gpu_mem_total_bytes']


def label(value):
    return value.replace('\\', '\\\\').replace('\n', '\\n').replace('"', '\\"')


def metrics(raw, sampled):
    rows = list(csv.reader(io.StringIO(raw), skipinitialspace=True))
    if not rows:
        raise ValueError('no devices')
    lines, ids = [], set()
    for row in rows:
        if len(row) != 7:
            raise ValueError('invalid GPU row')
        uuid, name = (part.strip() for part in row[:2])
        if not uuid or uuid in ids:
            raise ValueError('invalid GPU UUID')
        ids.add(uuid)
        labels = 'uuid="' + label(uuid) + '",name="' + label(name) + '"'
        lines.append('plex_gpu_sample_timestamp_seconds{' + labels + '} ' + str(sampled))
        for index, (metric, value) in enumerate(zip(NAMES, row[2:])):
            # Unsupported metrics are absent, never substituted with idle zeros.
            if value.strip() in ('N/A', '[N/A]', 'Not Supported', '[Not Supported]'):
                continue
            number = float(value)
            if not math.isfinite(number) or number < 0 or (index < 3 and number > 100):
                raise ValueError('invalid GPU value')
            if index >= 3:
                number *= 1048576
                if number != int(number):
                    raise ValueError('invalid GPU memory')
            lines.append(metric + '{' + labels + '} ' + str(number))
    return '\n'.join(lines) + '\n'


def write_atomic(path, text):
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(mode='w', dir=path.parent,
                                         prefix='.gpu-', suffix='.tmp', delete=False) as stream:
            temporary = stream.name
            stream.write(text)
            stream.flush()
            os.fchmod(stream.fileno(), 0o644)
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        temporary = None
    finally:
        if temporary is not None:
            os.unlink(temporary)


def main():
    # Ignore all caller arguments; argv and output path are fixed above.
    try:
        result = subprocess.run(ARGV, check=True, capture_output=True, text=True,
                                timeout=3, env={'PATH': '/usr/bin:/bin', 'LC_ALL': 'C'})
        text = metrics(result.stdout, time.time())
    except (subprocess.SubprocessError, OSError, ValueError):
        # Erase exported measurements on failure. The client then reports
        # unavailable immediately rather than retaining a valid-looking idle GPU.
        write_atomic(OUTPUT, '# GPU query unavailable\n')
        return 1
    write_atomic(OUTPUT, text)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
