# Task: small task-management web app (3 components)

Build exactly the 3 components described below, in the existing directory layout (`backend/`, `analysis/`, `frontend/` are already present with some starter files — do not rename or move them, do not add other top-level directories). Do not run `git`. Do not add a database, authentication, or any UI/HTML — none of that is in scope. Do not call any real external network API anywhere (the "AI" API in component 2 is entirely simulated/local — see below).

## 1. Backend: Go task-management REST API (`backend/`)

`backend/go.mod` already exists (module `taskapi`, go 1.25) — do not change it. Write `backend/main.go` (you may split into more `.go` files in the same directory if you prefer).

An in-memory (no persistence needed) HTTP JSON API for tasks. A task looks like:

```json
{ "id": "task-1", "title": "Write report", "priority": "high", "dueDate": "2026-03-10", "status": "pending" }
```

- `priority` is one of `"low"`, `"medium"`, `"high"` (case-sensitive; nothing else is valid).
- `dueDate` is `YYYY-MM-DD`.
- `status` is `"pending"` or `"done"`; new tasks are always created with `status: "pending"`.
- `id` is any unique, non-empty string you generate.

Endpoints:

- **`POST /tasks`** — body `{"title": string, "priority": string, "dueDate": string}`. Validate: `title` must be non-empty after trimming whitespace; `priority` must be exactly one of the three valid values; `dueDate` must be a real calendar date in `YYYY-MM-DD` format. On any validation failure, respond `400` with JSON `{"error": "<message>"}`. On success, respond `201` with the full created task JSON (including generated `id` and `status: "pending"`).
- **`GET /tasks?sort=priority|dueDate&order=asc|desc`** — returns a JSON array of all tasks.
  - `sort=dueDate` (or `sort` omitted entirely, the default): order chronologically by `dueDate`.
  - `sort=priority`: order by how urgent the priority is (`high` is the most urgent, then `medium`, then `low`) — **not** alphabetically.
  - `order=asc` (default if omitted) vs `order=desc` flips the direction for whichever field is being sorted.
  - When two tasks tie on the primary sort key, break the tie by `dueDate` ascending, regardless of what the primary sort field was.
  - Every call to this endpoint must return the same, fully deterministic order for the same underlying data — the ordering must never depend on incidental iteration order of whatever data structure you use internally.
- **`PATCH /tasks/:id`** — body `{"status": "pending"|"done"}`. `404` with `{"error": "..."}` if the id doesn't exist. `400` with `{"error": "..."}` if `status` isn't one of the two valid values. `200` with the updated task JSON on success.

The server must listen on the port given by the `PORT` environment variable, defaulting to `8080` if `PORT` is unset.

## 2. Analysis: Python task statistics + AI-summary handling (`analysis/`)

Write exactly these two files with exactly these importable names (other code depends on these exact module/function names and signatures):

**`analysis/stats.py`**

```python
def compute_stats(tasks: list[dict], today: str) -> dict:
    ...
```

`tasks` is a list of task dicts shaped like the backend's JSON (keys: `id`, `title`, `priority`, `dueDate`, `status`). `today` is an ISO date string (`YYYY-MM-DD`) representing "the current date" — always use this parameter as your notion of "now", never read the real system clock, so the function is deterministic and testable. Return a dict:

```python
{
  "total": <int, total number of tasks>,
  "by_priority": {"low": <int>, "medium": <int>, "high": <int>},  # always all 3 keys present
  "done_count": <int>,
  "pending_count": <int>,
  "overdue_count": <int>,  # pending tasks whose dueDate is strictly before `today` (a task due exactly on `today` is NOT overdue)
}
```

**`analysis/ai_summary.py`**

```python
def build_prompt(tasks: list[dict]) -> str:
    ...

def parse_ai_response(response_json: str) -> dict:
    ...
```

`build_prompt` renders a prompt string describing the given tasks (for a hypothetical summarization AI) — include each task's title, priority, and status somewhere in the output. Handle the empty-list case gracefully (return a sensible message, don't crash).

`parse_ai_response` parses a JSON string that represents a (simulated) AI API's response, always shaped like `{"summary": string, "action_items": [string, ...], "confidence": number}`, and returns a dict with those same three keys/values on success. Since this is untrusted external input (an AI API's output should never be trusted blindly), you must validate it thoroughly and raise a plain `ValueError` (with a descriptive message; never let a raw `json.JSONDecodeError`, `KeyError`, `TypeError`, etc. escape uncaught) whenever:
- the input isn't valid JSON, or isn't a JSON object,
- any of the three required keys is missing,
- `summary` isn't a string, or `action_items` isn't a list of strings,
- `confidence` isn't a number, or is outside the valid `[0, 1]` range.

## 3. Frontend: TypeScript API client + display logic (`frontend/`)

`frontend/tsconfig.json` already exists — do not change it; your code must typecheck cleanly under it (`npx tsc -p tsconfig.json --noEmit`, strict mode). Write exactly these two files with exactly these exported names:

**`frontend/client.ts`** — a typed wrapper around the backend's HTTP API, using the global `fetch` (no extra HTTP library dependency):

```typescript
export interface Task {
  id: string;
  title: string;
  priority: 'low' | 'medium' | 'high';
  dueDate: string;
  status: 'pending' | 'done';
}

export async function createTask(
  baseUrl: string,
  input: { title: string; priority: 'low' | 'medium' | 'high'; dueDate: string }
): Promise<Task> { ... }

export async function listTasks(
  baseUrl: string,
  opts?: { sort?: 'priority' | 'dueDate'; order?: 'asc' | 'desc' }
): Promise<Task[]> { ... }

export async function updateTaskStatus(
  baseUrl: string,
  id: string,
  status: 'pending' | 'done'
): Promise<Task> { ... }
```

Each function calls the corresponding backend endpoint (`POST /tasks`, `GET /tasks` with query params, `PATCH /tasks/:id`) against `baseUrl`, and returns the parsed JSON response on success. On a non-ok HTTP response, throw a JavaScript `Error` — read the response body's `error` field (if the body is JSON and has one) into the thrown error's message rather than a generic message. For `listTasks`, only include `sort`/`order` in the query string when the caller actually provided them (don't send them as literal empty or `"undefined"` values when `opts` is omitted or a field is unset).

**`frontend/transform.ts`** — pure display-logic helpers (no network calls):

```typescript
import type { Task } from './client';

export function groupByPriority(tasks: Task[]): Record<'low' | 'medium' | 'high', Task[]> { ... }

export function statusBadgeColor(
  status: 'pending' | 'done',
  dueDate: string,
  today: string
): 'red' | 'yellow' | 'green' { ... }
```

`groupByPriority` buckets tasks by their `priority`, always returning all three keys (`low`, `medium`, `high`) even when a bucket is empty. `statusBadgeColor` returns `'green'` for a done task regardless of date; for a pending task, `'red'` if `dueDate` is strictly before `today` (overdue), otherwise `'yellow'` — a task due exactly on `today` is `'yellow'`, not `'red'` (mirrors the backend's overdue rule).

## When you're done

Make sure all 3 components are present and self-consistent with the contracts above. Do not write your own test files anywhere (verification will be done externally). Do not explain your reasoning at length in your final reply — just implement the three components.
