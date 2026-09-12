import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock


SPEC = importlib.util.spec_from_file_location("business_api_mcp", Path(__file__).with_name("business_api_mcp.py"))
module = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(module)


class Response:
    def __enter__(self): return self
    def __exit__(self, *_): return False
    def read(self, *_): return json.dumps({"access_token": "fresh", "expires_in": 3600}).encode()


class BiginTokenCacheTest(unittest.TestCase):
    def test_reuses_refreshed_token_across_calls(self):
        with tempfile.TemporaryDirectory() as cache:
            env = {
                "BIGIN_BASE_URL": "https://example.invalid",
                "BIGIN_ACCOUNTS_URL": "https://accounts.example.invalid",
                "BIGIN_CLIENT_ID": "client",
                "BIGIN_CLIENT_SECRET": "secret",
                "BIGIN_REFRESH_TOKEN": "refresh",
                "TOOLMUX_TOKEN_CACHE_DIR": cache,
            }
            with mock.patch.dict(os.environ, env, clear=False), mock.patch.object(module.urllib.request, "urlopen", return_value=Response()) as urlopen:
                self.assertEqual(module.bigin_access_token(), (env["BIGIN_BASE_URL"], "fresh"))
                self.assertEqual(module.bigin_access_token(), (env["BIGIN_BASE_URL"], "fresh"))
                self.assertEqual(urlopen.call_count, 1)


if __name__ == "__main__":
    unittest.main()
