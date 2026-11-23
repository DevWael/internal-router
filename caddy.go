package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type caddySettings struct {
	AdminURL   string
	Domain     string
	RouterName string
	UIDial     string
	CARootCert string
	CARootKey  string
}

func (c caddySettings) host(name string) string {
	return name + "." + c.Domain
}

func buildCaddyConfig(c caddySettings, routes []Route) (map[string]any, error) {
	uiHost := c.host(c.RouterName)
	hosts := []string{uiHost}
	httpRoutes := []any{
		map[string]any{
			"match":    []any{map[string]any{"host": []string{uiHost}}},
			"handle":   []any{map[string]any{"handler": "reverse_proxy", "upstreams": []any{map[string]any{"dial": c.UIDial}}}},
			"terminal": true,
		},
	}
	for _, r := range routes {
		dial, useTLS, err := parseTarget(r.Target)
		if err != nil {
			return nil, err
		}
		proxy := map[string]any{
			"handler":   "reverse_proxy",
			"upstreams": []any{map[string]any{"dial": dial}},
		}
		if useTLS {
			tls := map[string]any{}
			if r.InsecureTLS {
				tls["insecure_skip_verify"] = true
			}
			proxy["transport"] = map[string]any{"protocol": "http", "tls": tls}
		}
		h := c.host(r.Name)
		hosts = append(hosts, h)
		httpRoutes = append(httpRoutes, map[string]any{
			"match":    []any{map[string]any{"host": []string{h}}},
			"handle":   []any{proxy},
			"terminal": true,
		})
	}

	ca := map[string]any{"name": "internal-router", "install_trust": false}
	if c.CARootCert != "" && c.CARootKey != "" {
		ca["root"] = map[string]any{
			"certificate": c.CARootCert,
			"private_key": c.CARootKey,
			"format":      "pem_file",
		}
	}

	return map[string]any{
		"admin": map[string]any{"listen": "localhost:2019"},
		"apps": map[string]any{
			"pki": map[string]any{"certificate_authorities": map[string]any{"local": ca}},
			"tls": map[string]any{
				"automation": map[string]any{
					"policies": []any{map[string]any{
						"subjects": hosts,
						"issuers":  []any{map[string]any{"module": "internal", "ca": "local"}},
					}},
				},
			},
			"http": map[string]any{
				"servers": map[string]any{
					"internal": map[string]any{
						"listen": []string{":443"},
						"routes": httpRoutes,
					},
				},
			},
		},
	}, nil
}

var caddyClient = &http.Client{Timeout: 30 * time.Second}

func pushCaddy(c caddySettings, routes []Route) error {
	cfg, err := buildCaddyConfig(c, routes)
	if err != nil {
		return err
	}
	body, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	resp, err := caddyClient.Post(c.AdminURL+"/load", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("caddy admin API: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("caddy rejected the config (%s): %s", resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}
