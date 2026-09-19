// The bearer token is held in memory only: it is never written to
// localStorage/sessionStorage, so a page reload signs the operator out and the
// token can't be lifted from browser storage by another script on the origin.
let currentToken = '';
export const token = () => currentToken;
export function setToken(v: string) {
  currentToken = v;
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
