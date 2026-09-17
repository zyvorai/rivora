import { useEffect, useState } from 'react';
import { api } from '../api';
import type { VIPStatus } from '../types';
import { fmtBytes } from '../types';

export default function VIPs() {
  const [vips, setVips] = useState<VIPStatus[]>([]);
  const [err, setErr] = useState('');

  useEffect(() => {
    let cancelled = false;
    const load = () =>
      api<VIPStatus[]>('/api/v1/vips')
        .then((v) => {
          if (cancelled) return;
          setVips(Array.isArray(v) ? v : []);
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
      {err && (
        <section className="card span2">
          <p className="warning">{err}</p>
        </section>
      )}
      {vips.length === 0 && !err && (
        <section className="card span2">
          <p className="eyebrow">VIRTUAL IPS</p>
          <h3>Nothing programmed yet.</h3>
          <p>Apply a static YAML config or run with <code>-kubernetes</code> to populate VIPs.</p>
        </section>
      )}
      {vips.map((v) => {
        const backends = v.backends || [];
        const up = backends.filter((b) => b.healthy).length;
        return (
          <section className="card span2" key={`${v.vipAddress}:${v.vipPort}:${v.protocol}`}>
            <p className="eyebrow">
              {(v.protocol || '').toUpperCase()} · {(v.mode || '').toUpperCase()}
            </p>
            <h3>
              {v.vipAddress}:{v.vipPort}
            </h3>
            <p>
              {up}/{backends.length} backends healthy · {v.packets.toLocaleString()} packets · {fmtBytes(v.bytes)} ·
              iface <code>{v.interface}</code>
            </p>
            <div className="metrics">
              <div>
                <b>{up}</b>
                <span>up</span>
              </div>
              <div>
                <b>{backends.length - up}</b>
                <span>down</span>
              </div>
              <div>
                <b>{v.dropped.toLocaleString()}</b>
                <span>dropped (node)</span>
              </div>
            </div>
          </section>
        );
      })}
    </div>
  );
}
