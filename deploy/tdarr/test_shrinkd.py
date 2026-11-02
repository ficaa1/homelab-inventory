"""Offline protocol regressions; subprocess execution is always mocked."""
import importlib.util
import io
import os
import contextlib
import tempfile
import json
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("shrinkd", Path(__file__).with_name("tdarr-shrinkd.py"))
shrinkd = importlib.util.module_from_spec(spec)
with patch.dict(os.environ, {}, clear=True), patch("builtins.open", side_effect=FileNotFoundError):
    spec.loader.exec_module(shrinkd)


class ValidationTests(unittest.TestCase):
    def request(self, result, paths=None, route="/validate", extra=None, auth="Bearer fixture", raw=None):
        handler = shrinkd.Handler.__new__(shrinkd.Handler)
        handler.path = route
        body = raw if raw is not None else json.dumps({"paths": paths or ["/media/series/Safe"], **(extra or {})}).encode()
        handler.headers = {"Authorization": auth, "Content-Length": str(len(body))}
        handler.rfile = io.BytesIO(body)
        replies = []
        handler._reply = lambda code, obj: replies.append((code, obj))
        with patch.object(shrinkd, "read_token", return_value="fixture"), patch.object(shrinkd.subprocess, "run", return_value=result) as run:
            handler.do_POST()
            return replies[0], run.call_args

    def test_read_only_command_and_expanded_paths(self):
        result = SimpleNamespace(returncode=0, stdout='{"paths":["/media/series/Safe/a.mkv"]}')
        reply, call = self.request(result)
        self.assertEqual(reply, (200, {"paths": ["/media/series/Safe/a.mkv"]}))
        self.assertEqual(call.args[0], ["sudo", "-n", shrinkd.SCRIPT, "--validate-only", "/media/series/Safe"])

    def test_fail_closed(self):
        for result in [SimpleNamespace(returncode=1), SimpleNamespace(returncode=0, stdout="garbage"),
                       SimpleNamespace(returncode=0, stdout=json.dumps({"paths": ["x"] * 17}))]:
            reply, _ = self.request(result)
            self.assertEqual(reply[0], 503)

    def test_flags_cannot_be_paths(self):
        reply, call = self.request(SimpleNamespace(returncode=0), ["--validate-only"])
        self.assertEqual(reply[0], 400)
        self.assertIsNone(call)

    def test_shrink_contract_and_bad_requests(self):
        result = SimpleNamespace(returncode=1, stdout="", stderr="refused")
        reply, call = self.request(result, route="/shrink", extra={"withPc": True})
        self.assertEqual(reply, (200, {"exit_code": 1, "stdout": "", "stderr": "refused"}))
        self.assertEqual(call.args[0], ["sudo", "-n", shrinkd.SCRIPT, "--with-pc", "/media/series/Safe"])
        for kwargs, status in [({"auth": "Bearer wrong"}, 403), ({"raw": b"[]"}, 400),
                               ({"extra": {"withPc": "yes"}}, 400), ({"paths": ["/other/a.mkv"]}, 400)]:
            reply, call = self.request(result, **kwargs)
            self.assertEqual(reply[0], status)
            self.assertIsNone(call)


class HostValidationTests(unittest.TestCase):
    def test_paths_cap_and_protected_hardlinks(self):
        source = Path(__file__).with_name("tdarr-shrink").read_text()
        code = source.split("<<'PY_VALIDATE'\n", 1)[1].split("\nPY_VALIDATE", 1)[0]
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            movies, series = root / "movies", root / "series"
            movies.mkdir()
            series.mkdir()
            safe = series / "safe.mkv"
            safe.touch()
            env = {"TDARR_MOVIE_ROOT": "/media/movies", "TDARR_SERIES_ROOT": "/media/series",
                   "TDARR_PATH_MAP": json.dumps({"/media/movies": str(movies), "/media/series": str(series)}),
                   "TDARR_PROTECTED_RADARR_IDS": ""}

            def validate(path, protected=""):
                output = io.StringIO()
                with patch.dict(os.environ, {**env, "TDARR_PROTECTED_RADARR_IDS": protected,
                                             "RADARR_API_KEY": "fixture"}, clear=True), \
                     patch("sys.argv", ["validate", path]), contextlib.redirect_stdout(output):
                    exec(compile(code, "host-validation", "exec"), {})
                return json.loads(output.getvalue())

            self.assertEqual(validate(str(safe)), {"paths": ["/media/series/safe.mkv"]})
            outside = root / "outside.mkv"
            outside.touch()
            (series / "escape.mkv").symlink_to(outside)
            with self.assertRaises(SystemExit):
                validate("/media/series/escape.mkv")
            (series / "escape.mkv").unlink()
            for i in range(16):
                (series / (str(i) + ".mkv")).touch()
            with self.assertRaises(SystemExit):
                validate("/media/series")
            protected = movies / "Protected"
            protected.mkdir()
            original = protected / "movie.mkv"
            original.touch()
            alias = series / "alias.mkv"
            os.link(original, alias)
            response = io.BytesIO(json.dumps({"id": 12, "path": "/media/movies/Protected"}).encode())
            with patch("urllib.request.urlopen", return_value=response):
                with self.assertRaises(SystemExit):
                    validate(str(alias), "12")


if __name__ == "__main__":
    unittest.main()
