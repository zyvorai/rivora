const WIDTH = 240;
const HEIGHT = 48;

/** A minimal inline SVG line chart for a rolling window of samples — no
 * charting dependency, since this is the only place in the console that
 * needs one. */
export default function Sparkline({ values, color = 'var(--accent-cyan)' }: { values: number[]; color?: string }) {
  if (values.length < 2) {
    return <svg width={WIDTH} height={HEIGHT} aria-hidden="true" />;
  }
  const max = Math.max(...values, 1);
  const step = WIDTH / (values.length - 1);
  const points = values.map((v, i) => `${(i * step).toFixed(1)},${(HEIGHT - (v / max) * HEIGHT).toFixed(1)}`).join(' ');
  const areaPoints = `0,${HEIGHT} ${points} ${WIDTH},${HEIGHT}`;

  return (
    <svg width={WIDTH} height={HEIGHT} viewBox={`0 0 ${WIDTH} ${HEIGHT}`} role="img" aria-label="Throughput over time">
      <polygon points={areaPoints} fill={color} opacity={0.12} />
      <polyline points={points} fill="none" stroke={color} strokeWidth={2} strokeLinejoin="round" strokeLinecap="round" />
    </svg>
  );
}
