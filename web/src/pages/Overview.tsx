import { useEffect, useState } from 'react';
import { api } from '../api';
import { useCountUp } from '../hooks/useCountUp';
import type { VIPStatus } from '../types';
import { fmtBytes } from '../types';

function Metric({ value, label }: { value: number | string; label: string }) {
  const numeric = typeof value === 'number' && Number.isFinite(value);
  const animated = useCountUp(numeric ? (value as number) : 0);
  return (
    <div>
      <b>{numeric ? Math.round(animated).toLocaleString() : value}</b>
      <span>{label}</span>
    </div>
  );
}

export default function Overview() {
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

  const backends = vips.flatMap((v) => v.backends || []);
  const healthy = backends.filter((b) => b.healthy).length;
  const packets = vips.reduce((n, v) => n + (v.packets || 0), 0);
  const bytes = vips.reduce((n, v) => n + (v.bytes || 0), 0);
  const dropped = vips[0]?.dropped ?? 0;
  const iface = vips[0]?.interface || '—';

  return (
    <div className="grid">
      <section className="card span2">
        <p className="eyebrow">RIVORA DATAPLANE</p>
        <h3>Independent by default.</h3>
        <p>
          XDP ingress and TCX egress own VIP selection, Maglev hashing, and health-gated backends — CNI-independent, with
          maps under <code>/sys/fs/bpf/rivora-lb</code>.
        </p>
        <div className="metrics">
          <Metric value={vips.length} label="VIPs" />
          <Metric value={healthy} label="healthy backends" />
          <Metric value={packets} label="packets" />
          <Metric value={dropped} label="dropped" />
        </div>
        {err && <p className="warning">{err}</p>}
      </section>

      <section className="card">
        <p className="eyebrow">NODE</p>
        <h3>Attachment.</h3>
        <p>
          Interface <code>{iface}</code> · {fmtBytes(bytes)} forwarded · up since{' '}
          {vips[0]?.startedAt ? new Date(vips[0].startedAt).toLocaleString() : '—'}
        </p>
      </section>

      <section className="card">
        <p className="eyebrow">MODES</p>
        <h3>Forwarding.</h3>
        <p>
          {vips.length === 0
            ? 'No VIPs configured.'
            : [...new Set(vips.map((v) => (v.mode || '').toUpperCase()))].join(' · ') || '—'}
        </p>
      </section>
    </div>
  );
}
