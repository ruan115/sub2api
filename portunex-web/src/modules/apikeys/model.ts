export interface ApiKeyRow {
  id: string;
  name: string;
  keyHint: string;
  owner: string;
  status: 'active' | 'disabled';
  lastUsedAt: string | null;
}
