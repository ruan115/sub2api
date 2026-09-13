import type { Page } from './ports';
import { PortError } from './ports';

export function invalidPayload(): never {
  throw new PortError('unavailable', '本地演示服务返回的数据格式不符合演示接口。');
}

export function record(value: unknown): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return invalidPayload();
  return value as Record<string, unknown>;
}

export function string(value: unknown): string {
  if (typeof value !== 'string' || value.length === 0 || value.length > 4096) return invalidPayload();
  return value;
}

export function enumeration<const T extends readonly string[]>(value: unknown, choices: T): T[number] {
  if (typeof value !== 'string' || !choices.includes(value)) return invalidPayload();
  return value;
}

export function integer(value: unknown, minimum = 0): number {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < minimum) return invalidPayload();
  return value;
}

export function timestamp(value: unknown): string {
  const parsed = string(value);
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(parsed) || !Number.isFinite(Date.parse(parsed))) return invalidPayload();
  return parsed;
}

export function parsePage<T>(value: unknown, parseRow: (row: unknown) => T): Page<T> {
  const input = record(value);
  const total = integer(input.total);
  const page = integer(input.page, 1);
  const pageSize = integer(input.pageSize, 1);
  if (!Array.isArray(input.items) || input.items.length > pageSize || input.items.length > total) return invalidPayload();
  return { items: input.items.map(parseRow), total, page, pageSize };
}
