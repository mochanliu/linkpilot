/* SPDX-License-Identifier: GPL-2.0 */
#include <linux/bpf.h>
#include <linux/in.h>

#include <linux/if_ether.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

// The parsing helper functions from the packet01 lesson have moved here
#include "../common/parsing_helpers.h"
#include "../common/rewrite_helpers.h"

/* Defines xdp_stats_map */
#include "../common/xdp_stats_kern_user.h"
//#include "../common/xdp_stats_kern.h"
#include <linux/pkt_cls.h>
#include <stdint.h>
#include <iproute2/bpf_elf.h>

#ifndef SEGLIST_MAX_DEPTH
#define SEGLIST_MAX_DEPTH 5
#endif

struct srouhdr {
        __u8    MagicNumber;
        __u8    FlowIDLen;
        __u8    ProtoID;
		__u8    SRLen;
		__be32  FlowID;
};

struct srousrh {
        __u8    SegType;
        __u8    LastEntry;
        __u8    SegLeft;
        __u8    SRHLen;
};

struct srlistv4{
	__be32 ip;
	__be16 port;
	__be16 arg;
};

struct sr{
		struct srouhdr srou;
		struct srlistv4 OriSrc;
		struct srousrh srh;
		struct srlistv4 seg0;
		struct srlistv4 seg1;
		struct srlistv4 seg2;
		struct srlistv4 seg3;
		struct srlistv4 seg4;
};

struct bpf_elf_map SEC("maps") receive_map = {
        .type           = BPF_MAP_TYPE_HASH,
        .size_key       = sizeof(uint16_t),
        .size_value     = sizeof(uint32_t),
        .pinning        = PIN_GLOBAL_NS,
        .max_elem       = 4096,
};

SEC("xdp_sdwan_receive")
int rcv(struct xdp_md *ctx)
{
	int action = XDP_PASS;
	int eth_type, ip_type;
	struct ethhdr *eth;
	struct iphdr *iphdr;
	struct udphdr *udphdr;
	void *data_end = (void *)(long)ctx->data_end;
	void *data = (void *)(long)ctx->data;
	struct hdr_cursor nh = { .pos = data };

	eth_type = parse_ethhdr(&nh, data_end, &eth);
	if (eth_type < 0) {
		action = XDP_ABORTED;
		goto out;
	}

	if (eth_type == bpf_htons(ETH_P_IP)) {
		ip_type = parse_iphdr(&nh, data_end, &iphdr);
	} else {
		goto out;
	}

	if (ip_type == 17) {
		if (parse_udphdr(&nh, data_end, &udphdr) < 0) {
			action = XDP_ABORTED;
			goto out;
		}
		__be32 *argument=bpf_map_lookup_elem(&receive_map, &udphdr->dest);
        if (argument){
			struct sr *insert;
			insert=nh.pos;
			if ((void *)(insert+1)>data_end)
				return XDP_ABORTED;
			if (*argument!=insert->srou.FlowID)
				return XDP_ABORTED;
 			if (insert->srou.ProtoID!=1)
				return XDP_ABORTED; 
			if (insert->srou.MagicNumber!=0)
				return XDP_ABORTED; 
			if ((insert->srou.SRLen-4-insert->srou.FlowIDLen)!=8)
				return XDP_ABORTED; 
			if (insert->srh.SegType!=1)
				return XDP_ABORTED;
			if (((insert->srh.LastEntry+1)*8+4)!=insert->srh.SRHLen)
				return XDP_ABORTED; 
			if (insert->srh.SegLeft == 0){
 				iphdr->tot_len=bpf_htons(bpf_ntohs(iphdr->tot_len)-sizeof(*insert));
				udphdr->len=bpf_htons(bpf_ntohs(udphdr->len)-sizeof(*insert));
				__u32 sum=~bpf_ntohs(iphdr->check);
				sum=(sum<<16)-sizeof(*insert);
				sum=(sum>>16)+(sum &0xffff);
				sum=(sum>>16)+(sum &0xffff);
				iphdr->check=bpf_htons(~(__u16)sum);
 				sum=bpf_csum_diff(&iphdr->saddr, 4, &insert->OriSrc.ip, 4,~iphdr->check);
				sum=(sum>>16)+(sum &0xffff);
				sum=(sum>>16)+(sum &0xffff);
				iphdr->check=~(__u16)sum;
				iphdr->saddr=insert->OriSrc.ip;
				udphdr->source=insert->OriSrc.port;
				struct ethhdr a;
				struct iphdr b;
				struct udphdr c;
				__builtin_memcpy(&a,  eth, sizeof(a));
				__builtin_memcpy(&b, iphdr, sizeof(b));
				__builtin_memcpy(&c, udphdr, sizeof(c));
				if (bpf_xdp_adjust_head(ctx, (sizeof(a)+sizeof(b)+sizeof(c)+sizeof(*insert)))<0){
					action=XDP_ABORTED; 
					goto out;
				}
				if (bpf_xdp_adjust_head(ctx, (0-(int)(sizeof(a)+sizeof(b)+sizeof(c))))<0){
					action=XDP_ABORTED; 
					goto out;
				}
				data_end = (void *)(long)ctx->data_end;
				data = (void *)(long)ctx->data;
				nh.pos=data;
				eth=nh.pos;
				if ((void *)(eth+1)>data_end)
					return -1;
				__builtin_memcpy(eth,&a,   sizeof(a));
				nh.pos+=sizeof(*eth);
				iphdr=nh.pos;
				if ((void *)(iphdr+1)>data_end)
					return -1;
				__builtin_memcpy(iphdr, &b, sizeof(b));
				nh.pos+=sizeof(*iphdr);
				udphdr=nh.pos;
				if ((void *)(udphdr+1)>data_end)
					return -1;
				__builtin_memcpy( udphdr,&c, sizeof(c));
				udphdr->check=0x0;
				return XDP_PASS;
			}
			else{
				if (insert->srh.LastEntry==insert->srh.SegLeft){
					insert->OriSrc.ip=iphdr->saddr;
					insert->OriSrc.port=udphdr->source;
				}
				swap_src_dst_mac(eth);
				iphdr->saddr=iphdr->daddr;
				udphdr->source=udphdr->dest;
				insert->srh.SegLeft--;
				nh.pos+=(insert->srou.SRLen+4+insert->srh.SegLeft*8);
				if (nh.pos+4>data_end){
					return XDP_ABORTED; 
				} 
				__builtin_memcpy(&iphdr->daddr, nh.pos, 4);
				nh.pos+=4;
				if (nh.pos+2>data_end){
					return XDP_ABORTED; 
				} 
				__builtin_memcpy(&udphdr->dest, nh.pos,2);
				return XDP_TX;
			}
			
		}
	}
out:
	return action;
}