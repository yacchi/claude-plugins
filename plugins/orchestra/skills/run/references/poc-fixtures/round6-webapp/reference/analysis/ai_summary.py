import json


def build_prompt(tasks: list[dict]) -> str:
    if not tasks:
        return "There are no tasks to summarize."
    lines = ["Summarize the following tasks:"]
    for t in tasks:
        lines.append(f"- {t['title']} (priority: {t['priority']}, status: {t['status']})")
    return "\n".join(lines)


def parse_ai_response(response_json: str) -> dict:
    try:
        data = json.loads(response_json)
    except (json.JSONDecodeError, TypeError) as e:
        raise ValueError(f"malformed AI response JSON: {e}")

    if not isinstance(data, dict):
        raise ValueError("AI response must be a JSON object")

    for key in ("summary", "action_items", "confidence"):
        if key not in data:
            raise ValueError(f"AI response missing required key: {key}")

    if not isinstance(data["summary"], str):
        raise ValueError("summary must be a string")
    if not isinstance(data["action_items"], list) or not all(isinstance(x, str) for x in data["action_items"]):
        raise ValueError("action_items must be a list of strings")
    confidence = data["confidence"]
    if not isinstance(confidence, (int, float)) or isinstance(confidence, bool):
        raise ValueError("confidence must be a number")
    if confidence < 0 or confidence > 1:
        raise ValueError(f"confidence out of range [0,1]: {confidence}")

    return {
        "summary": data["summary"],
        "action_items": data["action_items"],
        "confidence": confidence,
    }
