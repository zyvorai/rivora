// localStorage, not sessionStorage — same rationale as Netra's web/src/api.ts.
export const token = () => localStorage.getItem('rivora-token') || '';
export function setToken(v: string) {
  if (v) localStorage.setItem('rivora-token', v);
  else localStorage.removeItem('rivora-token');
}
function headers(extra: Record<string, string> = {}) {
  const h: { [k: string]: string } = { ...extra };
  if (token()) h.Authorization = `Bearer ${token()}`;
  return h;
}
export async function api<T = unknown>(path: string, init: RequestInit = {}): Promise<T> {
  const r = await fetch(path, { ...init, headers: headers((init.headers as Record<string, string>) || {}) });
  if (!r.ok) {
    if (r.status === 401) {
      setToken('');
      window.dispatchEvent(new Event('rivora-auth-expired'));
    }
    throw new Error((await r.text()) || r.statusText);
  }
  return r.json();
}
