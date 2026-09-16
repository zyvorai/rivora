// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package speaker is rivorad's L2 ARP responder for K8s-managed (NAT-mode)
// VIPs: gratuitous ARP on assignment/refresh, and an ARP-request responder
// for VIPs this node currently owns. DSR-mode VIPs aren't handled here —
// see natVIPAddresses.
//
// Exactly one node in the cluster may answer ARP for a given VIP at a time
// (answering from two nodes at once is a silent traffic-split outage, not
// a soft degradation), so — unlike internal/controller's per-node
// dataplane mirror, which every node runs independently — the speaker
// runs behind a single cluster-wide coordination.k8s.io/v1 Lease
// (active/passive whole-node failover; per-VIP leadership is a v0.3+
// enhancement, not this milestone).
package speaker

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/mdlayher/arp"
	"github.com/mdlayher/ethernet"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/zyvorai/rivora/internal/dataplane"
)

const (
	leaseName     = "rivora-speaker"
	leaseDuration = 15 * time.Second
	renewDeadline = 10 * time.Second
	retryPeriod   = 2 * time.Second
	// announceInterval is the periodic gratuitous-ARP refresh (a switch's
	// FDB / a peer's ARP cache entry for the VIP can time out even with no
	// membership change) on top of the immediate announce AnnounceNow
	// triggers on every dataplane change.
	announceInterval = 30 * time.Second
)

// vipSource is the subset of *dataplane.Dataplane the speaker needs.
type vipSource interface {
	Statuses() ([]dataplane.Status, error)
}

var _ vipSource = (*dataplane.Dataplane)(nil)

// Speaker owns one ARP client for iface and, only while holding the
// cluster-wide Lease, answers ARP requests for and periodically
// re-announces every NAT-mode VIP source currently reports.
type Speaker struct {
	iface     *net.Interface
	client    *arp.Client
	source    vipSource
	clientset kubernetes.Interface
	namespace string
	identity  string
	logger    *slog.Logger

	announceNow chan struct{}
}

// New dials an ARP client on iface. Requires CAP_NET_RAW (or root) — the
// same privilege level rivorad's XDP/TCX attach already needs.
func New(iface *net.Interface, source vipSource, clientset kubernetes.Interface, namespace, identity string, logger *slog.Logger) (*Speaker, error) {
	client, err := arp.Dial(iface)
	if err != nil {
		return nil, err
	}
	return &Speaker{
		iface:       iface,
		client:      client,
		source:      source,
		clientset:   clientset,
		namespace:   namespace,
		identity:    identity,
		logger:      logger,
		announceNow: make(chan struct{}, 1),
	}, nil
}

func (s *Speaker) Close() error { return s.client.Close() }

// AnnounceNow requests an out-of-cycle gratuitous-ARP pass (rivorad calls
// this from internal/controller's OnChange hook, so a newly-assigned or
// reassigned VIP doesn't wait for the next announceInterval tick to be
// reachable). Non-blocking: a pass already pending coalesces with this one.
func (s *Speaker) AnnounceNow() {
	select {
	case s.announceNow <- struct{}{}:
	default:
	}
}

// Run blocks until ctx is cancelled, participating in the cluster-wide
// speaker Lease and — only while leading — answering ARP and announcing.
func (s *Speaker) Run(ctx context.Context) error {
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{Name: leaseName, Namespace: s.namespace},
		Client:    s.clientset.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: s.identity,
		},
	}
	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   leaseDuration,
		RenewDeadline:   renewDeadline,
		RetryPeriod:     retryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: s.lead,
			OnStoppedLeading: func() {
				s.logger.Info("lost speaker leadership", "identity", s.identity)
			},
			OnNewLeader: func(newIdentity string) {
				if newIdentity != s.identity {
					s.logger.Info("observed new speaker leader", "leader", newIdentity)
				}
			},
		},
	})
	if err != nil {
		return err
	}
	elector.Run(ctx)
	return nil
}

func (s *Speaker) lead(ctx context.Context) {
	s.logger.Info("acquired speaker leadership", "identity", s.identity)
	go s.respondLoop(ctx)
	s.announceLoop(ctx)
}

// announceLoop sends gratuitous ARP for every currently-owned NAT VIP,
// both periodically and whenever AnnounceNow is signaled, until ctx is
// cancelled (typically because leadership was lost).
func (s *Speaker) announceLoop(ctx context.Context) {
	ticker := time.NewTicker(announceInterval)
	defer ticker.Stop()

	s.announceAll()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.announceAll()
		case <-s.announceNow:
			s.announceAll()
		}
	}
}

func (s *Speaker) announceAll() {
	statuses, err := s.source.Statuses()
	if err != nil {
		s.logger.Error("list vip statuses for announce", "err", err)
		return
	}
	for _, addr := range natVIPAddresses(statuses) {
		if err := s.gratuitous(addr); err != nil {
			s.logger.Error("send gratuitous arp", "vip", addr, "err", err)
		}
	}
}

func (s *Speaker) gratuitous(addr netip.Addr) error {
	pkt, err := arp.NewPacket(arp.OperationReply, s.iface.HardwareAddr, addr, ethernet.Broadcast, addr)
	if err != nil {
		return err
	}
	return s.client.WriteTo(pkt, ethernet.Broadcast)
}

// respondLoop answers ARP requests targeting a VIP this node currently
// owns. Runs only while leading (see lead); a request for a VIP this node
// doesn't currently have is silently ignored — some other node (or no
// node, if the Service has no assigned address yet) is authoritative.
func (s *Speaker) respondLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_ = s.client.SetReadDeadline(time.Now().Add(time.Second))
		pkt, _, err := s.client.Read()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			s.logger.Error("read arp packet", "err", err)
			continue
		}
		if pkt.Operation != arp.OperationRequest {
			continue
		}
		if !s.owns(pkt.TargetIP) {
			continue
		}
		if err := s.client.Reply(pkt, s.iface.HardwareAddr, pkt.TargetIP); err != nil {
			s.logger.Error("reply to arp request", "target", pkt.TargetIP, "err", err)
		}
	}
}

func (s *Speaker) owns(addr netip.Addr) bool {
	statuses, err := s.source.Statuses()
	if err != nil {
		return false
	}
	for _, owned := range natVIPAddresses(statuses) {
		if owned == addr {
			return true
		}
	}
	return false
}
