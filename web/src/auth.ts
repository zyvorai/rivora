// Login gate over Rivora's shared-bearer-token model (RIVORA_API_KEY). The
// server is the only authority: the console holds no credentials of its own and
// simply checks the token an operator types against the API.
import { setToken } from './api';

export type LoginResult = { ok: true } | { ok: false; error: string };

// login verifies the token by calling /api/v1/status. An empty token is allowed
// so a rivorad started without RIVORA_API_KEY (auth off) is still reachable;
// if auth is on, the server answers 401 and the sign-in is refused.
export async function login(apiToken: string): Promise<LoginResult> {
  const headers: Record<string, string> = {};
  if (apiToken) headers.Authorization = `Bearer ${apiToken}`;
  let r: Response;
  try {
    r = await fetch('/api/v1/status', { headers });
  } catch {
    return { ok: false, error: 'Could not reach the Rivora API.' };
  }
  if (r.status === 401) {
    return { ok: false, error: 'Invalid API token.' };
  }
  if (!r.ok) {
    return { ok: false, error: `API returned ${r.status} ${r.statusText}.` };
  }
  setToken(apiToken);
  return { ok: true };
}

export function logout(): void {
  setToken('');
}
