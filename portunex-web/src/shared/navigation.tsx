import { createContext, useContext, useEffect, useState, type MouseEvent, type ReactNode } from 'react';

const Navigation = createContext<{ path: string; navigate(path: string, replace?: boolean): void } | null>(null);
export function NavigationProvider({ children }: { children: ReactNode }) {
  const [path, setPath] = useState(window.location.pathname);
  useEffect(() => { const update = () => setPath(window.location.pathname); window.addEventListener('popstate', update); return () => window.removeEventListener('popstate', update); }, []);
  const navigate = (next: string, replace = false) => { window.history[replace ? 'replaceState' : 'pushState']({}, '', next); setPath(next); };
  return <Navigation.Provider value={{ path, navigate }}>{children}</Navigation.Provider>;
}
export function useNavigation() { const value = useContext(Navigation); if (!value) throw new Error('NavigationProvider required'); return value; }
export function AppLink({ href, children, className, current }: { href: string; children: ReactNode; className?: string; current?: boolean }) {
  const { navigate } = useNavigation();
  const click = (event: MouseEvent<HTMLAnchorElement>) => { if (event.button || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return; event.preventDefault(); navigate(href); };
  return <a href={href} onClick={click} className={className} aria-current={current ? 'page' : undefined}>{children}</a>;
}
