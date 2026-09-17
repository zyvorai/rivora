import type {ReactNode} from 'react';
import Link from '@docusaurus/Link';
import Heading from '@theme/Heading';
import styles from './styles.module.css';

type FeatureItem = {
  title: string;
  description: ReactNode;
  to: string;
};

const FeatureList: FeatureItem[] = [
  {
    title: 'XDP / TCX dataplane',
    description:
      'Own programs and maps under /sys/fs/bpf/rivora-lb — DSR and full-NAT, Maglev, draining, optional SYN rate limiting. No CNI dependency.',
    to: '/docs/core-concepts/architecture',
  },
  {
    title: 'IPv4 and IPv6',
    description:
      'First-class dual-stack: sparse /64 IPAM, NDP speaker, BGP /128 ads, and CI selftests on every push.',
    to: '/docs/core-concepts/ipv6',
  },
  {
    title: 'Kubernetes LB',
    description:
      'Service/EndpointSlice reconciler plus AddressPool IPAM. Works with Pods, KubeVirt VMIs, and hand-authored external EndpointSlices.',
    to: '/docs/kubernetes/overview',
  },
  {
    title: 'Gateway API (L4)',
    description:
      'Gateway + TCPRoute/UDPRoute alongside Services. HTTPRoute deliberately out of scope — no L7 in XDP.',
    to: '/docs/kubernetes/gateway-api',
  },
  {
    title: 'BGP / BFD HA',
    description:
      'Opt-in active/active ECMP: health-gated /32 and /128 host routes, dual AFI/SAFI, per-peer BFD.',
    to: '/docs/operations/bgp',
  },
  {
    title: 'Helm chart',
    description:
      'DaemonSet + controller + AddressPool CRD. Dual-stack pools, speaker, Gateway API, and BGP values.',
    to: '/docs/kubernetes/helm',
  },
];

function Feature({title, description, to}: FeatureItem) {
  return (
    <div className="col col--4">
      <Link to={to} className={styles.card}>
        <Heading as="h3">{title}</Heading>
        <p>{description}</p>
      </Link>
    </div>
  );
}

export default function FeatureHighlights(): ReactNode {
  return (
    <section className={styles.features}>
      <div className="container">
        <div className="row">
          {FeatureList.map((props, idx) => (
            <Feature key={idx} {...props} />
          ))}
        </div>
      </div>
    </section>
  );
}
