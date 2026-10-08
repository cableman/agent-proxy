package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed index.html
var indexHTML []byte

// ---------- settings ----------

type Settings struct {
	Config      string `json:"config"`      // dynamic dir | dynamic file | traefik static yml; "" = not configured
	Traefik     string `json:"traefik"`     // Traefik API base
	Base        string `json:"base"`        // display-only agent base URL
	EntryPoints string `json:"entryPoints"` // comma-separated; "" = all (key omitted)
}

var state struct {
	sync.Mutex
	Settings
}

func settingsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "agent-proxy", "settings.json")
}

func saveSettings(s Settings) error {
	p := settingsPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(p, data, 0o600)
}

func abs(p string) string {
	if p == "" {
		return ""
	}
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// resolve turns the user-supplied config path into the dynamic-config target.
func resolve(cfg string) (target string, isFile bool, err error) {
	if cfg == "" {
		return "", false, errors.New("no config path set — open Settings")
	}
	fi, err := os.Stat(cfg)
	if err != nil {
		return "", false, fmt.Errorf("config path not found: %s", cfg)
	}
	if fi.IsDir() {
		return cfg, false, nil
	}
	if strings.HasSuffix(cfg, ".toml") {
		return "", false, errors.New("only YAML static config is supported")
	}
	doc, err := readDoc(cfg)
	if err != nil {
		return "", false, err
	}
	// ponytail: Docker-mounted Traefik has container paths here (e.g. /config); user points -config at the host file/dir instead.
	if file, ok := nested(doc, "providers", "file").(map[string]any); ok {
		for _, key := range []string{"directory", "filename"} {
			p, ok := file[key].(string)
			if !ok || p == "" {
				continue
			}
			if !filepath.IsAbs(p) {
				p = filepath.Join(filepath.Dir(cfg), p)
			}
			if _, err := os.Stat(p); err != nil {
				return "", false, fmt.Errorf("config path not found: %s", p)
			}
			return p, key == "filename", nil
		}
	}
	return cfg, true, nil
}

func entryPoints(s Settings) []string {
	var out []string
	for _, e := range strings.Split(s.EntryPoints, ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// ---------- model ----------

type Service struct {
	Name     string            `json:"name"`
	Upstream string            `json:"upstream"`
	Headers  map[string]string `json:"headers"`
	Managed  bool              `json:"managed"`
	Source   string            `json:"source"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

func validate(s Service) error {
	if !nameRe.MatchString(s.Name) {
		return errors.New("name: must match ^[a-z0-9][a-z0-9-]{0,63}$")
	}
	u, err := url.Parse(s.Upstream)
	if err != nil {
		return fmt.Errorf("upstream: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("upstream: scheme must be http or https")
	}
	if u.Host == "" {
		return errors.New("upstream: host is required")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("upstream: query and fragment are not allowed")
	}
	for k, v := range s.Headers {
		if k == "" || strings.TrimSpace(k) != k || strings.ContainsAny(k, "\r\n:") {
			return fmt.Errorf("header %q: key must be non-empty, trimmed and contain no ':' or newlines", k)
		}
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("header %s: value must not contain newlines", k)
		}
	}
	return nil
}

// sub returns the nested map at key, creating it if absent.
func sub(m map[string]any, key string) map[string]any {
	if c, ok := m[key].(map[string]any); ok {
		return c
	}
	c := map[string]any{}
	m[key] = c
	return c
}

func nested(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func upsert(doc map[string]any, s Service, eps []string) {
	h := sub(doc, "http")
	router := map[string]any{
		"rule":        "PathPrefix(`/" + s.Name + "`)",
		"middlewares": []any{s.Name + "-strip", s.Name + "-headers"},
		"service":     s.Name,
	}
	if len(eps) > 0 {
		e := make([]any, len(eps))
		for i, v := range eps {
			e[i] = v
		}
		router["entryPoints"] = e
	}
	sub(h, "routers")[s.Name] = router
	headers := map[string]any{}
	for k, v := range s.Headers {
		headers[k] = v
	}
	mw := sub(h, "middlewares")
	mw[s.Name+"-strip"] = map[string]any{"stripPrefix": map[string]any{"prefixes": []any{"/" + s.Name}}}
	mw[s.Name+"-headers"] = map[string]any{"headers": map[string]any{"customRequestHeaders": headers}}
	sub(h, "services")[s.Name] = map[string]any{"loadBalancer": map[string]any{
		"servers":        []any{map[string]any{"url": s.Upstream}},
		"passHostHeader": false,
	}}
}

func remove(doc map[string]any, name string) {
	h, ok := doc["http"].(map[string]any)
	if !ok {
		return
	}
	for key, names := range map[string][]string{
		"routers":     {name},
		"middlewares": {name + "-strip", name + "-headers"},
		"services":    {name},
	} {
		if m, ok := h[key].(map[string]any); ok {
			for _, n := range names {
				delete(m, n)
			}
			if len(m) == 0 {
				delete(h, key)
			}
		}
	}
	if len(h) == 0 {
		delete(doc, "http")
	}
}

// ponytail: marshal/unmarshal via map loses comments and key order in single-file mode. Upgrade: yaml.Node round-trip.
func extract(doc map[string]any, source string) []Service {
	routers, _ := nested(doc, "http", "routers").(map[string]any)
	var out []Service
	for r := range routers {
		s := Service{Name: r, Source: source}
		svc, _ := nested(doc, "http", "routers", r, "service").(string)
		servers, _ := nested(doc, "http", "services", r, "loadBalancer", "servers").([]any)
		hdrs, hok := nested(doc, "http", "middlewares", r+"-headers", "headers", "customRequestHeaders").(map[string]any)
		if svc == r && hok && len(servers) > 0 {
			if u, ok := nested(map[string]any{"s": servers[0]}, "s", "url").(string); ok {
				s.Managed, s.Upstream, s.Headers = true, u, map[string]string{}
				for k, v := range hdrs {
					if str, ok := v.(string); ok {
						s.Headers[k] = str
					}
				}
			}
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ---------- storage ----------

func readDoc(path string) (map[string]any, error) {
	doc := map[string]any{}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return doc, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

// writeDoc writes in place (truncate+write) so a file-level Docker bind mount keeps its inode and fsnotify watch.
// ponytail: not atomic; Traefik may read a half-written file once and log an error, then reloads on the next write event.
func writeDoc(path string, doc map[string]any) error {
	data, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func listServices(target string, isFile bool) ([]Service, error) {
	if isFile {
		doc, err := readDoc(target)
		if err != nil {
			return nil, err
		}
		return extract(doc, target), nil
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return nil, err
	}
	var out []Service
	for _, e := range entries {
		n := e.Name()
		ext := filepath.Ext(n)
		if e.IsDir() || (ext != ".yml" && ext != ".yaml") {
			continue
		}
		doc, err := readDoc(filepath.Join(target, n))
		if err != nil {
			log.Print(err)
		}
		svcs := extract(doc, n)
		if len(svcs) == 0 {
			svcs = []Service{{Name: strings.TrimSuffix(n, ext), Source: n}}
		}
		out = append(out, svcs...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func saveService(target string, isFile bool, s Service, eps []string) error {
	doc := map[string]any{}
	if isFile {
		var err error
		if doc, err = readDoc(target); err != nil {
			return err
		}
	} else {
		target = filepath.Join(target, s.Name+".yml")
	}
	upsert(doc, s, eps)
	return writeDoc(target, doc)
}

func deleteService(target string, isFile bool, name string) error {
	if !isFile {
		err := os.Remove(filepath.Join(target, name+".yml"))
		if errors.Is(err, fs.ErrNotExist) {
			err = os.Remove(filepath.Join(target, name+".yaml"))
		}
		return err
	}
	doc, err := readDoc(target)
	if err != nil {
		return err
	}
	for _, s := range extract(doc, target) {
		if s.Name == name && s.Managed {
			remove(doc, name)
			return writeDoc(target, doc)
		}
	}
	return fs.ErrNotExist
}

// ---------- http ----------

func current() Settings {
	state.Lock()
	defer state.Unlock()
	return state.Settings
}

func httpURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid URL: %q", s)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func main() {
	s := Settings{Traefik: "http://127.0.0.1:8080", Base: "http://localhost", EntryPoints: "web"}
	if data, err := os.ReadFile(settingsPath()); err == nil {
		json.Unmarshal(data, &s)
	}
	var f Settings
	listen := flag.String("listen", "127.0.0.1:9000", "address to listen on")
	flag.StringVar(&f.Config, "config", "", "dynamic-config dir, dynamic-config file, or traefik static yml")
	flag.StringVar(&f.Traefik, "traefik", "", "Traefik API base URL")
	flag.StringVar(&f.Base, "base", "", "agent-facing base URL (display only)")
	flag.StringVar(&f.EntryPoints, "entrypoints", "", "comma-separated router entryPoints; empty = all")
	flag.Parse()
	flag.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "config":
			s.Config = abs(f.Config)
		case "traefik":
			s.Traefik = f.Traefik
		case "base":
			s.Base = f.Base
		case "entrypoints":
			s.EntryPoints = f.EntryPoints
		}
	})
	state.Settings = s
	if err := saveSettings(s); err != nil {
		log.Print(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		s := current()
		target, isFile, err := resolve(s.Config)
		mode, msg := "dir", ""
		if isFile {
			mode = "file"
		}
		if err != nil {
			msg = err.Error()
		}
		writeJSON(w, map[string]any{"config": s.Config, "traefik": s.Traefik, "base": s.Base,
			"entryPoints": s.EntryPoints, "resolved": target, "mode": mode, "error": msg})
	})
	mux.HandleFunc("PUT /api/settings", func(w http.ResponseWriter, r *http.Request) {
		var s Settings
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		s.Config = abs(strings.TrimSpace(s.Config))
		if _, _, err := resolve(s.Config); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := errors.Join(httpURL(s.Traefik), httpURL(s.Base)); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		state.Lock()
		state.Settings = s
		state.Unlock()
		if err := saveSettings(s); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /api/services", func(w http.ResponseWriter, r *http.Request) {
		s := current()
		target, isFile, err := resolve(s.Config)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		svcs, err := listServices(target, isFile)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if svcs == nil {
			svcs = []Service{}
		}
		writeJSON(w, map[string]any{"base": s.Base, "services": svcs})
	})
	mux.HandleFunc("PUT /api/services/{name}", func(w http.ResponseWriter, r *http.Request) {
		var svc Service
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&svc); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		svc.Name = r.PathValue("name")
		if err := validate(svc); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		s := current()
		target, isFile, err := resolve(s.Config)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := saveService(target, isFile, svc, entryPoints(s)); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("DELETE /api/services/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !nameRe.MatchString(name) {
			http.Error(w, "name: must match ^[a-z0-9][a-z0-9-]{0,63}$", 400)
			return
		}
		target, isFile, err := resolve(current().Config)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		switch err := deleteService(target, isFile, name); {
		case errors.Is(err, fs.ErrNotExist):
			http.Error(w, "not found", 404)
		case err != nil:
			http.Error(w, err.Error(), 500)
		default:
			w.WriteHeader(204)
		}
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		routers := map[string]string{}
		client := http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(strings.TrimRight(current().Traefik, "/") + "/api/http/routers")
		if err != nil {
			writeJSON(w, map[string]any{"traefik": false, "routers": routers})
			return
		}
		defer resp.Body.Close()
		var list []struct{ Name, Status string }
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			writeJSON(w, map[string]any{"traefik": false, "routers": routers})
			return
		}
		for _, r := range list {
			if n, ok := strings.CutSuffix(r.Name, "@file"); ok {
				routers[n] = r.Status
			}
		}
		writeJSON(w, map[string]any{"traefik": true, "routers": routers})
	})
	log.Printf("agent-proxy listening on http://%s (config: %q)", *listen, s.Config)
	log.Fatal(http.ListenAndServe(*listen, mux))
}
