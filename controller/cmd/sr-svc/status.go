package main

// sr-svc's local status page: leases, row counts, counters. 127.0.0.1 only --
// the lease table says which customer MAC has which address, and that belongs
// to the box, not the LAN.
//
// The controller does not poll this in v1 (devices are behind NAT and the
// agent's heartbeat is the only northbound channel); the page exists for the
// operator on the box and for the next telemetry hop to scrape.

import (
	"encoding/json"
	"log"
	"net/http"
)

func httpStatusMux(s *server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		s.leaseMu.Lock()
		leases := make([]*lease, 0, len(s.leases))
		for _, l := range s.leases {
			leases = append(leases, l)
		}
		s.leaseMu.Unlock()
		s.mu.Lock()
		view := statusView{
			DHCPRows: len(s.cfg.DHCP),
			DNSRows:  len(s.cfg.DNS),
			DHCPReq:  s.dhcpReq,
			DHCPAck:  s.dhcpAck,
			DNSQuery: s.dnsQuery,
			DNSReply: s.dnsReply,
			DNSFail:  s.dnsFail,
			Leases:   leases,
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(view)
	})
	return mux
}

func serveStatus(mux *http.ServeMux) error {
	srv := &http.Server{Addr: statusAddr, Handler: mux}
	srv.SetKeepAlivesEnabled(false)
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	if err != nil {
		log.Printf("状态口起不来（%s）: %v", statusAddr, err)
		return nil // 状态口不是生命线：起不来也要照常服务
	}
	return nil
}
