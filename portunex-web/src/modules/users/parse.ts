import { enumeration, record, string, timestamp } from '../../shared/parse';
import type { UserRow } from './model';

export function parseUser(value: unknown): UserRow {
  const input = record(value);
  return { id: string(input.id), name: string(input.name), email: string(input.email), role: enumeration(input.role, ['admin', 'user']), status: enumeration(input.status, ['active', 'disabled']), createdAt: timestamp(input.createdAt) };
}
