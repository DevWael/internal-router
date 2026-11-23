package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

//go:embed web/index.html web/icon.svg
var webFiles embed.FS

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

const usage = `internal-router: give services a name under one private domain.

Usage:
  internal-router list
  internal-router add NAME TARGET [--insecure-tls]
  internal-router rm NAME
  internal-router health
  internal-router serve

TARGET is http://host[:port] or https://host[:port].
--server URL (or ROUTER_URL) picks the router to talk to. Default: http://127.0.0.1:8080
`

func main() {
	log.SetFlags(0)
	args, opts := splitArgs(os.Args[1:])
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	server := strings.TrimSuffix(env("ROUTER_URL", "http://127.0.0.1:8080"), "/")
	if v, ok := opts["server"]; ok {
		server = strings.TrimSuffix(v, "/")
	}
	var err error
	switch args[0] {
	case "serve":
		err = serve()
	case "list", "ls":
		err = cliList(server)
	case "add":
		if len(args) != 3 {
			err = errors.New("usage: internal-router add NAME TARGET [--insecure-tls]")
			break
		}
		_, insecure := opts["insecure-tls"]
		err = cliAdd(server, args[1], args[2], insecure)
	case "rm", "remove", "delete":
		if len(args) != 2 {
			err = errors.New("usage: internal-router rm NAME")
			break
		}
		err = cliRemove(server, args[1])
	case "health":
		err = cliHealth(server)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
	}
	if err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func splitArgs(in []string) ([]string, map[string]string) {
	var args []string
	opts := map[string]string{}
	for i := 0; i < len(in); i++ {
		a := in[i]
		if !strings.HasPrefix(a, "--") {
			args = append(args, a)
			continue
		}
		k, v, hasValue := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if !hasValue && k == "server" && i+1 < len(in) {
			v = in[i+1]
			i++
		}
		opts[k] = v
	}
	return args, opts
}

func serve() error {
	domain := strings.Trim(strings.ToLower(env("ROUTER_DOMAIN", "internal")), ".")
	routerName := env("ROUTER_NAME", "router")
	if err := validateName(routerName, ""); err != nil {
		return fmt.Errorf("ROUTER_NAME: %v", err)
	}
	dataDir := env("ROUTER_DATA", "/data")
	store, err := OpenStore(filepath.Join(dataDir, "routes.json"))
	if err != nil {
		return err
	}
	cs := caddySettings{
		AdminURL:   strings.TrimSuffix(env("CADDY_ADMIN", "http://localhost:2019"), "/"),
		Domain:     domain,
		RouterName: routerName,
		UIDial:     env("ROUTER_UI_LISTEN", "127.0.0.1:8080"),
		CARootCert: os.Getenv("CA_ROOT_CERT"),
		CARootKey:  os.Getenv("CA_ROOT_KEY"),
		CAIntCert:  os.Getenv("CA_INTERMEDIATE_CERT"),
		CAIntKey:   os.Getenv("CA_INTERMEDIATE_KEY"),
	}

	dns := &dnsServer{
		domain:    domain,
		iface:     env("ROUTER_DNS_INTERFACE", "tailscale0"),
		fixedIPv4: net.ParseIP(os.Getenv("ROUTER_DNS_IPV4")),
		fixedIPv6: net.ParseIP(os.Getenv("ROUTER_DNS_IPV6")),
	}
	dnsAddr := env("ROUTER_DNS_LISTEN", ":53")
	errc := make(chan error, 3)
	go func() { errc <- fmt.Errorf("dns udp: %w", dns.serveUDP(dnsAddr)) }()
	go func() { errc <- fmt.Errorf("dns tcp: %w", dns.serveTCP(dnsAddr)) }()

	go keepCaddyLoaded(cs, store)

	srv := &http.Server{
		Addr:              cs.UIDial,
		Handler:           apiHandler(store, cs),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { errc <- fmt.Errorf("http: %w", srv.ListenAndServe()) }()
	log.Printf("internal-router: *.%s, UI at https://%s, DNS on %s, %d routes", domain, cs.host(routerName), dnsAddr, len(store.List()))
	return <-errc
}

func keepCaddyLoaded(cs caddySettings, store *Store) {
	loaded := false
	for {
		if !loaded || !caddyHasConfig(cs) {
			if err := pushCaddy(cs, store.List()); err != nil {
				log.Printf("caddy: %v", err)
				loaded = false
				time.Sleep(3 * time.Second)
				continue
			}
			if !loaded {
				log.Printf("caddy: config loaded")
			}
			loaded = true
		}
		time.Sleep(30 * time.Second)
	}
}

func caddyHasConfig(cs caddySettings) bool {
	resp, err := caddyClient.Get(cs.AdminURL + "/config/apps/http/servers/internal/listen")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return resp.StatusCode == http.StatusOK && !bytes.Equal(bytes.TrimSpace(b), []byte("null"))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func apiHandler(store *Store, cs caddySettings) http.Handler {
	uiOrigin := "https://" + cs.host(cs.RouterName)
	mux := http.NewServeMux()

	static := func(file, ctype string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, _ := webFiles.ReadFile(file)
			w.Header().Set("Content-Type", ctype)
			w.Header().Set("Cache-Control", "no-cache")
			w.Write(b)
		}
	}
	mux.HandleFunc("GET /{$}", static("web/index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /icon.svg", static("web/icon.svg", "image/svg+xml"))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "caddy": caddyHasConfig(cs)})
	})
	mux.HandleFunc("GET /api/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"domain": cs.Domain, "router": cs.host(cs.RouterName)})
	})
	mux.HandleFunc("GET /api/routes", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, store.List())
	})
	mux.HandleFunc("PUT /api/routes/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := strings.ToLower(r.PathValue("name"))
		var in struct {
			Target      string `json:"target"`
			InsecureTLS bool   `json:"insecure_tls"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("body must be JSON like {\"target\":\"http://host:port\"}: %v", err))
			return
		}
		route := Route{Name: name, Target: strings.TrimSuffix(strings.TrimSpace(in.Target), "/"), InsecureTLS: in.InsecureTLS}
		err := store.Update(func(m map[string]Route) error {
			if err := validateName(name, cs.RouterName); err != nil {
				return err
			}
			if _, _, err := parseTarget(route.Target); err != nil {
				return err
			}
			m[name] = route
			return nil
		}, func(rs []Route) error { return pushCaddy(cs, rs) })
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		log.Printf("route set by %s: %s -> %s", clientAddr(r), cs.host(name), route.Target)
		writeJSON(w, http.StatusOK, route)
	})
	mux.HandleFunc("DELETE /api/routes/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := strings.ToLower(r.PathValue("name"))
		err := store.Update(func(m map[string]Route) error {
			if _, ok := m[name]; !ok {
				return fmt.Errorf("no route named %q", name)
			}
			delete(m, name)
			return nil
		}, func(rs []Route) error { return pushCaddy(cs, rs) })
		if err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		log.Printf("route removed by %s: %s", clientAddr(r), cs.host(name))
		w.WriteHeader(http.StatusNoContent)
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if o := r.Header.Get("Origin"); o != "" && o != uiOrigin {
				writeErr(w, http.StatusForbidden, fmt.Errorf("requests from %s are not allowed", o))
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func clientAddr(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	return r.RemoteAddr
}

var cliClient = &http.Client{Timeout: 60 * time.Second}

func call(method, u string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := cliClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("%s %s: %s", method, u, resp.Status)
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

func cliList(server string) error {
	var info struct{ Domain string }
	if err := call("GET", server+"/api/info", nil, &info); err != nil {
		return err
	}
	var routes []Route
	if err := call("GET", server+"/api/routes", nil, &routes); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTARGET\tTLS CHECK")
	for _, r := range routes {
		check := "on"
		if r.InsecureTLS {
			check = "off"
		}
		fmt.Fprintf(tw, "%s.%s\t%s\t%s\n", r.Name, info.Domain, r.Target, check)
	}
	return tw.Flush()
}

func cliAdd(server, name, target string, insecure bool) error {
	var r Route
	body := map[string]any{"target": target, "insecure_tls": insecure}
	if err := call("PUT", server+"/api/routes/"+url.PathEscape(name), body, &r); err != nil {
		return err
	}
	fmt.Printf("%s -> %s\n", r.Name, r.Target)
	return nil
}

func cliRemove(server, name string) error {
	if err := call("DELETE", server+"/api/routes/"+url.PathEscape(name), nil, nil); err != nil {
		return err
	}
	fmt.Printf("removed %s\n", name)
	return nil
}

func cliHealth(server string) error {
	var h struct {
		OK    bool `json:"ok"`
		Caddy bool `json:"caddy"`
	}
	if err := call("GET", server+"/healthz", nil, &h); err != nil {
		return err
	}
	if !h.Caddy {
		return errors.New("caddy has not loaded the router config yet")
	}
	fmt.Println("ok")
	return nil
}
