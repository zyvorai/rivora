import { useState } from 'react';
import { login } from '../auth';

export default function Login({ onLogin }: { onLogin: () => void }) {
  const [apiToken, setApiToken] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const host = window.location.host || window.location.hostname;

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError('');
    const result = await login(apiToken);
    setBusy(false);
    if (!result.ok) {
      setError(result.error);
      return;
    }
    onLogin();
  }

  return (
    <div className="login-shell">
      <div className="login-info">
        <img src="/zyvor-mark.svg" alt="Zyvor" className="login-logo" />
        <p className="eyebrow">Rivora · Zyvor</p>
        <h1>Own the VIP. Steer the backends.</h1>
        <p>
          Rivora is an eBPF-native load balancer for Kubernetes and bare metal — Maglev selection, DSR and full-NAT,
          health-gated backends, served from one console.
        </p>
        <p className="login-host">
          Connecting to <code>{host}</code>
        </p>
      </div>
      <form className="card login-card" onSubmit={submit}>
        <h1>Sign in.</h1>
        <label className="tokenbox">
          API token
          <input
            type="password"
            value={apiToken}
            onChange={(e) => setApiToken(e.target.value)}
            autoFocus
            autoComplete="off"
            placeholder="RIVORA_API_KEY (leave empty if auth is off)"
          />
        </label>
        {error && <p className="warning">{error}</p>}
        <button type="submit" className="primary" disabled={busy}>
          {busy ? 'Checking…' : 'Sign in'}
        </button>
      </form>
    </div>
  );
}
