import copy
import pathlib
import runpy
import unittest

CHECK = runpy.run_path(str(pathlib.Path(__file__).with_name("check-forward.py")))["check_value"]


class ForwardCheckerTest(unittest.TestCase):
    def test_unknown_tier_cannot_be_priced_or_satisfy_positive_gate(self):
        row = {"origins": [{"normalized_source_usage": {"cache_create_5m": 25, "cache_create_1h": 75}, "native_provenance": {}}],
               "value": None, "issues": ["charged_tier_unknown"]}
        totals = {"valued_count": 0, "valued_subset": {"low_nano_usd": 0, "high_nano_usd": 0}}
        self.assertIsNone(CHECK(row, totals, totals, "unknown"))
        with self.assertRaises((AssertionError, KeyError)):
            CHECK(row, totals, totals, "standard")
        false = copy.deepcopy(row)
        false["value"] = {"low_nano_usd": 0, "high_nano_usd": 0}
        with self.assertRaises(AssertionError):
            CHECK(false, totals, totals, "unknown")
        value = {"low_nano_usd": 945000, "high_nano_usd": 945000,
                 "tokens": {"cache_write_5m": 25, "cache_write_1h": 75, "cache_write_unknown": 0}}
        row["value"] = value
        row["origins"][0]["native_provenance"]["response_service_tier"] = "standard"
        after = {"valued_count": 1, "valued_subset": {"low_nano_usd": 945000, "high_nano_usd": 945000}}
        self.assertEqual(CHECK(row, totals, after, "standard"), 945000)
        row["origins"][0]["native_provenance"]["response_service_tier"] = "auto"
        with self.assertRaises(AssertionError):
            CHECK(row, totals, after, "standard")


if __name__ == "__main__":
    unittest.main()
