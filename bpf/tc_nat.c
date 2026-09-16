// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
//
// Rivora TCX egress: reverses the full-NAT rewrite xdp_ingress applied on
// the way in. v0.1 runs client and backends on the same single interface
// (one-armed NAT), so this attaches to that same interface's egress path —
// it rewrites the backend's reply (src = backend:port) back to
// (src = VIP:port) right before the packet leaves, using the reverse
// mapping xdp_ingress wrote to nat_reverse_map. DSR-mode traffic never
// touches this program (the backend replies directly using the VIP as its
// own source address).

#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/tcp.h>
#include <linux/udp.h>
#include <linux/in.h>
#include <linux/pkt_cls.h>

#include "rivora_common.h"

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 65536);
    __type(key, struct nat_reverse_key);
    __type(value, struct nat_reverse_val);
} nat_reverse_map SEC(".maps");

SEC("tc")
int rivora_tc_nat_egress(struct __sk_buff *skb)
{
    void *data = (void *)(long)skb->data;
    void *data_end = (void *)(long)skb->data_end;

    struct ethhdr *eth = data;
    if ((void *)(eth + 1) > data_end)
        return TC_ACT_OK;
    if (eth->h_proto != __constant_htons(ETH_P_IP))
        return TC_ACT_OK;

    struct iphdr *iph = (void *)(eth + 1);
    if ((void *)(iph + 1) > data_end)
        return TC_ACT_OK;
    if (iph->ihl != 5)
        return TC_ACT_OK;
    if (iph->protocol != IPPROTO_TCP_ && iph->protocol != IPPROTO_UDP_)
        return TC_ACT_OK;

    void *l4 = (void *)iph + (iph->ihl * 4);
    struct tcphdr *tcph = NULL;
    struct udphdr *udph = NULL;
    __u16 sport, dport;
    __u16 *l4_csum;

    if (iph->protocol == IPPROTO_TCP_) {
        tcph = l4;
        if ((void *)(tcph + 1) > data_end)
            return TC_ACT_OK;
        sport = tcph->source;
        dport = tcph->dest;
        l4_csum = &tcph->check;
    } else {
        udph = l4;
        if ((void *)(udph + 1) > data_end)
            return TC_ACT_OK;
        sport = udph->source;
        dport = udph->dest;
        l4_csum = &udph->check;
    }

    /* This is the backend->client leg: src = backend, dst = client. */
    struct nat_reverse_key rk = {
        .backend_addr = iph->saddr, .client_addr = iph->daddr,
        .backend_port = sport, .client_port = dport, .proto = iph->protocol,
    };
    struct nat_reverse_val *rv = bpf_map_lookup_elem(&nat_reverse_map, &rk);
    if (!rv)
        return TC_ACT_OK; /* not a flow we NAT'd (or affinity already expired) */

    __u32 old_saddr = iph->saddr;
    __u32 new_saddr = rv->vip_addr;
    __u16 old_sport = sport;
    __u16 new_sport = rv->vip_port;

    csum_replace(&iph->check, &old_saddr, &new_saddr, 4);
    if (*l4_csum != 0) {
        csum_replace(l4_csum, &old_saddr, &new_saddr, 4);
        csum_replace(l4_csum, &old_sport, &new_sport, 2);
    }
    iph->saddr = new_saddr;
    if (tcph)
        tcph->source = new_sport;
    else
        udph->source = new_sport;

    return TC_ACT_OK;
}

char _license[] SEC("license") = "GPL";
