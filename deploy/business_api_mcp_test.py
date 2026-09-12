import importlib.util
import pathlib
import unittest


PATH = pathlib.Path(__file__).with_name("business_api_mcp.py")
SPEC = importlib.util.spec_from_file_location("business_api_mcp", PATH)
MODULE = importlib.util.module_from_spec(SPEC)
assert SPEC.loader
SPEC.loader.exec_module(MODULE)


class BusinessAdapterTests(unittest.TestCase):
    def test_every_provider_has_one_bounded_request_tool(self):
        for provider in ("zammad", "bigin", "xpayroll"):
            catalog = MODULE.tools(provider)
            self.assertEqual([tool["name"] for tool in catalog], ["request"])
            self.assertFalse(catalog[0]["inputSchema"].get("additionalProperties", False))

    def test_relative_endpoint_rejects_external_and_parent_paths(self):
        for value in ("https://example.com", "//example.com/path", "../secret", "tickets bad", ""):
            with self.assertRaises(ValueError):
                MODULE.relative_endpoint(value)
        self.assertEqual(MODULE.relative_endpoint("/tickets?per_page=1"), "tickets?per_page=1")

    def test_payroll_operation_catalog_matches_local_capability(self):
        self.assertEqual(len(MODULE.XPAYROLL_OPERATIONS), 18)
        self.assertIn("payroll.view", MODULE.XPAYROLL_OPERATIONS)
        self.assertIn("contractor.list_pending", MODULE.XPAYROLL_OPERATIONS)


if __name__ == "__main__":
    unittest.main()
