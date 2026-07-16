export interface Task {
  id: string;
  title: string;
  priority: 'low' | 'medium' | 'high';
  dueDate: string;
  status: 'pending' | 'done';
}

async function readError(res: Response): Promise<string> {
  try {
    const body = await res.json();
    if (body && typeof body.error === 'string') return body.error;
  } catch {
    // fall through
  }
  return `request failed with status ${res.status}`;
}

export async function createTask(
  baseUrl: string,
  input: { title: string; priority: 'low' | 'medium' | 'high'; dueDate: string }
): Promise<Task> {
  const res = await fetch(`${baseUrl}/tasks`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  });
  if (!res.ok) throw new Error(await readError(res));
  return res.json();
}

export async function listTasks(
  baseUrl: string,
  opts?: { sort?: 'priority' | 'dueDate'; order?: 'asc' | 'desc' }
): Promise<Task[]> {
  const params = new URLSearchParams();
  if (opts?.sort) params.set('sort', opts.sort);
  if (opts?.order) params.set('order', opts.order);
  const qs = params.toString();
  const res = await fetch(`${baseUrl}/tasks${qs ? '?' + qs : ''}`);
  if (!res.ok) throw new Error(await readError(res));
  return res.json();
}

export async function updateTaskStatus(
  baseUrl: string,
  id: string,
  status: 'pending' | 'done'
): Promise<Task> {
  const res = await fetch(`${baseUrl}/tasks/${id}`, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ status }),
  });
  if (!res.ok) throw new Error(await readError(res));
  return res.json();
}
