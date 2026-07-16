import json
import sys

sys.path.insert(0, ".")

from stats import compute_stats
from ai_summary import build_prompt, parse_ai_response

TASKS = [
    {"id": "1", "title": "A", "priority": "low", "dueDate": "2026-03-01", "status": "pending"},
    {"id": "2", "title": "B", "priority": "high", "dueDate": "2026-03-01", "status": "pending"},  # overdue if today > this
    {"id": "3", "title": "C", "priority": "high", "dueDate": "2026-03-10", "status": "done"},
    {"id": "4", "title": "D", "priority": "medium", "dueDate": "2026-03-10", "status": "pending"},  # due exactly "today"
]


def test_compute_stats_counts():
    result = compute_stats(TASKS, "2026-03-10")
    assert result["total"] == 4
    assert result["by_priority"] == {"low": 1, "medium": 1, "high": 2}
    assert result["done_count"] == 1
    assert result["pending_count"] == 3


def test_compute_stats_overdue_boundary():
    # today = 2026-03-10: task "A" (2026-03-01, pending) and "B" (2026-03-01, pending) are overdue.
    # task "D" is due EXACTLY today (2026-03-10) and pending -> must NOT count as overdue.
    result = compute_stats(TASKS, "2026-03-10")
    assert result["overdue_count"] == 2, f"expected 2 overdue, got {result['overdue_count']}"


def test_compute_stats_empty():
    result = compute_stats([], "2026-01-01")
    assert result["total"] == 0
    assert result["by_priority"] == {"low": 0, "medium": 0, "high": 0}
    assert result["overdue_count"] == 0


def test_build_prompt_empty():
    p = build_prompt([])
    assert isinstance(p, str) and len(p) > 0


def test_build_prompt_contains_task_info():
    p = build_prompt(TASKS)
    assert "A" in p and "high" in p


def test_parse_ai_response_valid():
    raw = json.dumps({"summary": "All good", "action_items": ["do x", "do y"], "confidence": 0.75})
    result = parse_ai_response(raw)
    assert result["summary"] == "All good"
    assert result["action_items"] == ["do x", "do y"]
    assert result["confidence"] == 0.75


def test_parse_ai_response_rejects_out_of_range_confidence():
    raw = json.dumps({"summary": "s", "action_items": [], "confidence": 1.5})
    try:
        parse_ai_response(raw)
        assert False, "expected ValueError for confidence > 1"
    except ValueError:
        pass


def test_parse_ai_response_rejects_negative_confidence():
    raw = json.dumps({"summary": "s", "action_items": [], "confidence": -0.2})
    try:
        parse_ai_response(raw)
        assert False, "expected ValueError for negative confidence"
    except ValueError:
        pass


def test_parse_ai_response_rejects_malformed_json():
    try:
        parse_ai_response("{not valid json")
        assert False, "expected ValueError for malformed JSON"
    except ValueError:
        pass


def test_parse_ai_response_rejects_missing_key():
    raw = json.dumps({"summary": "s", "action_items": []})  # missing confidence
    try:
        parse_ai_response(raw)
        assert False, "expected ValueError for missing key"
    except ValueError:
        pass


def test_parse_ai_response_rejects_wrong_type():
    raw = json.dumps({"summary": "s", "action_items": "not a list", "confidence": 0.5})
    try:
        parse_ai_response(raw)
        assert False, "expected ValueError for wrong action_items type"
    except ValueError:
        pass
