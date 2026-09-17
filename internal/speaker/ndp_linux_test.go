// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package speaker

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/mdlayher/ndp"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/zyvorai/rivora/internal/dataplane"
)

type ndpFakeSource struct {
	statuses []dataplane.Status
}

func (f *ndpFakeSource) Statuses() ([]dataplane.Status, error) {
	out := make([]dataplane.Status, len(f.statuses))
	copy(out, f.statuses)
	return out, nil
}

// TestNDPRespondsOnVeth proves the speaker's NDP path: an unsolicited NA
// is sent for a NAT-mode IPv6 VIP, and a Neighbor Solicitation for that
// VIP elicits a solicited NA. Requires root (raw ICMPv6 + veth create).
func TestNDPRespondsOnVeth(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root required for NDP/veth selftest")
	}

	const (
		aName = "rvndp-a"
		bName = "rvndp-b"
		vip   = "fd00:99::100"
	)
	cleanup := func() {
		_ = exec.Command("ip", "link", "del", aName).Run()
	}
	cleanup()
	t.Cleanup(cleanup)

	if out, err := exec.Command("ip", "link", "add", aName, "type", "veth", "peer", "name", bName).CombinedOutput(); err != nil {
		t.Fatalf("create veth: %v (%s)", err, out)
	}
	for _, name := range []string{aName, bName} {
		if out, err := exec.Command("sysctl", "-w", "net.ipv6.conf."+name+".accept_dad=0").CombinedOutput(); err != nil {
			t.Fatalf("disable dad on %s: %v (%s)", name, err, out)
		}
		if out, err := exec.Command("ip", "link", "set", name, "up").CombinedOutput(); err != nil {
			t.Fatalf("up %s: %v (%s)", name, err, out)
		}
	}
	// arp.Dial (used by speaker.New) requires an IPv4 address on the
	// iface; give each side v4 + ULA so ARP and NDP both have L3 context.
	// The VIP itself is only "owned" via vipSource, not assigned on-link.
	if out, err := exec.Command("ip", "addr", "add", "10.255.99.1/30", "dev", aName).CombinedOutput(); err != nil {
		t.Fatalf("addr4 a: %v (%s)", err, out)
	}
	if out, err := exec.Command("ip", "addr", "add", "10.255.99.2/30", "dev", bName).CombinedOutput(); err != nil {
		t.Fatalf("addr4 b: %v (%s)", err, out)
	}
	if out, err := exec.Command("ip", "-6", "addr", "add", "fd00:99::1/64", "dev", aName, "nodad").CombinedOutput(); err != nil {
		t.Fatalf("addr6 a: %v (%s)", err, out)
	}
	if out, err := exec.Command("ip", "-6", "addr", "add", "fd00:99::2/64", "dev", bName, "nodad").CombinedOutput(); err != nil {
		t.Fatalf("addr6 b: %v (%s)", err, out)
	}
	time.Sleep(200 * time.Millisecond)

	ifaceA, err := net.InterfaceByName(aName)
	if err != nil {
		t.Fatal(err)
	}
	ifaceB, err := net.InterfaceByName(bName)
	if err != nil {
		t.Fatal(err)
	}

	src := &ndpFakeSource{statuses: []dataplane.Status{{
		VIPAddress: vip,
		Mode:       "nat",
		Protocol:   "tcp",
	}}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sp, err := New(ifaceA, src, fake.NewSimpleClientset(), "default", "ndp-selftest", logger)
	if err != nil {
		t.Fatalf("speaker.New: %v", err)
	}
	defer sp.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Skip Lease: production uses Run(); this exercises ARP+NDP loops only.
	go sp.lead(ctx)
	time.Sleep(500 * time.Millisecond) // allow unsolicited NA + JoinGroup

	connB, _, err := ndp.Listen(ifaceB, ndp.LinkLocal)
	if err != nil {
		t.Fatalf("ndp listen on b: %v", err)
	}
	defer connB.Close()

	target := netip.MustParseAddr(vip)
	snm, err := ndp.SolicitedNodeMulticast(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := connB.JoinGroup(snm); err != nil {
		t.Fatalf("join solicited-node: %v", err)
	}

	ns := &ndp.NeighborSolicitation{
		TargetAddress: target,
		Options: []ndp.Option{
			&ndp.LinkLayerAddress{Direction: ndp.Source, Addr: ifaceB.HardwareAddr},
		},
	}
	if err := connB.WriteTo(ns, nil, snm); err != nil {
		t.Fatalf("send NS: %v", err)
	}

	_ = connB.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		msg, _, _, err := connB.ReadFrom()
		if err != nil {
			t.Fatalf("waiting for NA: %v", err)
		}
		na, ok := msg.(*ndp.NeighborAdvertisement)
		if !ok {
			continue
		}
		if na.TargetAddress == target {
			return
		}
	}
}
