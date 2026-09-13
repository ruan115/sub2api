import { enumeration, record, string, timestamp } from '../../shared/parse';
import { PortError } from '../../shared/ports';
import type { Session } from './model';

export function parseSession(value: unknown, now = Date.now()): Session {
  const input = record(value);
  const user = record(input.user);
  const session: Session = {
    user: { id: string(user.id), email: string(user.email), displayName: string(user.displayName), role: enumeration(user.role, ['admin', 'user']) },
    expiresAt: timestamp(input.expiresAt),
  };
  if (Date.parse(session.expiresAt) <= now) throw new PortError('unauthorized', '本地演示会话已过期，请重新登录。');
  return session;
}
