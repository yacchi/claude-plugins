"""Tests for agent_exec_pricing: the price list, longest-prefix matching, cache
write derivation, user overrides, and what stays unpriced."""

import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import agent_exec_pricing as pricing  # noqa: E402


class PriceForTests(unittest.TestCase):
    def test_longest_prefix_wins(self):
        self.assertEqual(pricing.price_for("claude-fable-5-1")["input"], 10.0)
        self.assertEqual(pricing.price_for("claude-fable-5-1")["cache_read"], 0.25)
        self.assertEqual(pricing.price_for("claude-fable-5")["cache_read"], 1.0)
        self.assertEqual(pricing.price_for("claude-opus-5-5")["input"], 4.0)
        self.assertEqual(pricing.price_for("claude-opus-5")["input"], 5.0)

    def test_dated_ids_resolve(self):
        self.assertEqual(pricing.price_for("claude-haiku-4-5-20251001")["output"], 5.0)

    def test_cache_writes_derive_from_input(self):
        price = pricing.price_for("claude-sonnet-5-5")
        self.assertEqual(price["cache_write_5m"], 2.5)
        self.assertEqual(price["cache_write_1h"], 4.0)

    def test_unknown_and_garbage_models_are_unpriced(self):
        for model in ("gpt-5.6-luna", "", None, 7, "<synthetic>"):
            self.assertIsNone(pricing.price_for(model), repr(model))

    def test_override_wins_and_defaults_the_derived_fields(self):
        overrides = {"models": {"claude-opus-5-5": {"input": 1, "output": 2}}}
        price = pricing.price_for("claude-opus-5-5", overrides)
        self.assertEqual((price["input"], price["output"]), (1.0, 2.0))
        self.assertEqual(price["cache_read"], 0.1)
        self.assertEqual(price["cache_write_5m"], 1.25)

    def test_malformed_override_is_ignored_not_fatal(self):
        for bad in ({"models": {"claude-opus-5-5": {"input": "four"}}},
                    {"models": {"claude-opus-5-5": {"input": True, "output": 2}}},
                    {"models": {"claude-opus-5-5": {"input": -1, "output": 2}}},
                    {"models": "nope"}, "nope"):
            self.assertEqual(pricing.price_for("claude-opus-5-5", bad)["input"], 4.0, repr(bad))


class EstimateTests(unittest.TestCase):
    def test_each_component_uses_its_own_rate(self):
        detail = {"claude-opus-5-5": {"fresh_input": 1_000_000, "cache_write_5m": 1_000_000,
                                      "cache_write_1h": 1_000_000, "cache_read": 1_000_000,
                                      "output": 1_000_000}}
        est = pricing.estimate(detail)
        self.assertEqual(est["micro_usd"], round((4 + 5 + 8 + 0.2 + 20) * 1_000_000))

    def test_all_zero_models_are_skipped(self):
        est = pricing.estimate({"claude-opus-5-5": {k: 0 for k in pricing.DETAIL_KEYS}})
        self.assertEqual((est["micro_usd"], est["models"], est["unpriced_models"]), (0, {}, {}))

    def test_unpriced_model_reports_its_token_total(self):
        est = pricing.estimate({"mystery": {"fresh_input": 5, "output": 7}})
        self.assertEqual(est["unpriced_models"], {"mystery": 12})
        self.assertEqual(est["micro_usd"], 0)

    def test_price_list_provenance_is_carried(self):
        est = pricing.estimate({})
        self.assertEqual(est["price_list"]["as_of"], pricing.PRICE_LIST_AS_OF)


if __name__ == "__main__":
    unittest.main()
