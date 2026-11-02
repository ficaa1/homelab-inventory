import importlib.util
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('exporter', Path(__file__).with_name('gpu-exporter.py'))
exporter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(exporter)


class ExporterTests(unittest.TestCase):
    def test_conversion_unsupported_and_escaping(self):
        text = exporter.metrics('GPU-1, "A ""GPU""", 0, N/A, 5, 5, 4096\n', 123)
        self.assertIn('plex_gpu_mem_used_bytes{uuid="GPU-1",name="A \\"GPU\\""} 5242880.0', text)
        self.assertNotIn('plex_gpu_encoder_pct', text)
        self.assertIn('plex_gpu_util_pct', text)

    def test_invalid_and_duplicate(self):
        for raw in ['', 'bad', 'GPU-1, A, NaN, 0, 0, 1, 2',
                    'GPU-1, A, 101, 0, 0, 1, 2',
                    'GPU-1, A, 0, 0, 0, 1, 2\nGPU-1, A, 0, 0, 0, 1, 2']:
            with self.assertRaises(ValueError):
                exporter.metrics(raw, 123)

    def test_atomic_replacement(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'metrics.prom'
            exporter.write_atomic(path, 'old')
            exporter.write_atomic(path, 'new')
            self.assertEqual(path.read_text(), 'new')
            self.assertEqual(path.stat().st_mode & 0o777, 0o644)
            self.assertEqual(list(Path(directory).iterdir()), [path])


if __name__ == '__main__':
    unittest.main()
