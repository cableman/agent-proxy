package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/widget"
	"gopkg.in/yaml.v3"
)

// ---------- settings ----------

type Settings struct {
	Config      string `json:"config"`      // dynamic dir | dynamic file | traefik static yml; "" = not configured
	Traefik     string `json:"traefik"`     // Traefik API base
	Base        string `json:"base"`        // display-only agent base URL
	EntryPoints string `json:"entryPoints"` // comma-separated; "" = all (key omitted)
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

// ---------- traefik ----------

func httpURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid URL: %q", s)
	}
	return nil
}

// status returns the status of each @file router; ok=false when Traefik is unreachable.
func status(api string) (ok bool, routers map[string]string) {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(strings.TrimRight(api, "/") + "/api/http/routers")
	if err != nil {
		return false, nil
	}
	defer resp.Body.Close()
	var list []struct{ Name, Status string }
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return false, nil
	}
	routers = map[string]string{}
	for _, r := range list {
		if n, ok := strings.CutSuffix(r.Name, "@file"); ok {
			routers[n] = r.Status
		}
	}
	return true, routers
}

// ---------- gui ----------

func warn(w fyne.Window, err error) { dialog.ShowError(err, w) }

// form shows a dialog; submit runs on OK and the form reopens (values intact) when it errors. cancel runs when dismissed.
func form(w fyne.Window, title, note string, items []*widget.FormItem, submit func() error, cancel func()) {
	if note != "" {
		l := widget.NewLabel(note)
		l.Wrapping = fyne.TextWrapWord
		items = append([]*widget.FormItem{{Widget: l}}, items...)
	}
	var show func()
	show = func() {
		d := dialog.NewForm(title, "OK", "Cancel", items, func(ok bool) {
			if !ok {
				if cancel != nil {
					cancel()
				}
			} else if err := submit(); err != nil {
				warn(w, err)
				show()
			}
		}, w)
		d.Resize(fyne.NewSize(640, 0))
		d.Show()
	}
	show()
}

func parseHeaders(text string) (map[string]string, error) {
	h := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("header line %q: expected Key: Value", line)
		}
		h[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return h, validate(Service{Name: "x", Upstream: "http://x", Headers: h})
}

func entry(text, placeholder string) *widget.Entry {
	e := widget.NewEntry()
	e.SetText(text)
	e.SetPlaceHolder(placeholder)
	return e
}

// editSettings calls done(true) after a valid save, done(false) on cancel.
func editSettings(w fyne.Window, s *Settings, problem string, done func(ok bool)) {
	cfg := entry(s.Config, "dynamic dir, dynamic .yml, or traefik.yml")
	api, base := entry(s.Traefik, ""), entry(s.Base, "")
	eps := entry(s.EntryPoints, "comma-separated; empty = all")
	file := widget.NewButton("File…", func() {
		d := dialog.NewFileOpen(func(r fyne.URIReadCloser, _ error) {
			if r != nil {
				r.Close()
				cfg.SetText(r.URI().Path())
			}
		}, w)
		d.SetFilter(storage.NewExtensionFileFilter([]string{".yml", ".yaml"}))
		d.Resize(fyne.NewSize(800, 600))
		d.Show()
	})
	dir := widget.NewButton("Folder…", func() {
		d := dialog.NewFolderOpen(func(u fyne.ListableURI, _ error) {
			if u != nil {
				cfg.SetText(u.Path())
			}
		}, w)
		d.Resize(fyne.NewSize(800, 600))
		d.Show()
	})
	form(w, "Settings", problem, []*widget.FormItem{
		widget.NewFormItem("Traefik config path", container.NewBorder(nil, nil, nil, container.NewHBox(file, dir), cfg)),
		widget.NewFormItem("Traefik API URL", api),
		widget.NewFormItem("Agent base URL (display only)", base),
		widget.NewFormItem("Entry points", eps),
	}, func() error {
		n := Settings{Config: abs(strings.TrimSpace(cfg.Text)), Traefik: strings.TrimSpace(api.Text), Base: strings.TrimSpace(base.Text), EntryPoints: eps.Text}
		if _, _, err := resolve(n.Config); err != nil {
			return err
		}
		if err := httpURL(n.Traefik); err != nil {
			return err
		}
		if err := httpURL(n.Base); err != nil {
			return err
		}
		*s = n
		if err := saveSettings(n); err != nil {
			log.Print(err)
		}
		done(true)
		return nil
	}, func() { done(false) })
}

// editService edits old, or creates a new service when old.Name is empty; done gets the save result.
func editService(w fyne.Window, s Settings, target string, isFile bool, old Service, done func(error)) {
	name, up := entry(old.Name, "becomes the /name prefix"), entry(old.Upstream, "https://api.github.com")
	if old.Name != "" {
		name.Disable()
	}
	var lines []string
	for k, v := range old.Headers {
		lines = append(lines, k+": "+v)
	}
	sort.Strings(lines)
	hdrs := widget.NewMultiLineEntry()
	hdrs.SetText(strings.Join(lines, "\n"))
	hdrs.SetPlaceHolder("one Key: Value per line, e.g.\nAuthorization: Bearer ghp_xxx")
	hdrs.SetMinRowsVisible(4)
	title := "New service"
	if old.Name != "" {
		title = "Edit " + old.Name
	}
	form(w, title, "", []*widget.FormItem{
		widget.NewFormItem("Name", name),
		widget.NewFormItem("Upstream URL", up),
		widget.NewFormItem("Request headers", hdrs),
	}, func() error {
		h, err := parseHeaders(hdrs.Text)
		if err != nil {
			return err
		}
		svc := Service{Name: strings.TrimSpace(name.Text), Upstream: strings.TrimSpace(up.Text), Headers: h}
		if err := validate(svc); err != nil {
			return err
		}
		done(saveService(target, isFile, svc, entryPoints(s)))
		return nil
	}, nil)
}

func main() {
	s := Settings{Traefik: "http://127.0.0.1:8080", Base: "http://localhost", EntryPoints: "web"}
	if data, err := os.ReadFile(settingsPath()); err == nil {
		json.Unmarshal(data, &s)
	}
	var f Settings
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
	if err := saveSettings(s); err != nil {
		log.Print(err)
	}

	w := app.New().NewWindow("Agent Proxy")
	w.Resize(fyne.NewSize(960, 520))
	cols := []string{"Name", "Status", "Proxy URL", "Upstream", "Headers", "Source"}
	var rows [][]string
	sel := -1
	table := widget.NewTableWithHeaders(
		func() (int, int) { return len(rows), len(cols) },
		func() fyne.CanvasObject { return widget.NewLabel("") },
		func(id widget.TableCellID, o fyne.CanvasObject) { o.(*widget.Label).SetText(rows[id.Row][id.Col]) },
	)
	table.ShowHeaderColumn = false
	table.UpdateHeader = func(id widget.TableCellID, o fyne.CanvasObject) { o.(*widget.Label).SetText(cols[id.Col]) }
	table.OnSelected = func(id widget.TableCellID) { sel = id.Row }
	table.OnUnselected = func(widget.TableCellID) { sel = -1 }
	msg, conn := widget.NewLabel(""), widget.NewLabel("")
	flash := func(text string) {
		msg.SetText(text)
		time.AfterFunc(8*time.Second, func() {
			fyne.Do(func() {
				if msg.Text == text {
					msg.SetText("")
				}
			})
		})
	}

	var svcs []Service
	var target string
	var isFile bool
	refresh := func() {
		var err error
		if target, isFile, err = resolve(s.Config); err != nil {
			flash(err.Error())
			return
		}
		if svcs, err = listServices(target, isFile); err != nil {
			flash(err.Error())
		}
		ok, routers := status(s.Traefik)
		conn.SetText("Traefik unreachable")
		if ok {
			conn.SetText("Traefik connected")
		}
		mode := "dir"
		if isFile {
			mode = "file"
		}
		w.SetTitle(fmt.Sprintf("Agent Proxy — %s (%s)", target, mode))
		base := strings.TrimRight(s.Base, "/")
		rows = make([][]string, len(svcs))
		for i, v := range svcs {
			rows[i] = []string{v.Name, "unmanaged", "", "", "", v.Source}
			if v.Managed {
				st := routers[v.Name]
				if st == "" {
					st = "unknown"
				}
				rows[i][1], rows[i][2], rows[i][3], rows[i][4] = st, base+"/"+v.Name+"/", v.Upstream, fmt.Sprint(len(v.Headers))
			}
		}
		for c, name := range cols {
			width := widget.NewLabel(name).MinSize().Width
			for _, r := range rows {
				width = max(width, widget.NewLabel(r[c]).MinSize().Width)
			}
			table.SetColumnWidth(c, width)
		}
		table.Refresh()
	}
	selected := func() (Service, bool) {
		if sel < 0 || sel >= len(svcs) {
			flash("select a service first")
			return Service{}, false
		}
		if !svcs[sel].Managed {
			flash(svcs[sel].Name + " is not in agent-proxy shape; edit it by hand")
			return Service{}, false
		}
		return svcs[sel], true
	}
	report := func(err error) {
		if err != nil {
			warn(w, err)
		}
		refresh()
	}

	buttons := container.NewHBox(
		widget.NewButton("New", func() { editService(w, s, target, isFile, Service{}, report) }),
		widget.NewButton("Edit", func() {
			if svc, ok := selected(); ok {
				editService(w, s, target, isFile, svc, report)
			}
		}),
		widget.NewButton("Delete", func() {
			svc, ok := selected()
			if !ok {
				return
			}
			dialog.ShowConfirm("Delete", fmt.Sprintf("Delete service %q?", svc.Name), func(yes bool) {
				if yes {
					report(deleteService(target, isFile, svc.Name))
				}
			}, w)
		}),
		widget.NewButton("Refresh", refresh),
		widget.NewButton("Settings", func() {
			editSettings(w, &s, "", func(ok bool) {
				if ok {
					refresh()
				}
			})
		}),
	)
	w.SetContent(container.NewBorder(buttons, container.NewBorder(nil, nil, nil, conn, msg), nil, nil, table))
	if _, _, err := resolve(s.Config); err != nil {
		editSettings(w, &s, err.Error(), func(ok bool) {
			if ok {
				refresh()
			} else {
				w.Close()
			}
		})
	} else {
		refresh()
	}
	w.ShowAndRun()
}
