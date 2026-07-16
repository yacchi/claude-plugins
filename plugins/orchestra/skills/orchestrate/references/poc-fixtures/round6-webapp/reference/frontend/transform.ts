import type { Task } from './client';

export function groupByPriority(tasks: Task[]): Record<'low' | 'medium' | 'high', Task[]> {
  const groups: Record<'low' | 'medium' | 'high', Task[]> = { low: [], medium: [], high: [] };
  for (const t of tasks) {
    groups[t.priority].push(t);
  }
  return groups;
}

export function statusBadgeColor(
  status: 'pending' | 'done',
  dueDate: string,
  today: string
): 'red' | 'yellow' | 'green' {
  if (status === 'done') return 'green';
  return dueDate < today ? 'red' : 'yellow';
}
