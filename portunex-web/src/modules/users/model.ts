export interface UserRow {
  id: string;
  name: string;
  email: string;
  role: 'admin' | 'user';
  status: 'active' | 'disabled';
  createdAt: string;
}
