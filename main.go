package main

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image/color"
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
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/theme"
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
	Disabled bool              `json:"disabled"` // managed but router withheld, so Traefik does not route it
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
	routers := sub(h, "routers")
	delete(routers, s.Name)
	if !s.Disabled {
		routers[s.Name] = router
	}
	if len(routers) == 0 {
		delete(h, "routers")
	}
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
	services, _ := nested(doc, "http", "services").(map[string]any)
	// Every router, plus routerless services in agent-proxy shape (those are disabled services).
	names := map[string]bool{}
	for r := range routers {
		names[r] = true
	}
	for r := range services {
		if nested(doc, "http", "middlewares", r+"-headers") != nil {
			names[r] = true
		}
	}
	var out []Service
	for r := range names {
		s := Service{Name: r, Source: source}
		_, hasRouter := routers[r]
		svc, _ := nested(doc, "http", "routers", r, "service").(string)
		servers, _ := nested(doc, "http", "services", r, "loadBalancer", "servers").([]any)
		hdrs, hok := nested(doc, "http", "middlewares", r+"-headers", "headers", "customRequestHeaders").(map[string]any)
		if (!hasRouter || svc == r) && hok && len(servers) > 0 {
			if u, ok := nested(map[string]any{"s": servers[0]}, "s", "url").(string); ok {
				s.Managed, s.Disabled, s.Upstream, s.Headers = true, !hasRouter, u, map[string]string{}
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

//go:embed assets/icon.png
var iconPNG []byte

//go:embed assets/Rubik-Regular.ttf
var rubik []byte

//go:embed assets/Rubik-Medium.ttf
var rubikMedium []byte

//go:embed assets/FiraMono-Regular.otf
var firaMono []byte

// look follows the Traefik dashboard (Faency, light, blue primary): Rubik type, Radix blue, navy header.
type look struct{ fyne.Theme }

func hex(v uint32) color.Color {
	return color.NRGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 0xFF}
}

var (
	navy      = hex(0x031828) // faency deepBlue11
	navyMuted = hex(0xC3CFDA) // faency deepBlue6
)

var palette = map[fyne.ThemeColorName]color.Color{
	theme.ColorNameBackground:        hex(0xF4F5F6), // grayBlue2
	theme.ColorNameForeground:        hex(0x1E2124), // grayBlue12
	theme.ColorNamePrimary:           hex(0x0091FF), // radix blue9
	theme.ColorNameFocus:             color.NRGBA{R: 0x00, G: 0x91, B: 0xFF, A: 0x66},
	theme.ColorNameSelection:         hex(0xE1F0FF), // blue4
	theme.ColorNameHover:             hex(0xF0F2F3), // grayBlue4
	theme.ColorNamePressed:           hex(0xCEE7FE), // blue5
	theme.ColorNameButton:            hex(0xFFFFFF),
	theme.ColorNameInputBackground:   hex(0xFFFFFF),
	theme.ColorNameInputBorder:       hex(0xD0D5D8), // grayBlue7
	theme.ColorNameSeparator:         hex(0xE2E5E7), // grayBlue6
	theme.ColorNameHeaderBackground:  hex(0xF3F4F5), // grayBlue3
	theme.ColorNameOverlayBackground: hex(0xFFFFFF),
	theme.ColorNameMenuBackground:    hex(0xFFFFFF),
	theme.ColorNamePlaceHolder:       hex(0x7C8892), // grayBlue10
	theme.ColorNameDisabled:          hex(0x7C8892),
	theme.ColorNameSuccess:           hex(0x30A46C), // radix green9
	theme.ColorNameWarning:           hex(0xF7680A), // faency orange9
	theme.ColorNameError:             hex(0xFF3366), // faency red9
}

var fonts = map[string]fyne.Resource{
	"regular": fyne.NewStaticResource("Rubik-Regular.ttf", rubik),
	"bold":    fyne.NewStaticResource("Rubik-Medium.ttf", rubikMedium),
	"mono":    fyne.NewStaticResource("FiraMono-Regular.otf", firaMono),
}

func (l look) Color(n fyne.ThemeColorName, _ fyne.ThemeVariant) color.Color {
	if c, ok := palette[n]; ok {
		return c
	}
	return l.Theme.Color(n, theme.VariantLight)
}

func (l look) Font(st fyne.TextStyle) fyne.Resource {
	name := "regular"
	switch {
	case st.Monospace:
		name = "mono"
	case st.Bold:
		name = "bold"
	}
	if f, ok := fonts[name]; ok {
		return f
	}
	return l.Theme.Font(st)
}

func (l look) Size(n fyne.ThemeSizeName) float32 {
	if n == theme.SizeNamePadding {
		return 6
	}
	return l.Theme.Size(n)
}

// cols lays children out in fixed-width columns; a 0 width takes the remaining space.
type cols []float32

func (c cols) MinSize(objs []fyne.CanvasObject) fyne.Size {
	var w, h float32
	for i, o := range objs {
		w += max(c[i], o.MinSize().Width)
		h = max(h, o.MinSize().Height)
	}
	return fyne.NewSize(w, h)
}

func (c cols) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	var x, fixed float32
	for _, w := range c {
		fixed += w
	}
	for i, o := range objs {
		w := c[i]
		if w == 0 {
			w = size.Width - fixed
		}
		o.Resize(fyne.NewSize(w, size.Height))
		o.Move(fyne.NewPos(x, 0))
		x += w
	}
}

// clickRow is a list row that reports double clicks; single taps still select through the list.
type clickRow struct {
	widget.BaseWidget
	content  *fyne.Container
	onDouble func()
}

func newClickRow(c *fyne.Container) *clickRow {
	r := &clickRow{content: c}
	r.ExtendBaseWidget(r)
	return r
}

func (r *clickRow) CreateRenderer() fyne.WidgetRenderer { return widget.NewSimpleRenderer(r.content) }

func (r *clickRow) DoubleTapped(*fyne.PointEvent) {
	if r.onDouble != nil {
		r.onDouble()
	}
}

func label(text string, st fyne.TextStyle) *widget.Label {
	l := widget.NewLabel(text)
	l.TextStyle = st
	return l
}

func dot(c color.Color) fyne.CanvasObject {
	return container.NewCenter(container.New(layout.NewGridWrapLayout(fyne.NewSize(10, 10)), canvas.NewCircle(c)))
}

func warn(w fyne.Window, err error) { dialog.ShowError(err, w) }

// form shows a dialog; submit runs on Save and the form reopens (values intact) when it errors. cancel runs when dismissed.
// The returned func re-fits the dialog after its content grows or shrinks.
func form(w fyne.Window, title, note string, items []*widget.FormItem, submit func() error, cancel func()) func() {
	if note != "" {
		l := widget.NewLabel(note)
		l.Wrapping = fyne.TextWrapWord
		items = append([]*widget.FormItem{{Widget: l}}, items...)
	}
	var cur dialog.Dialog
	fit := func() { cur.Resize(fyne.NewSize(680, 0)) }
	var show func()
	show = func() {
		cur = dialog.NewForm(title, "Save", "Cancel", items, func(ok bool) {
			if !ok {
				if cancel != nil {
					cancel()
				}
			} else if err := submit(); err != nil {
				warn(w, err)
				show()
			}
		}, w)
		fit()
		cur.Show()
	}
	show()
	return fit
}

// Auth kinds that fold into one static header; Traefik cannot sign requests, so signed schemes are out.
const (
	noAuth = "No auth"
	bearer = "Bearer token"
	basic  = "Basic auth"
	apiKey = "API key"
)

// splitAuth pulls the auth header out of h: Authorization (Bearer or Basic) or X-API-Key.
// Other headers, including unrecognised Authorization values, come back in rest for the plain list.
func splitAuth(h map[string]string) (kind, name, user, secret string, rest map[string]string) {
	rest = map[string]string{}
	for k, v := range h {
		rest[k] = v
	}
	if v, ok := rest["Authorization"]; ok {
		if t, ok := strings.CutPrefix(v, "Bearer "); ok {
			delete(rest, "Authorization")
			return bearer, "", "", t, rest
		}
		if b, ok := strings.CutPrefix(v, "Basic "); ok {
			if dec, err := base64.StdEncoding.DecodeString(b); err == nil {
				delete(rest, "Authorization")
				u, p, _ := strings.Cut(string(dec), ":")
				return basic, "", u, p, rest
			}
		}
	}
	if v, ok := rest["X-API-Key"]; ok {
		delete(rest, "X-API-Key")
		return apiKey, "X-API-Key", "", v, rest
	}
	return noAuth, "", "", "", rest
}

// joinAuth is the inverse of splitAuth: adds the auth header for kind to h.
func joinAuth(h map[string]string, kind, name, user, secret string) error {
	switch kind {
	case bearer:
		if secret == "" {
			return errors.New("bearer token is required")
		}
		h["Authorization"] = "Bearer " + secret
	case basic:
		if user == "" {
			return errors.New("basic auth username is required")
		}
		h["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+secret))
	case apiKey:
		if strings.TrimSpace(name) == "" {
			return errors.New("auth header name is required")
		}
		h[strings.TrimSpace(name)] = secret
	}
	return nil
}

func fields(pairs ...any) fyne.CanvasObject {
	f := container.New(layout.NewFormLayout())
	for i := 0; i < len(pairs); i += 2 {
		f.Add(widget.NewLabel(pairs[i].(string)))
		f.Add(pairs[i+1].(fyne.CanvasObject))
	}
	return f
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
	file := widget.NewButton("Choose file", func() {
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
	dir := widget.NewButton("Choose folder", func() {
		d := dialog.NewFolderOpen(func(u fyne.ListableURI, _ error) {
			if u != nil {
				cfg.SetText(u.Path())
			}
		}, w)
		d.Resize(fyne.NewSize(800, 600))
		d.Show()
	})
	form(w, "Settings", problem, []*widget.FormItem{
		widget.NewFormItem("Traefik config", container.NewBorder(nil, nil, nil, container.NewHBox(file, dir), cfg)),
		widget.NewFormItem("Traefik API", api),
		widget.NewFormItem("Agents call", base),
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
	fit := func() {}

	// Auth section: one dropdown, fields swap with the kind.
	kind, hname, user, secret, rest := splitAuth(old.Headers)
	hdr, userE := entry(hname, "Header name"), entry(user, "")
	secretE := widget.NewPasswordEntry()
	secretE.SetText(secret)
	authFields := container.NewVBox()
	auth := widget.NewSelect([]string{noAuth, bearer, basic, apiKey}, func(k string) {
		authFields.Objects = nil
		switch k {
		case bearer:
			authFields.Add(fields("Token", secretE))
		case basic:
			authFields.Add(fields("Username", userE, "Password", secretE))
		case apiKey:
			if hdr.Text == "" {
				hdr.SetText("X-API-Key")
			}
			authFields.Add(fields("Header", hdr, "Key", secretE))
		}
		authFields.Refresh()
		fit()
	})
	auth.SetSelected(kind)

	// Header list: one row per header, remove per row, add at the bottom.
	rows := container.NewVBox()
	rowEntries := map[fyne.CanvasObject][2]*widget.Entry{}
	addRow := func(k, v string) {
		kE, vE := entry(k, "Header name"), entry(v, "Value")
		var row fyne.CanvasObject
		rm := widget.NewButtonWithIcon("", theme.ContentRemoveIcon(), func() {
			rows.Remove(row)
			delete(rowEntries, row)
			fit()
		})
		row = container.NewBorder(nil, nil, nil, rm, container.NewGridWithColumns(2, kE, vE))
		rowEntries[row] = [2]*widget.Entry{kE, vE}
		rows.Add(row)
		fit()
	}
	keys := make([]string, 0, len(rest))
	for k := range rest {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		addRow(k, rest[k])
	}
	addBtn := widget.NewButtonWithIcon("Add header", theme.ContentAddIcon(), func() { addRow("", "") })

	title := "Add service"
	if old.Name != "" {
		title = "Edit " + old.Name
	}
	fit = form(w, title, "", []*widget.FormItem{
		widget.NewFormItem("Name", name),
		widget.NewFormItem("Upstream", up),
		widget.NewFormItem("Auth", auth),
		widget.NewFormItem("", authFields),
		widget.NewFormItem("Other headers", container.NewVBox(rows, container.NewHBox(addBtn))),
	}, func() error {
		h := map[string]string{}
		for _, row := range rows.Objects {
			e := rowEntries[row]
			k := strings.TrimSpace(e[0].Text)
			if k == "" && strings.TrimSpace(e[1].Text) == "" {
				continue
			}
			h[k] = e[1].Text
		}
		if err := joinAuth(h, auth.Selected, hdr.Text, userE.Text, secretE.Text); err != nil {
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

	a := app.New()
	a.Settings().SetTheme(look{theme.DefaultTheme()})
	a.SetIcon(fyne.NewStaticResource("icon.png", iconPNG))
	w := a.NewWindow("Agent Proxy")
	w.Resize(fyne.NewSize(1000, 560))

	// Row columns: dot, name, proxy URL, arrow, upstream, status, headers, delete. Widths are measured in refresh.
	mono, bold := fyne.TextStyle{Monospace: true}, fyne.TextStyle{Bold: true}
	widths := cols{24, 0, 0, 0, 0, 0, 0, 40, 40}
	type row struct {
		svc    Service
		status string
		proxy  string
	}
	var rows []row
	var edit, del, toggle func(Service)
	list := widget.NewList(
		func() int { return len(rows) },
		func() fyne.CanvasObject {
			tg, rm := widget.NewButtonWithIcon("", theme.MediaPauseIcon(), nil), widget.NewButtonWithIcon("", theme.DeleteIcon(), nil)
			tg.Importance, rm.Importance = widget.LowImportance, widget.LowImportance
			return newClickRow(container.New(widths, dot(color.Transparent), label("", bold), label("", mono), widget.NewLabel("→"), label("", mono), widget.NewLabel(""), widget.NewLabel(""), tg, rm))
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			r, cr := rows[i], o.(*clickRow)
			objs := cr.content.Objects
			cr.onDouble = func() { edit(r.svc) }
			tg, rm := objs[7].(*widget.Button), objs[8].(*widget.Button)
			tg.OnTapped, rm.OnTapped = func() { toggle(r.svc) }, func() { del(r.svc) }
			tg.Hidden, rm.Hidden = !r.svc.Managed, !r.svc.Managed
			tg.SetIcon(theme.MediaPauseIcon())
			if r.svc.Disabled {
				tg.SetIcon(theme.MediaPlayIcon())
			}
			c := objs[0].(*fyne.Container).Objects[0].(*fyne.Container).Objects[0].(*canvas.Circle)
			c.FillColor = map[string]color.Color{"enabled": palette[theme.ColorNameSuccess], "unmanaged": palette[theme.ColorNameDisabled], "disabled": palette[theme.ColorNameDisabled]}[r.status]
			if c.FillColor == nil {
				c.FillColor = palette[theme.ColorNameWarning]
			}
			c.Refresh()
			n := len(r.svc.Headers)
			texts := []string{"", r.svc.Name, r.proxy, "→", r.svc.Upstream, r.status, fmt.Sprintf("%d headers", n)}
			if n == 1 {
				texts[6] = "1 header"
			}
			note := objs[2].(*widget.Label)
			note.TextStyle = mono
			if !r.svc.Managed {
				note.TextStyle = fyne.TextStyle{}
				texts = []string{"", r.svc.Name, "not managed by Agent Proxy (" + r.svc.Source + ")", "", "", "", ""}
			}
			for i := 1; i < len(texts); i++ {
				objs[i].(*widget.Label).SetText(texts[i])
			}
		},
	)
	head := container.New(widths, widget.NewLabel(""), label("Service", bold), label("Agents call", bold), widget.NewLabel(""), label("Upstream", bold), label("Status", bold), label("Headers", bold), widget.NewLabel(""), widget.NewLabel(""))
	empty := widget.NewLabel("No services yet. Add one to give agents a proxied path to an upstream API.")
	empty.Alignment = fyne.TextAlignCenter

	title, where, conn := canvas.NewText("Agent Proxy", color.White), canvas.NewText("", navyMuted), canvas.NewText("", color.White)
	title.TextStyle, title.TextSize, where.TextStyle = bold, 20, mono
	connDot := dot(color.Transparent)
	header := container.NewStack(canvas.NewRectangle(navy), container.NewPadded(container.NewPadded(container.NewBorder(nil, nil,
		container.NewHBox(container.NewCenter(title), container.NewCenter(where)), container.NewHBox(connDot, container.NewCenter(conn))))))
	msg := widget.NewLabel("")
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
		c := connDot.(*fyne.Container).Objects[0].(*fyne.Container).Objects[0].(*canvas.Circle)
		c.FillColor, conn.Text = palette[theme.ColorNameDisabled], "Traefik unreachable"
		if ok {
			c.FillColor, conn.Text = palette[theme.ColorNameSuccess], "Traefik connected"
		}
		c.Refresh()
		conn.Refresh()
		where.Text = target
		where.Refresh()
		base := strings.TrimRight(s.Base, "/")
		rows = rows[:0]
		for _, v := range svcs {
			r := row{svc: v, status: "unmanaged"}
			if v.Managed {
				r.proxy = base + "/" + v.Name + "/"
				// Our "disabled" means no router; Traefik's own "disabled" means it refused the router.
				switch r.status = routers[v.Name]; {
				case v.Disabled:
					r.status = "disabled"
				case r.status == "":
					r.status = "unknown"
				case r.status == "disabled":
					r.status = "error"
				}
			}
			rows = append(rows, r)
		}
		measure := func(st fyne.TextStyle, texts ...string) float32 {
			var w float32
			for _, t := range texts {
				w = max(w, label(t, st).MinSize().Width)
			}
			return w
		}
		for i, r := range rows {
			if i == 0 {
				widths[1], widths[2], widths[4], widths[5] = measure(bold, "Service"), measure(bold, "Agents call"), measure(bold, "Upstream"), measure(bold, "Status")
			}
			widths[1] = max(widths[1], measure(bold, r.svc.Name))
			widths[2] = max(widths[2], measure(mono, r.proxy))
			widths[4] = max(widths[4], measure(mono, r.svc.Upstream))
			widths[5] = max(widths[5], measure(fyne.TextStyle{}, r.status))
		}
		widths[3] = measure(fyne.TextStyle{}, "→")
		if len(rows) == 0 {
			empty.Show()
		} else {
			empty.Hide()
		}
		head.Refresh()
		list.Refresh()
	}
	report := func(err error) {
		if err != nil {
			warn(w, err)
		}
		refresh()
	}
	edit = func(svc Service) {
		if !svc.Managed {
			flash(svc.Name + " was not created by Agent Proxy; edit its YAML by hand")
			return
		}
		editService(w, s, target, isFile, svc, report)
	}
	toggle = func(svc Service) {
		svc.Disabled = !svc.Disabled
		report(saveService(target, isFile, svc, entryPoints(s)))
	}
	del = func(svc Service) {
		dialog.ShowConfirm("Delete service", fmt.Sprintf("Delete %s? Agents will no longer reach it through the proxy.", svc.Name), func(yes bool) {
			if yes {
				report(deleteService(target, isFile, svc.Name))
			}
		}, w)
	}

	add := widget.NewButton("Add service", func() { editService(w, s, target, isFile, Service{}, report) })
	add.Importance = widget.HighImportance
	buttons := container.NewHBox(
		add,
		layout.NewSpacer(),
		widget.NewButton("Refresh", refresh),
		widget.NewButton("Settings", func() {
			editSettings(w, &s, "", func(ok bool) {
				if ok {
					refresh()
				}
			})
		}),
	)
	body := container.NewBorder(container.NewVBox(buttons, head, widget.NewSeparator()), nil, nil, nil, container.NewStack(list, empty))
	w.SetContent(container.NewBorder(header, container.NewPadded(msg), nil, nil, container.NewPadded(body)))
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
