package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type Machine struct {
	Name   string `json:"name"`
	DNS    string `json:"dns"`
	IP     string `json:"ip"`
	OS     string `json:"os"`
	Online bool   `json:"online"`
}

type tailnet struct {
	client *http.Client

	mu      sync.Mutex
	fetched time.Time
	cached  []Machine
}

func newTailnet(socket string) *tailnet {
	if socket == "" {
		return &tailnet{}
	}
	return &tailnet{client: &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}}
}

func (t *tailnet) Machines() ([]Machine, error) {
	if t.client == nil {
		return nil, fmt.Errorf("no Tailscale socket configured")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cached != nil && time.Since(t.fetched) < 15*time.Second {
		return t.cached, nil
	}
	req, _ := http.NewRequest("GET", "http://local-tailscaled.sock/localapi/v0/status", nil)
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tailscale status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tailscale status: %s", resp.Status)
	}
	var st struct {
		Peer map[string]struct {
			HostName     string
			DNSName      string
			TailscaleIPs []string
			OS           string
			Online       bool
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, fmt.Errorf("tailscale status: %v", err)
	}
	out := make([]Machine, 0, len(st.Peer))
	for _, p := range st.Peer {
		dns := strings.TrimSuffix(p.DNSName, ".")
		if dns == "" {
			continue
		}
		m := Machine{Name: strings.SplitN(dns, ".", 2)[0], DNS: dns, OS: p.OS, Online: p.Online}
		for _, ip := range p.TailscaleIPs {
			if strings.Contains(ip, ".") {
				m.IP = ip
				break
			}
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	t.cached, t.fetched = out, time.Now()
	return out, nil
}
