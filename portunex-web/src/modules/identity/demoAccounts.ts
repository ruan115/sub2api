// Public, synthetic credentials shared with the loopback-only Go demo.
export const demoAccounts = {
  admin: { email: 'admin@example.invalid', password: 'Demo-admin-2026!', label: '管理员' },
  user: { email: 'member@example.invalid', password: 'Demo-member-2026!', label: '普通用户' },
} as const;
