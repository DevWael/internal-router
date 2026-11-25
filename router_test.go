package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in      string
		dial    string
		tls     bool
		wantErr bool
	}{
		{"http://10.0.0.5:8080", "10.0.0.5:8080", false, false},
		{"http://box", "box:80", false, false},
		{"https://box", "box:443", true, false},
		{"https://[fd00::1]:8443/", "[fd00::1]:8443", true, false},
		{"ftp://box", "", false, true},
		{"http://box/path", "", false, true},
		{"http://user@box", "", false, true},
		{"http://box:99999", "", false, true},
		{"box:80", "", false, true},
	}
	for _, c := range cases {
		dial, useTLS, err := parseTarget(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v, want error %v", c.in, err, c.wantErr)
			continue
		}
		if dial != c.dial || useTLS != c.tls {
			t.Errorf("%s: got %s tls=%v, want %s tls=%v", c.in, dial, useTLS, c.dial, c.tls)
		}
	}
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"gym", "a", "my-app2"} {
		if err := validateName(ok, "router"); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Gym", "-a", "a-", "a.b", "router", strings.Repeat("a", 64)} {
		if validateName(bad, "router") == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestStoreRollsBackWhenApplyFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.json")
	s, _ := OpenStore(path)
	ok := func([]Route) error { return nil }
	if err := s.Update(func(m map[string]Route) error { m["a"] = Route{Name: "a", Target: "http://x"}; return nil }, ok); err != nil {
		t.Fatal(err)
	}
	fail := func([]Route) error { return errors.New("caddy down") }
	if err := s.Update(func(m map[string]Route) error { m["b"] = Route{Name: "b", Target: "http://y"}; return nil }, fail); err == nil {
		t.Fatal("expected the failing apply to return an error")
	}
	if got := s.List(); len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("in-memory routes after failed apply: %+v", got)
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.List(); len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("routes on disk after failed apply: %+v", got)
	}
}

func query(name string, qtype uint16) []byte {
	msg := []byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, l := range strings.Split(name, ".") {
		msg = append(msg, byte(len(l)))
		msg = append(msg, l...)
	}
	msg = append(msg, 0, byte(qtype>>8), byte(qtype), 0, 1)
	return msg
}

func TestDNSAnswers(t *testing.T) {
	d := &dnsServer{domain: "internal", fixedIPv4: net.ParseIP("100.64.0.7"), fixedIPv6: net.ParseIP("fd7a::7")}

	resp := d.answer(query("Gym.Internal", typeA))
	if binary.BigEndian.Uint16(resp[0:2]) != 0x1234 {
		t.Fatal("reply id does not match the query")
	}
	if rcode := resp[3] & 0x0f; rcode != 0 {
		t.Fatalf("rcode %d", rcode)
	}
	if an := binary.BigEndian.Uint16(resp[6:8]); an != 1 {
		t.Fatalf("answers %d", an)
	}
	if ip := net.IP(resp[len(resp)-4:]); !ip.Equal(net.ParseIP("100.64.0.7")) {
		t.Fatalf("answer %s", ip)
	}

	resp = d.answer(query("gym.internal", typeAAAA))
	if ip := net.IP(resp[len(resp)-16:]); !ip.Equal(net.ParseIP("fd7a::7")) {
		t.Fatalf("AAAA answer %s", ip)
	}

	resp = d.answer(query("example.com", typeA))
	if rcode := resp[3] & 0x0f; rcode != rcodeRefused {
		t.Fatalf("outside the zone: rcode %d, want REFUSED", rcode)
	}

	resp = d.answer(query("gym.internal", 16))
	if an := binary.BigEndian.Uint16(resp[6:8]); an != 0 || resp[3]&0x0f != 0 {
		t.Fatal("TXT should be an empty NOERROR reply")
	}

	if d.answer([]byte{1, 2, 3}) != nil {
		t.Fatal("short packet should be dropped")
	}
}

func TestCaddyConfig(t *testing.T) {
	cs := caddySettings{Domain: "internal", RouterName: "router", UIDial: "127.0.0.1:8080", CARootCert: "/ca/ca.crt", CARootKey: "/ca/ca.key"}
	cfg, err := buildCaddyConfig(cs, []Route{
		{Name: "gym", Target: "http://10.0.0.5"},
		{Name: "nas", Target: "https://10.0.0.6:8443", InsecureTLS: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cfg)
	s := string(b)
	for _, want := range []string{
		`"router.internal"`, `"gym.internal"`, `"nas.internal"`,
		`"dial":"10.0.0.5:80"`, `"dial":"10.0.0.6:8443"`, `"insecure_skip_verify":true`,
		`"certificate":"/ca/ca.crt"`, `"module":"internal"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("config is missing %s", want)
		}
	}
}

func TestAPIRejectsOtherOrigins(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "routes.json"))
	h := apiHandler(s, caddySettings{Domain: "internal", RouterName: "router", AdminURL: "http://127.0.0.1:1"}, newTailnet(""))

	req := httptest.NewRequest("DELETE", "/api/routes/gym", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin delete: status %d, want 403", rec.Code)
	}

	req = httptest.NewRequest("PUT", "/api/routes/gym", strings.NewReader(`{"target":"http://10.0.0.5"}`))
	req.Header.Set("Origin", "https://router.internal")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "caddy") {
		t.Fatalf("same-origin put with Caddy down: status %d body %s", rec.Code, rec.Body.String())
	}
	if len(s.List()) != 0 {
		t.Fatal("route was saved although Caddy did not accept it")
	}
}

func TestTailnetMachines(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "ts.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/localapi/v0/status" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"Self":{"DNSName":"router.example.ts.net."},"Peer":{
			"a":{"HostName":"nas","DNSName":"nas.example.ts.net.","TailscaleIPs":["fd7a::2","100.64.0.2"],"OS":"linux","Online":true},
			"b":{"HostName":"Work Laptop","DNSName":"laptop.example.ts.net.","TailscaleIPs":["100.64.0.3"],"OS":"macOS","Online":false},
			"c":{"HostName":"shared","DNSName":"","TailscaleIPs":["100.64.0.4"]}}}`))
	})}
	go srv.Serve(ln)
	defer srv.Close()

	got, err := newTailnet(sock).Machines()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("machines: %+v", got)
	}
	if got[0].Name != "laptop" || got[0].Online || got[0].DNS != "laptop.example.ts.net" {
		t.Errorf("first machine: %+v", got[0])
	}
	if got[1].Name != "nas" || got[1].IP != "100.64.0.2" || !got[1].Online {
		t.Errorf("second machine: %+v", got[1])
	}

	if _, err := newTailnet("").Machines(); err == nil {
		t.Error("expected an error without a socket")
	}
}

func TestWatchInterface(t *testing.T) {
	err := watchInterface(func() error { return errors.New("gone") }, "tailscale0", 5*time.Millisecond, 40*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "tailscale0 missing") {
		t.Fatalf("expected a missing-interface error, got %v", err)
	}

	var calls atomic.Int32
	lookup := func() error {
		if n := calls.Add(1); n%4 != 0 {
			return errors.New("gone")
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- watchInterface(lookup, "tailscale0", 5*time.Millisecond, 40*time.Millisecond) }()
	select {
	case err := <-done:
		t.Fatalf("watcher gave up although the interface kept coming back: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}
