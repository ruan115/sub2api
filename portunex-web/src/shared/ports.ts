export interface PageQuery { query: string; page: number; pageSize: number }
export interface Page<T> { items: T[]; total: number; page: number; pageSize: number }
export interface ListPort<T> { list(query: PageQuery, signal?: AbortSignal): Promise<Page<T>> }

export class PortError extends Error {
  constructor(public readonly code: 'unauthorized' | 'unavailable' | 'invalid_credentials', message: string) {
    super(message);
    this.name = 'PortError';
  }
}
