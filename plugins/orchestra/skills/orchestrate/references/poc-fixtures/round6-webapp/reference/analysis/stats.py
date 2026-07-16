from datetime import date


def _parse(d: str) -> date:
    y, m, dd = d.split("-")
    return date(int(y), int(m), int(dd))


def compute_stats(tasks: list[dict], today: str) -> dict:
    today_d = _parse(today)
    by_priority = {"low": 0, "medium": 0, "high": 0}
    done_count = 0
    pending_count = 0
    overdue_count = 0

    for t in tasks:
        by_priority[t["priority"]] = by_priority.get(t["priority"], 0) + 1
        if t["status"] == "done":
            done_count += 1
        else:
            pending_count += 1
            if _parse(t["dueDate"]) < today_d:
                overdue_count += 1

    return {
        "total": len(tasks),
        "by_priority": by_priority,
        "done_count": done_count,
        "pending_count": pending_count,
        "overdue_count": overdue_count,
    }
