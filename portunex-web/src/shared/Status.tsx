const labels: Record<string, string> = { active: '正常', disabled: '已停用', ready: '可用', cooling: '冷却中', admin: '管理员', user: '普通用户' };
export function Status({ value }: { value: string }) { return <span className={`status status--${value}`}><span aria-hidden="true" />{labels[value] ?? value}</span>; }
export function dateLabel(value: string | null) { if (!value) return '尚未使用'; return new Intl.DateTimeFormat('zh-CN', { year: 'numeric', month: '2-digit', day: '2-digit', timeZone: 'Asia/Shanghai' }).format(new Date(value)); }
