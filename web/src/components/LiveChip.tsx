import { useEffect, useState } from 'react';
import { api } from '../api';

export default function LiveChip({ onOpen }: { onOpen: () => void }) {
  const [label, setLabel] = useState('…');
  const [ok, setOk] = useState(false);

  useEffect(() => {
    let cancelled = false;
    const tick = () => {
      api<unknown[]>('/api/v1/vips')
        .then((vips) => {
          if (cancelled) return;
          const n = Array.isArray(vips) ? vips.length : 0;
          setLabel(`${n} VIP${n === 1 ? '' : 's'}`);
          setOk(true);
        })
        .catch(() => {
          if (cancelled) return;
          setLabel('offline');
          setOk(false);
        });
    };
    tick();
    const id = setInterval(tick, 5000);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, []);

  return (
    <button
      type="button"
      className="theme-toggle"
      onClick={onOpen}
      title="Dataplane status"
      aria-label={`Dataplane ${label}`}
      style={{ fontSize: 11, gap: 6, padding: '0 10px', width: 'auto', borderRadius: 980 }}
    >
      <span
        style={{
          width: 6,
          height: 6,
          borderRadius: '50%',
          background: ok ? 'var(--accent-green)' : 'var(--danger)',
          display: 'inline-block',
        }}
      />
      {label}
    </button>
  );
}
