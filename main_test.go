package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDocRoundTrip(t *testing.T) {
	s := Service{Name: "github", Upstream: "https://api.github.com", Headers: map[string]string{"Authorization": "Bearer x"}}
	doc := map[string]any{}
	upsert(doc, s, []string{"web"})
	data, _ := yaml.Marshal(doc)
	back := map[string]any{}
	if err := yaml.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	s.Managed, s.Source = true, "f"
	if got := extract(back, "f"); !reflect.DeepEqual(got, []Service{s}) {
		t.Fatalf("round-trip: got %+v", got)
	}

	// foreign content survives upsert + remove
	doc = map[string]any{
		"http": map[string]any{"routers": map[string]any{"other": map[string]any{"rule": "Host(`a`)", "service": "other"}}},
		"tls":  map[string]any{"certificates": []any{map[string]any{"certFile": "/c.crt"}}},
	}
	upsert(doc, s, nil)
	if _, ok := doc["http"].(map[string]any)["routers"].(map[string]any)["github"].(map[string]any)["entryPoints"]; ok {
		t.Fatal("nil entryPoints must omit key")
	}
	remove(doc, "github")
	data, _ = yaml.Marshal(doc)
	out := string(data)
	if !strings.Contains(out, "other") || !strings.Contains(out, "tls") || strings.Contains(out, "github") {
		t.Fatalf("foreign preservation failed:\n%s", out)
	}
	if got := extract(doc, "f"); len(got) != 1 || got[0].Name != "other" || got[0].Managed {
		t.Fatalf("unmanaged detection: %+v", got)
	}

	for _, bad := range []Service{
		{Name: "../etc", Upstream: "https://x"},
		{Name: "a", Upstream: "ftp://x"},
		{Name: "a", Upstream: "api.github.com"},
		{Name: "a", Upstream: "https://x", Headers: map[string]string{"Bad Key\n": "v"}},
	} {
		if validate(bad) == nil {
			t.Fatalf("validate accepted %+v", bad)
		}
	}

	tmp := t.TempDir()
	os.Mkdir(filepath.Join(tmp, "dyn"), 0o700)
	os.WriteFile(filepath.Join(tmp, "traefik.yml"), []byte("providers:\n  file:\n    directory: dyn\n"), 0o600)
	os.WriteFile(filepath.Join(tmp, "x.yml"), []byte("http: {}\n"), 0o600)
	if p, isFile, err := resolve(filepath.Join(tmp, "traefik.yml")); err != nil || isFile || p != filepath.Join(tmp, "dyn") {
		t.Fatalf("static resolve: %q %v %v", p, isFile, err)
	}
	if p, isFile, err := resolve(filepath.Join(tmp, "x.yml")); err != nil || !isFile || p != filepath.Join(tmp, "x.yml") {
		t.Fatalf("dynamic resolve: %q %v %v", p, isFile, err)
	}
	if _, _, err := resolve(filepath.Join(tmp, "missing")); err == nil {
		t.Fatal("missing path must error")
	}
}

func TestAuthRoundTrip(t *testing.T) {
	for _, h := range []map[string]string{
		{"Authorization": "Bearer tok", "Accept": "json"},
		{"Authorization": "Basic " + "dXNlcjpwYXNz"},
		{"Authorization": "Digest abc"},
		{"X-API-Key": "k"},
		{"Accept": "json"},
	} {
		kind, name, user, secret, rest := splitAuth(h)
		if err := joinAuth(rest, kind, name, user, secret); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(rest, h) {
			t.Fatalf("%v: got %v via %s", h, rest, kind)
		}
	}
	if _, _, _, s, _ := splitAuth(map[string]string{"Authorization": "Basic dXNlcjpwYXNz"}); s != "pass" {
		t.Fatalf("basic password: %q", s)
	}
}
