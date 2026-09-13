export type Role = 'admin' | 'user';
export interface Session {
  user: { id: string; email: string; displayName: string; role: Role };
  expiresAt: string;
}
export interface LoginInput { email: string; password: string }
export interface IdentityPort {
  getSession(signal?: AbortSignal): Promise<Session | null>;
  login(input: LoginInput, signal?: AbortSignal): Promise<Session>;
  logout(): Promise<void>;
}
