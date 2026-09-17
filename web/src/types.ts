export type BackendStatus = {
  id: number;
  address: string;
  port: number;
  weight: number;
  healthy: boolean;
  packets: number;
  bytes: number;
};

export type VIPStatus = {
  vipAddress: string;
  vipPort: number;
  protocol: string;
  mode: string;
  interface: string;
  startedAt: string;
  backends: BackendStatus[];
  packets: number;
  bytes: number;
  dropped: number;
};

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
