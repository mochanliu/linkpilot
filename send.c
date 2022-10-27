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

struct bpf_elf_map SEC("maps") send_map = {
        .type           = BPF_MAP_TYPE_HASH,
        .size_key       = sizeof(uint16_t),
        .size_value     = 56,
        .pinning        = PIN_GLOBAL_NS,
        .max_elem       = 256,
};

SEC("classifier_sdwan_send")
int snd(struct __sk_buff *skb)
{
	int eth_type, ip_type;
	struct ethhdr *eth;
	struct iphdr *iphdr;
	struct udphdr *udphdr;
	void *data_end = (void *)(long)skb->data_end;
	void *data = (void *)(long)skb->data;
	struct hdr_cursor nh = { .pos = data };

	eth_type = parse_ethhdr(&nh, data_end, &eth);
	if (eth_type == bpf_htons(ETH_P_IP)) {
		ip_type = parse_iphdr(&nh, data_end, &iphdr);
	} else {
		goto out;
	}

	if (ip_type == IPPROTO_UDP) {
		if (parse_udphdr(&nh, data_end, &udphdr)<0){
			goto out;
		}
		struct sr *insert=bpf_map_lookup_elem(&send_map, &udphdr->source);
        if (insert){
			struct udphdr u;
			insert->seg0.ip=iphdr->daddr;
			insert->seg0.port=udphdr->dest;
			if (insert->srh.LastEntry==1){
				__u32 sum=bpf_csum_diff(&iphdr->daddr, 4, &insert->seg1.ip, 4, ~iphdr->check);
				sum=(sum>>16)+(sum &0xffff);
				sum=(sum>>16)+(sum &0xffff);
				iphdr->check=~sum;
				iphdr->daddr=insert->seg1.ip;
				udphdr->dest=insert->seg1.port;
			}else if(insert->srh.LastEntry==2){
				__u32 sum=bpf_csum_diff(&iphdr->daddr, 4, &insert->seg2.ip, 4, ~iphdr->check);
				sum=(sum>>16)+(sum &0xffff);
				sum=(sum>>16)+(sum &0xffff);
				iphdr->check=~sum;
				iphdr->daddr=insert->seg2.ip;
				udphdr->dest=insert->seg2.port;
			}else if(insert->srh.LastEntry==3){
				__u32 sum=bpf_csum_diff(&iphdr->daddr, 4, &insert->seg3.ip, 4, ~iphdr->check);
				sum=(sum>>16)+(sum &0xffff);
				sum=(sum>>16)+(sum &0xffff);
				iphdr->check=~sum;
				iphdr->daddr=insert->seg3.ip;
				udphdr->dest=insert->seg3.port;
			}else{
				__u32 sum=bpf_csum_diff(&iphdr->daddr, 4, &insert->seg4.ip, 4, ~iphdr->check);
				sum=(sum>>16)+(sum &0xffff);
				sum=(sum>>16)+(sum &0xffff);
				iphdr->check=~sum;
				iphdr->daddr=insert->seg4.ip;
				udphdr->dest=insert->seg4.port;
			}
			__u32 srsize=sizeof(*insert);
 			__be16 preiplen=iphdr->tot_len;
			iphdr->tot_len=bpf_htons(bpf_ntohs(iphdr->tot_len)+srsize);
			__be16 postiplen=iphdr->tot_len; 
			udphdr->len=bpf_htons(bpf_ntohs(udphdr->len)+srsize);
			udphdr->check=0x0;
			__builtin_memcpy(&u, udphdr, sizeof(u));
  			if (bpf_l3_csum_replace(skb, sizeof(*eth)+10, preiplen, postiplen, 2)<0)
				return -1; 
 			if (bpf_skb_adjust_room(skb,srsize, BPF_ADJ_ROOM_NET, 0)<0)
				return -1; 
			data_end = (void *)(long)skb->data_end;
			data = (void *)(long)skb->data;
			nh.pos=data;
			eth=nh.pos;
			if ((void *)(eth+1)>data_end)
				return -1;
			nh.pos+=sizeof(*eth);
			iphdr=nh.pos;
			if ((void *)(iphdr+1)>data_end)
				return -1;
			nh.pos+=sizeof(*iphdr);
			udphdr=nh.pos;
			if ((void *)(udphdr+1)>data_end)
				return -1;
			__builtin_memcpy(nh.pos, &u, sizeof(u));
 			nh.pos+=sizeof(*udphdr);
			if (nh.pos+srsize>data_end)
				return -1;
			if (bpf_skb_store_bytes(skb,sizeof(*iphdr)+sizeof(*eth)+sizeof(*udphdr) ,  insert, srsize, 0)<0)
				return -1; 
        }
	}

out:
	return 0;
}