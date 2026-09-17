import { useEffect, useState } from 'react';
import { api } from '../api';
import type { BackendStatus, VIPStatus } from '../types';
import { fmtBytes } from '../types';

type Row = BackendStatus & { vip: string };

export default function Backends() {
  const [rows, setRows] = useState<Row[]>([]);
  const [err, setErr] = useState('');

  useEffect(() => {
    let cancelled = false;
    const load = () =>
      api<VIPStatus[]>('/api/v1/vips')
        .then((vips) => {
          if (cancelled) return;
          const list = Array.isArray(vips) ? vips : [];
          const next: Row[] = [];
          for (const v of list) {
            for (const b of v.backends || []) {
              next.push({ ...b, vip: `${v.vipAddress}:${v.vipPort}` });
            }
          }
          setRows(next);
          setErr('');
        })
        .catch((e) => {
          if (!cancelled) setErr(String(e));
        });
    load();
    const id = setInterval(load, 3000);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, []);

  return (
    <div className="grid">
      <section className="card span2">
        <p className="eyebrow">BACKENDS</p>
        <h3>Live Maglev set.</h3>
        {err && <p className="warning">{err}</p>}
        {rows.length === 0 && !err ? (
          <p>No backends reported.</p>
        ) : (
          <div className="investigation-table">
            <table>
              <thead>
                <tr>
                  <th>Status</th>
                  <th>Backend</th>
                  <th>VIP</th>
                  <th>Weight</th>
                  <th>Packets</th>
                  <th>Bytes</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((b) => (
                  <tr key={`${b.vip}-${b.id}-${b.address}:${b.port}`}>
                    <td>{b.healthy ? 'up' : 'down'}</td>
                    <td>
                      <code>
                        {b.address}:{b.port}
                      </code>
                    </td>
                    <td>
                      <code>{b.vip}</code>
                    </td>
                    <td>{b.weight ?? 1}</td>
                    <td>{(b.packets || 0).toLocaleString()}</td>
                    <td>{fmtBytes(b.bytes || 0)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </div>
  );
}
