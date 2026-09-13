import { enumeration, integer, record, string, timestamp } from '../../shared/parse';
import type { ProviderRow } from './model';

export function parseProvider(value: unknown): ProviderRow {
  const input = record(value);
  return { id: string(input.id), name: string(input.name), type: enumeration(input.type, ['Claude', 'OpenAI', 'Gemini']), status: enumeration(input.status, ['ready', 'cooling', 'disabled']), modelCount: integer(input.modelCount), updatedAt: timestamp(input.updatedAt) };
}
