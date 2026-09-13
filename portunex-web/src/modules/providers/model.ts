export interface ProviderRow {
  id: string;
  name: string;
  type: 'Claude' | 'OpenAI' | 'Gemini';
  status: 'ready' | 'cooling' | 'disabled';
  modelCount: number;
  updatedAt: string;
}
