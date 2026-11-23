package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
)

type Route struct {
	Name        string `json:"name"`
	Target      string `json:"target"`
	InsecureTLS bool   `json:"insecure_tls,omitempty"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func validateName(name, reserved string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("name %q must be lowercase letters, digits and dashes (1-63 chars), without the domain", name)
	}
	if name == reserved {
		return fmt.Errorf("name %q is used by the router itself", name)
	}
	return nil
}

func parseTarget(target string) (dial string, useTLS bool, err error) {
	u, err := url.Parse(target)
	if err != nil {
		return "", false, fmt.Errorf("target %q is not a URL: %v", target, err)
	}
	switch u.Scheme {
	case "http":
	case "https":
		useTLS = true
	default:
		return "", false, fmt.Errorf("target %q must start with http:// or https://", target)
	}
	if u.Hostname() == "" {
		return "", false, fmt.Errorf("target %q has no host", target)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return "", false, fmt.Errorf("target %q must be scheme://host[:port] only", target)
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if useTLS {
			port = "443"
		}
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return "", false, fmt.Errorf("target %q has an invalid port", target)
	}
	return net.JoinHostPort(u.Hostname(), port), useTLS, nil
}

type Store struct {
	mu     sync.Mutex
	path   string
	routes map[string]Route
}

func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, routes: map[string]Route{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var file struct {
		Routes []Route `json:"routes"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	for _, r := range file.Routes {
		s.routes[r.Name] = r
	}
	return s, nil
}

func (s *Store) List() []Route {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sortedLocked()
}

func (s *Store) sortedLocked() []Route {
	out := make([]Route, 0, len(s.routes))
	for _, r := range s.routes {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) Update(fn func(map[string]Route) error, apply func([]Route) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]Route, len(s.routes))
	for k, v := range s.routes {
		next[k] = v
	}
	if err := fn(next); err != nil {
		return err
	}
	prev := s.routes
	s.routes = next
	if err := apply(s.sortedLocked()); err != nil {
		s.routes = prev
		return err
	}
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(struct {
		Routes []Route `json:"routes"`
	}{s.sortedLocked()}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".routes-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}
