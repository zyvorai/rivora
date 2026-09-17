import { useEffect, useRef, useState } from 'react';
import { api } from '../api';
import { useCountUp } from '../hooks/useCountUp';
import Sparkline from '../components/Sparkline';
import type { VIPStatus } from '../types';
import { fmtBytes } from '../types';

const HISTORY_LEN = 30; // 30 samples @ 3s polling = 90s of visible history

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

/** Tracks the last cumulative (total, timestampMs) sample and turns each new
 * one into a per-second rate, since the API only ever reports monotonic
 * counters (see dataplane.Status's Packets/Bytes) — the rate is what makes
 * "what it's doing right now" visible rather than a slowly-climbing total. */
function useRateHistory(total: number) {
  const [history, setHistory] = useState<number[]>([]);
  const prevRef = useRef<{ total: number; t: number } | null>(null);

  useEffect(() => {
    const now = performance.now();
    const prev = prevRef.current;
    prevRef.current = { total, t: now };
    if (!prev) return;
    const dt = (now - prev.t) / 1000;
    if (dt <= 0) return;
    const rate = Math.max(0, (total - prev.total) / dt);
    setHistory((h) => [...h, rate].slice(-HISTORY_LEN));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [total]);

  return history;
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

  const packetRate = useRateHistory(packets);
  const byteRate = useRateHistory(bytes);
  const currentPPS = packetRate.length ? packetRate[packetRate.length - 1] : 0;
  const currentBPS = byteRate.length ? byteRate[byteRate.length - 1] : 0;

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

      <section className="card span2">
        <p className="eyebrow">LIVE THROUGHPUT</p>
        <h3>
          {Math.round(currentPPS).toLocaleString()} pkt/s · {fmtBytes(currentBPS)}/s
        </h3>
        <p>Sampled every 3s from the dataplane's own packet/byte counters — a flat line means no traffic is currently flowing.</p>
        <div style={{ display: 'flex', gap: 24, flexWrap: 'wrap', marginTop: 8 }}>
          <div>
            <Sparkline values={packetRate} color="var(--accent-cyan)" />
            <p className="eyebrow" style={{ marginTop: 4 }}>
              packets/sec
            </p>
          </div>
          <div>
            <Sparkline values={byteRate} color="var(--accent-purple)" />
            <p className="eyebrow" style={{ marginTop: 4 }}>
              bytes/sec
            </p>
          </div>
        </div>
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
