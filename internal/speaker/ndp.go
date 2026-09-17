// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package speaker

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/mdlayher/ndp"
)

// startNDP dials an NDP conn on the speaker's interface. Soft-fails when
// the interface has no IPv6 link-local — common on v4-only lab NICs — so
// ARP-only deployments keep working.
func (s *Speaker) startNDP() (*ndp.Conn, error) {
	conn, _, err := ndp.Listen(s.iface, ndp.LinkLocal)
	if err != nil {
		return nil, fmt.Errorf("ndp listen on %s: %w", s.iface.Name, err)
	}
	return conn, nil
}

func (s *Speaker) ndpSyncAndAnnounce(conn *ndp.Conn) {
	statuses, err := s.source.Statuses()
	if err != nil {
		s.logger.Error("list vip statuses for ndp announce", "err", err)
		return
	}
	vips := natVIP6(statuses)
	// Join each VIP's solicited-node multicast so we receive Neighbor
	// Solicitations targeting it (unsolicited NAs alone don't cover that).
	for _, addr := range vips {
		snm, err := ndp.SolicitedNodeMulticast(addr)
		if err != nil {
			continue
		}
		_ = conn.JoinGroup(snm) // idempotent enough; Leave on VIP removal is best-effort later
		if err := s.ndpAdvertisement(conn, addr, netip.IPv6LinkLocalAllNodes()); err != nil {
			s.logger.Error("send unsolicited neighbor advertisement", "vip", addr, "err", err)
		}
	}
}

func (s *Speaker) ndpAdvertisement(conn *ndp.Conn, target, dst netip.Addr) error {
	solicited := dst != netip.IPv6LinkLocalAllNodes()
	msg := &ndp.NeighborAdvertisement{
		Solicited:     solicited,
		Override:      true,
		TargetAddress: target,
		Options: []ndp.Option{
			&ndp.LinkLayerAddress{
				Direction: ndp.Target,
				Addr:      s.iface.HardwareAddr,
			},
		},
	}
	return conn.WriteTo(msg, nil, dst)
}

func (s *Speaker) ndpRespondLoop(ctx context.Context, conn *ndp.Conn) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		msg, _, from, err := conn.ReadFrom()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			s.logger.Error("read ndp packet", "err", err)
			continue
		}
		ns, ok := msg.(*ndp.NeighborSolicitation)
		if !ok {
			continue
		}
		if !s.owns6(ns.TargetAddress) {
			continue
		}
		if err := s.ndpAdvertisement(conn, ns.TargetAddress, from); err != nil {
			s.logger.Error("reply to neighbor solicitation", "target", ns.TargetAddress, "err", err)
		}
	}
}

func (s *Speaker) owns6(addr netip.Addr) bool {
	statuses, err := s.source.Statuses()
	if err != nil {
		return false
	}
	for _, owned := range natVIP6(statuses) {
		if owned == addr {
			return true
		}
	}
	return false
}
