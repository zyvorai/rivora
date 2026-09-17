import type {ReactNode} from 'react';
import clsx from 'clsx';
import Link from '@docusaurus/Link';
import useBaseUrl from '@docusaurus/useBaseUrl';
import Layout from '@theme/Layout';
import Heading from '@theme/Heading';
import FeatureHighlights from '@site/src/components/FeatureHighlights';
import Reveal from '@site/src/components/Reveal';

import styles from './index.module.css';

function HomepageHeader() {
  const shareCard = useBaseUrl('/rivora-share-card.png');
  return (
    <header className={clsx('hero hero--primary', styles.heroBanner)}>
      <div className="container">
        <div className={styles.heroGrid}>
          <div>
            <Heading as="h1" className="hero__title">
              eBPF-native
              <br />
              load balancing
              <br />
              for every environment.
            </Heading>
            <p className="hero__subtitle">
              Rivora owns VIPs, Maglev backend selection, health checks, and
              NAT/DSR — CNI-independent XDP/TCX on Linux and Kubernetes, with
              first-class IPv6, ARP+NDP announce, and opt-in BGP/BFD HA.
            </p>
            <div className={styles.buttons}>
              <Link
                className="button button--secondary button--lg"
                to="/docs/getting-started/quickstart">
                Get Started
              </Link>
              <Link
                className="button button--outline button--lg button--secondary"
                to="https://github.com/zyvorai/rivora">
                View on GitHub
              </Link>
            </div>
          </div>
          <div className={styles.heroMedia}>
            <img
              src={shareCard}
              alt="Rivora — eBPF-native load balancer for Kubernetes and bare metal"
            />
          </div>
        </div>
      </div>
    </header>
  );
}

function ProblemStatement() {
  return (
    <section className={styles.problem}>
      <div className="container">
        <Reveal className="row">
          <div className="col col--8 col--offset-2 text--center">
            <Heading as="h2" className={styles.sectionHeading}>
              Own the traffic-delivery layer
            </Heading>
            <p>
              Rivora attaches its own XDP/TCX programs and pins maps under{' '}
              <code>/sys/fs/bpf/rivora-lb</code>. It does not require Cilium or
              any particular CNI. Drive VIPs from static YAML on a single node,
              or from Kubernetes <code>Service</code>/<code>EndpointSlice</code>{' '}
              (and optional Gateway API) via <code>rivorad -kubernetes</code>{' '}
              with <code>rivora-controller</code> handling AddressPool IPAM.
            </p>
            <p>
              IPv6 is a peer of IPv4 across the dataplane, sparse IPAM, NDP
              speaker, and BGP <code>/128</code> advertisements — covered by CI
              selftests on every push.
            </p>
          </div>
        </Reveal>
      </div>
    </section>
  );
}

function TrustBand() {
  return (
    <section className={styles.trust}>
      <div className="container">
        <Reveal className={styles.trustGrid}>
          <div>
            <Heading as="h3" className={styles.sectionHeading}>
              Open core, tested in CI
            </Heading>
            <p>
              Apache-2.0. Every push runs Go <code>-race</code>, Helm render
              (including IPv6 pools and Gateway API), AddressPool CRD checks,
              BPF compile, and root selftests for DSR/NAT, Maglev, rate limit,
              IPv6 dataplane, and NDP.
            </p>
            <Link to="/docs/operations/selftests-ci">
              See the selftest and CI matrix →
            </Link>
          </div>
          <div className={styles.trustBadges}>
            <img
              src="https://github.com/zyvorai/rivora/actions/workflows/ci.yml/badge.svg"
              alt="CI status"
            />
            <img
              src="https://img.shields.io/badge/License-Apache%202.0-blue.svg"
              alt="Apache 2.0 license"
            />
          </div>
        </Reveal>
      </div>
    </section>
  );
}

function EnterpriseCTA() {
  return (
    <section className={styles.enterprise}>
      <div className="container text--center">
        <Reveal>
          <Heading as="h2" className={styles.sectionHeading}>
            Need production support or SLAs?
          </Heading>
          <p className={styles.enterpriseCopy}>
            Rivora&apos;s core is Apache-2.0 and free to run in production.
            Zyvor Enterprise adds support contracts, SLAs, and additional
            products for teams that need them.
          </p>
          <Link
            className="button button--primary button--lg"
            to="mailto:sales@zyvor.dev">
            Contact sales@zyvor.dev
          </Link>
        </Reveal>
      </div>
    </section>
  );
}

export default function Home(): ReactNode {
  return (
    <Layout
      title="Rivora — eBPF-native load balancing for every environment"
      description="eBPF-native load balancing for Kubernetes and bare metal: XDP/TCX, Maglev, IPv6, ARP+NDP, Gateway API, and opt-in BGP/BFD HA.">
      <HomepageHeader />
      <main>
        <ProblemStatement />
        <Reveal>
          <FeatureHighlights />
        </Reveal>
        <TrustBand />
        <EnterpriseCTA />
      </main>
    </Layout>
  );
}
