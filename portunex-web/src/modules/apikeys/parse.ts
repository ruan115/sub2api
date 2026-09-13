import { enumeration, record, string, timestamp } from '../../shared/parse';
import type { ApiKeyRow } from './model';

export function parseApiKey(value: unknown): ApiKeyRow {
  const input = record(value);
  return { id: string(input.id), name: string(input.name), keyHint: string(input.keyHint), owner: string(input.owner), status: enumeration(input.status, ['active', 'disabled']), lastUsedAt: input.lastUsedAt === null ? null : timestamp(input.lastUsedAt) };
}
