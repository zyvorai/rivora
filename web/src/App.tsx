import { useEffect, useState } from 'react';
import Nav, { type Page } from './components/Nav';
import PageHero, { type HeroTint } from './components/PageHero';
import Login from './components/Login';
import Overview from './pages/Overview';
import VIPs from './pages/VIPs';
import Backends from './pages/Backends';
import { logout } from './auth';
import { applyTheme, readStoredTheme, toggleTheme, type Theme } from './theme';

const pageHero: Partial<Record<Page, { eyebrow: string; title: string; lede: string; tint?: HeroTint }>> = {
  vips: {
    eyebrow: 'Dataplane',
    title: 'Every VIP this node owns.',
    lede: 'Mode, protocol, health, and counters from rivorad’s live map state — the same surface as rivoractl vips.',
    tint: 'green',
  },
  backends: {
    eyebrow: 'Dataplane',
    title: 'Backend health and weight.',
    lede: 'Maglev members with packet/byte counters. Unhealthy backends are drained from new flows.',
    tint: 'amber',
  },
};

export default function App() {
  const [page, setPage] = useState<Page>('overview');
  const [loggedIn, setLoggedIn] = useState(false);
  const [theme, setTheme] = useState<Theme>(() => {
    const t = readStoredTheme();
    applyTheme(t);
    return t;
  });

  useEffect(() => {
    const onExpired = () => setLoggedIn(false);
    window.addEventListener('rivora-auth-expired', onExpired);
    return () => window.removeEventListener('rivora-auth-expired', onExpired);
  }, []);

  if (!loggedIn) return <Login onLogin={() => setLoggedIn(true)} />;

  const body = {
    overview: <Overview />,
    vips: <VIPs />,
    backends: <Backends />,
  }[page];

  const hero = pageHero[page];

  return (
    <>
      <Nav
        page={page}
        setPage={setPage}
        theme={theme}
        onToggleTheme={() => setTheme((t) => toggleTheme(t))}
        onLogout={() => {
          logout();
          setLoggedIn(false);
        }}
      />
      <main>
        <div key={page}>
          {page === 'overview' ? (
            <header className="hero">
              <div>
                <p className="eyebrow">eBPF-NATIVE LOAD BALANCING</p>
                <h1>Own the VIP. Steer the backends.</h1>
                <p>
                  Rivora runs its own XDP/TCX datapath for Maglev selection, DSR and full-NAT, and health-gated backends —
                  Kubernetes or bare metal.
                </p>
              </div>
            </header>
          ) : (
            hero && <PageHero eyebrow={hero.eyebrow} title={hero.title} lede={hero.lede} tint={hero.tint} />
          )}
          {body}
        </div>
      </main>
    </>
  );
}
