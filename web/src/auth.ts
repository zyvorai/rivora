// Frontend-only login gate over Rivora's shared-bearer-token model
// (RIVORA_API_KEY). Same friendly admin/password pair as Netra's console
// so the Zyvor lab UX stays consistent across products.
import { setToken } from './api';

const USERNAME = 'admin';
const PASSWORD = 'Admin@321';
const API_TOKEN = 'Admin@321';

export function checkCredentials(username: string, password: string): string | null {
  return username === USERNAME && password === PASSWORD ? API_TOKEN : null;
}

export function logout(): void {
  setToken('');
}
