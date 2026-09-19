export type BackendStatus = {
  id: number;
  address: string;
  port: number;
  weight: number;
  healthy: boolean;
  // Absent when talking to an older rivorad; fall back to `healthy`.
  state?: 'healthy' | 'draining' | 'down';
  adminDraining?: boolean;
  packets: number;
  bytes: number;
};

export type VIPStatus = {
  vipAddress: string;
  vipPort: number; // the port, or a port range's first port
  vipPortEnd?: number; // a port range's last port; absent for a single-port VIP
  protocol: string;
  mode: string;
  interface: string;
  startedAt: string;
  backends: BackendStatus[];
  packets: number;
  bytes: number;
  dropped: number;
};

// "443", or "30000-30100" for a range VIP.
export function vipPortLabel(v: { vipPort: number; vipPortEnd?: number }): string {
  return v.vipPortEnd ? `${v.vipPort}-${v.vipPortEnd}` : String(v.vipPort);
}

export function fmtBytes(n: number): string {
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let v = Number(n) || 0;
  let i = 0;
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v < 10 && i > 0 ? v.toFixed(1) : Math.round(v)} ${u[i]}`;
}
