// BlayUGC Wig Texture Tool
// Small local app: browse Wigs\<Year>\<Mon Year>\<Folder>, drop transparent
// hair PNGs, and get the original + 6 skin-background versions saved next to it.
package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed ui.html favicon.png
var uiFS embed.FS

const (
	appName     = "BlayUGC Wig Tool"
	listenAddr  = "127.0.0.1:47821"
	pingMarker  = "blayugc-wigtool"
	defaultRoot = `C:\Users\9xbla\OneDrive\Belgeler\BlayUGC\ASSETS\Wigs`
)

var monthNames = []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}

// ---------- settings ----------

type Settings struct {
	Root      string    `json:"root"`
	Skins     []string  `json:"skins"`
	Overwrite bool      `json:"overwrite"`
	Recolors  []Recolor `json:"recolors"`
}

var (
	settingsMu sync.Mutex
	settings   = Settings{
		Root: defaultRoot,
		// Skin1 (lightest) -> Skin6, sampled from the swatches used for Texture Comm 1
		Skins:     []string{"#EAB892", "#CC8E69", "#A05F35", "#694028", "#564236", "#7B5B46"},
		Overwrite: true,
		Recolors:  defaultRecolors,
	}
)

func settingsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "BlayUGC WigTool", "settings.json")
}

func loadSettings() {
	b, err := os.ReadFile(settingsPath())
	if err != nil {
		return
	}
	var s Settings
	if json.Unmarshal(b, &s) == nil {
		if s.Root != "" {
			settings.Root = s.Root
		}
		if len(s.Skins) == 6 {
			settings.Skins = s.Skins
		}
		settings.Overwrite = s.Overwrite
		if s.Recolors != nil && validRecolors(s.Recolors) == nil {
			settings.Recolors = s.Recolors
		}
	}
}

func saveSettings() error {
	p := settingsPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(settings, "", "  ")
	return os.WriteFile(p, b, 0o644)
}

func getSettings() Settings {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	s := settings
	s.Skins = append([]string(nil), settings.Skins...)
	s.Recolors = append([]Recolor(nil), settings.Recolors...)
	return s
}

// ---------- paths ----------

var badName = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

func cleanName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if s == "." || s == ".." || badName.MatchString(s) {
		return "", fmt.Errorf("invalid folder name: %q", s)
	}
	return s, nil
}

// target builds Root\year\month\folder, refusing anything that escapes Root.
func target(year, month, folder string) (string, error) {
	root := getSettings().Root
	parts := []string{root}
	for _, p := range []string{year, month, folder} {
		c, err := cleanName(p)
		if err != nil {
			return "", err
		}
		if c == "" {
			break
		}
		parts = append(parts, c)
	}
	return filepath.Join(parts...), nil
}

func listDirs(p string) []string {
	ents, err := os.ReadDir(p)
	if err != nil {
		return []string{}
	}
	out := []string{}
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	return out
}

func monthIndex(name string) int {
	f := strings.Fields(name)
	if len(f) > 0 {
		for i, m := range monthNames {
			if strings.EqualFold(f[0], m) {
				return i
			}
		}
	}
	return 99
}

// natural sort so "Texture Comm 10" comes after "Texture Comm 2"
var numRe = regexp.MustCompile(`\d+|\D+`)

func naturalLess(a, b string) bool {
	pa, pb := numRe.FindAllString(strings.ToLower(a), -1), numRe.FindAllString(strings.ToLower(b), -1)
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] == pb[i] {
			continue
		}
		na, ea := strconv.Atoi(pa[i])
		nb, eb := strconv.Atoi(pb[i])
		if ea == nil && eb == nil {
			return na < nb
		}
		return pa[i] < pb[i]
	}
	return len(pa) < len(pb)
}

// ---------- image work ----------

func parseHex(h string) (color.NRGBA, error) {
	h = strings.TrimPrefix(strings.TrimSpace(h), "#")
	if len(h) != 6 {
		return color.NRGBA{}, fmt.Errorf("bad color %q", h)
	}
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return color.NRGBA{}, err
	}
	return color.NRGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 255}, nil
}

func hasTransparency(img image.Image) bool {
	switch m := img.(type) {
	case *image.NRGBA:
		for i := 3; i < len(m.Pix); i += 4 {
			if m.Pix[i] < 250 {
				return true
			}
		}
		return false
	case *image.RGBA:
		for i := 3; i < len(m.Pix); i += 4 {
			if m.Pix[i] < 250 {
				return true
			}
		}
		return false
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y += 4 {
		for x := b.Min.X; x < b.Max.X; x += 4 {
			if _, _, _, a := img.At(x, y).RGBA(); a < 0xF000 {
				return true
			}
		}
	}
	return false
}

func composite(src image.Image, bg color.NRGBA) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{bg}, image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Over)
	return dst
}

func writePNG(path string, img image.Image) error {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

var skinSuffix = regexp.MustCompile(`(?i)Skin[1-6]$`)

type FileResult struct {
	Name    string   `json:"name"`
	Created []string `json:"created"`
	Skipped []string `json:"skipped"`
	Warning string   `json:"warning,omitempty"`
	Error   string   `json:"error,omitempty"`
}

func processOne(dir, filename string, data []byte, s Settings) FileResult {
	base := strings.TrimSuffix(filename, filepath.Ext(filename))
	res := FileResult{Name: base, Created: []string{}, Skipped: []string{}}
	if !strings.EqualFold(filepath.Ext(filename), ".png") {
		res.Error = "Not a PNG file"
		return res
	}
	if _, err := cleanName(base); err != nil || base == "" {
		res.Error = "Invalid file name"
		return res
	}
	if skinSuffix.MatchString(base) {
		res.Error = "Already a skin version (ends with SkinN) — drop the plain texture instead"
		return res
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		res.Error = "Could not read PNG: " + err.Error()
		return res
	}

	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }

	// original, saved byte-for-byte
	orig := filepath.Join(dir, base+".png")
	if exists(orig) && !s.Overwrite {
		res.Skipped = append(res.Skipped, base+".png")
	} else if err := os.WriteFile(orig, data, 0o644); err != nil {
		res.Error = err.Error()
		return res
	} else {
		res.Created = append(res.Created, base+".png")
	}

	if !hasTransparency(img) {
		res.Warning = "No transparent background found, so skin versions were not made"
		return res
	}

	makeSkins(dir, base, img, s, &res)
	return res
}

// makeSkins writes BaseSkin1..6.png. With s.Overwrite false, existing files are left alone.
func makeSkins(dir, base string, img image.Image, s Settings, res *FileResult) {
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	type out struct {
		name string
		err  error
		skip bool
	}
	outs := make([]out, len(s.Skins))
	var wg sync.WaitGroup
	for i, hex := range s.Skins {
		wg.Add(1)
		go func(i int, hex string) {
			defer wg.Done()
			name := fmt.Sprintf("%sSkin%d.png", base, i+1)
			outs[i].name = name
			p := filepath.Join(dir, name)
			if exists(p) && !s.Overwrite {
				outs[i].skip = true
				return
			}
			c, err := parseHex(hex)
			if err != nil {
				outs[i].err = err
				return
			}
			outs[i].err = writePNG(p, composite(img, c))
		}(i, hex)
	}
	wg.Wait()
	for _, o := range outs {
		switch {
		case o.err != nil:
			res.Error = o.name + ": " + o.err.Error()
		case o.skip:
			res.Skipped = append(res.Skipped, o.name)
		default:
			res.Created = append(res.Created, o.name)
		}
	}
}

// rebuildSkins moves every BaseSkinN.png to the Recycle Bin, then makes Skin1-6
// fresh from the current Base.png (after it was edited).
func rebuildSkins(dir, base string, s Settings) FileResult {
	res := FileResult{Name: base, Created: []string{}, Skipped: []string{}}
	if n, err := cleanName(base); err != nil || n == "" || skinSuffix.MatchString(base) {
		res.Error = "Invalid texture name"
		return res
	}
	if _, err := os.Stat(filepath.Join(dir, base+".png")); err != nil {
		res.Error = "Could not find " + base + ".png"
		return res
	}
	re := regexp.MustCompile(`(?i)^` + regexp.QuoteMeta(base) + `Skin\d+\.png$`)
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.IsDir() || !re.MatchString(e.Name()) {
			continue
		}
		if err := recycle([]string{filepath.Join(dir, e.Name())}); err != nil {
			res.Error = "Could not remove " + e.Name() + ": " + err.Error()
			return res
		}
	}
	return generateMissing(dir, base, s)
}

// generateMissing rebuilds only the skin versions that don't exist yet for Base.png.
func generateMissing(dir, base string, s Settings) FileResult {
	res := FileResult{Name: base, Created: []string{}, Skipped: []string{}}
	if n, err := cleanName(base); err != nil || n == "" || skinSuffix.MatchString(base) {
		res.Error = "Invalid texture name"
		return res
	}
	data, err := os.ReadFile(filepath.Join(dir, base+".png"))
	if err != nil {
		res.Error = "Could not open " + base + ".png"
		return res
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		res.Error = "Could not read PNG: " + err.Error()
		return res
	}
	if !hasTransparency(img) {
		res.Warning = "No transparent background found, so skin versions were not made"
		return res
	}
	s.Overwrite = false
	makeSkins(dir, base, img, s, &res)
	return res
}

// ---------- thumbnails ----------

type thumbKey struct {
	path string
	mod  int64
	size int
}

var (
	thumbMu    sync.Mutex
	thumbCache = map[thumbKey][]byte{}
)

func makeThumb(path string, size int) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	key := thumbKey{path, st.ModTime().UnixNano(), size}
	thumbMu.Lock()
	if b, ok := thumbCache[key]; ok {
		thumbMu.Unlock()
		return b, nil
	}
	thumbMu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	src, err := png.Decode(f)
	if err != nil {
		return nil, err
	}
	sb := src.Bounds()
	w, h := size, size
	if sb.Dx() > sb.Dy() {
		h = max(1, size*sb.Dy()/sb.Dx())
	} else {
		w = max(1, size*sb.Dx()/sb.Dy())
	}
	nrgba := image.NewNRGBA(sb)
	draw.Draw(nrgba, sb, src, sb.Min, draw.Src)
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	// box filter downscale
	for y := 0; y < h; y++ {
		y0, y1 := y*sb.Dy()/h, (y+1)*sb.Dy()/h
		for x := 0; x < w; x++ {
			x0, x1 := x*sb.Dx()/w, (x+1)*sb.Dx()/w
			var r, g, b, a, n uint64
			step := max(1, (x1-x0)/6)
			for yy := y0; yy < y1; yy += step {
				for xx := x0; xx < x1; xx += step {
					i := yy*nrgba.Stride + xx*4
					pa := uint64(nrgba.Pix[i+3])
					r += uint64(nrgba.Pix[i]) * pa
					g += uint64(nrgba.Pix[i+1]) * pa
					b += uint64(nrgba.Pix[i+2]) * pa
					a += pa
					n++
				}
			}
			o := y*dst.Stride + x*4
			if a > 0 {
				dst.Pix[o], dst.Pix[o+1], dst.Pix[o+2] = uint8(r/a), uint8(g/a), uint8(b/a)
			}
			if n > 0 {
				dst.Pix[o+3] = uint8(a / n)
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, err
	}
	thumbMu.Lock()
	if len(thumbCache) > 2000 {
		thumbCache = map[thumbKey][]byte{}
	}
	thumbCache[key] = buf.Bytes()
	thumbMu.Unlock()
	return buf.Bytes(), nil
}

// ---------- http ----------

var lastPing = time.Now()
var pingMu sync.Mutex

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func q(r *http.Request, k string) string { return r.URL.Query().Get(k) }

func routes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := uiFS.ReadFile("ui.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})

	mux.HandleFunc("/favicon.png", func(w http.ResponseWriter, r *http.Request) {
		b, _ := uiFS.ReadFile("favicon.png")
		w.Header().Set("Content-Type", "image/png")
		w.Write(b)
	})

	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		pingMu.Lock()
		lastPing = time.Now()
		pingMu.Unlock()
		io.WriteString(w, pingMarker)
	})

	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		s := getSettings()
		_, err := os.Stat(s.Root)
		years := listDirs(s.Root)
		sort.Slice(years, func(i, j int) bool { return naturalLess(years[i], years[j]) })
		now := time.Now()
		writeJSON(w, map[string]any{
			"settings":   s,
			"rootExists": err == nil,
			"years":      years,
			"nowYear":    strconv.Itoa(now.Year()),
			"nowMonth":   fmt.Sprintf("%s %d", monthNames[now.Month()-1], now.Year()),
		})
	})

	mux.HandleFunc("/api/months", func(w http.ResponseWriter, r *http.Request) {
		p, err := target(q(r, "year"), "", "")
		if err != nil {
			fail(w, err)
			return
		}
		m := listDirs(p)
		sort.Slice(m, func(i, j int) bool {
			a, b := monthIndex(m[i]), monthIndex(m[j])
			if a != b {
				return a < b
			}
			return naturalLess(m[i], m[j])
		})
		writeJSON(w, m)
	})

	mux.HandleFunc("/api/folders", func(w http.ResponseWriter, r *http.Request) {
		p, err := target(q(r, "year"), q(r, "month"), "")
		if err != nil {
			fail(w, err)
			return
		}
		type F struct {
			Name  string `json:"name"`
			Count int    `json:"count"`
		}
		out := []F{}
		for _, d := range listDirs(p) {
			n := 0
			ents, _ := os.ReadDir(filepath.Join(p, d))
			for _, e := range ents {
				if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".png") {
					n++
				}
			}
			out = append(out, F{d, n})
		}
		sort.Slice(out, func(i, j int) bool { return naturalLess(out[i].Name, out[j].Name) })
		writeJSON(w, out)
	})

	mux.HandleFunc("/api/files", func(w http.ResponseWriter, r *http.Request) {
		p, err := target(q(r, "year"), q(r, "month"), q(r, "folder"))
		if err != nil {
			fail(w, err)
			return
		}
		type F struct {
			Name string `json:"name"`
			Mod  int64  `json:"mod"`
		}
		out := []F{}
		ents, _ := os.ReadDir(p)
		for _, e := range ents {
			if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".png") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			out = append(out, F{e.Name(), info.ModTime().UnixMilli()})
		}
		sort.Slice(out, func(i, j int) bool { return naturalLess(out[i].Name, out[j].Name) })
		writeJSON(w, map[string]any{"path": p, "files": out})
	})

	// raw PNG from the open folder (used to recolor a Black.png that's already there)
	mux.HandleFunc("/file", func(w http.ResponseWriter, r *http.Request) {
		p, err := target(q(r, "year"), q(r, "month"), q(r, "folder"))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		name, err := cleanName(q(r, "name"))
		if err != nil || name == "" || !strings.EqualFold(filepath.Ext(name), ".png") {
			http.Error(w, "bad name", 400)
			return
		}
		b, err := os.ReadFile(filepath.Join(p, name))
		if err != nil {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	})

	mux.HandleFunc("/thumb", func(w http.ResponseWriter, r *http.Request) {
		p, err := target(q(r, "year"), q(r, "month"), q(r, "folder"))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		name, err := cleanName(q(r, "name"))
		if err != nil || name == "" {
			http.Error(w, "bad name", 400)
			return
		}
		size := 220
		if q(r, "big") != "" {
			size = 1024
		}
		b, err := makeThumb(filepath.Join(p, name), size)
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "max-age=86400")
		w.Write(b)
	})

	mux.HandleFunc("/api/mkdir", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Year, Month, Folder string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, err)
			return
		}
		if strings.TrimSpace(body.Year) == "" {
			fail(w, errors.New("year is required"))
			return
		}
		p, err := target(body.Year, body.Month, body.Folder)
		if err != nil {
			fail(w, err)
			return
		}
		if err := os.MkdirAll(p, 0o755); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]string{"path": p})
	})

	mux.HandleFunc("/api/process", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(256 << 20); err != nil {
			fail(w, err)
			return
		}
		year, month, folder := r.FormValue("year"), r.FormValue("month"), r.FormValue("folder")
		if year == "" || month == "" {
			fail(w, errors.New("pick a year and a month first"))
			return
		}
		dir, err := target(year, month, folder)
		if err != nil {
			fail(w, err)
			return
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fail(w, err)
			return
		}
		s := getSettings()
		results := []FileResult{}
		for _, fh := range r.MultipartForm.File["files"] {
			f, err := fh.Open()
			if err != nil {
				results = append(results, FileResult{Name: fh.Filename, Error: err.Error()})
				continue
			}
			data, err := io.ReadAll(f)
			f.Close()
			if err != nil {
				results = append(results, FileResult{Name: fh.Filename, Error: err.Error()})
				continue
			}
			results = append(results, processOne(dir, filepath.Base(fh.Filename), data, s))
		}
		writeJSON(w, map[string]any{"path": dir, "results": results})
	})

	mux.HandleFunc("/api/rebuild", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", 405)
			return
		}
		var body struct {
			Year, Month, Folder string
			Names               []string
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, err)
			return
		}
		dir, err := target(body.Year, body.Month, body.Folder)
		if err != nil {
			fail(w, err)
			return
		}
		s := getSettings()
		results := []FileResult{}
		for _, n := range body.Names {
			results = append(results, rebuildSkins(dir, n, s))
		}
		writeJSON(w, map[string]any{"path": dir, "results": results})
	})

	mux.HandleFunc("/api/generate", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Year, Month, Folder string
			Names               []string
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, err)
			return
		}
		dir, err := target(body.Year, body.Month, body.Folder)
		if err != nil {
			fail(w, err)
			return
		}
		s := getSettings()
		results := []FileResult{}
		for _, n := range body.Names {
			results = append(results, generateMissing(dir, n, s))
		}
		writeJSON(w, map[string]any{"path": dir, "results": results})
	})

	mux.HandleFunc("/api/delete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", 405)
			return
		}
		var body struct {
			Year, Month, Folder string
			Names               []string
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail(w, err)
			return
		}
		if body.Year == "" || body.Month == "" {
			fail(w, errors.New("no folder selected"))
			return
		}
		dir, err := target(body.Year, body.Month, body.Folder)
		if err != nil {
			fail(w, err)
			return
		}
		deleted, failed := []string{}, []string{}
		for _, n := range body.Names {
			name, err := cleanName(n)
			if err != nil || name == "" || !strings.EqualFold(filepath.Ext(name), ".png") {
				failed = append(failed, n)
				continue
			}
			p := filepath.Join(dir, name)
			if info, err := os.Stat(p); err != nil || info.IsDir() {
				failed = append(failed, n)
				continue
			}
			// one at a time so a single failure doesn't block the rest
			if err := recycle([]string{p}); err != nil {
				failed = append(failed, n+" ("+err.Error()+")")
				continue
			}
			deleted = append(deleted, name)
		}
		writeJSON(w, map[string]any{"deleted": deleted, "failed": failed})
	})

	mux.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		var s Settings
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			fail(w, err)
			return
		}
		if len(s.Skins) != 6 {
			fail(w, errors.New("need exactly 6 skin colors"))
			return
		}
		for _, h := range s.Skins {
			if _, err := parseHex(h); err != nil {
				fail(w, err)
				return
			}
		}
		s.Root = strings.TrimSpace(s.Root)
		if s.Root == "" {
			fail(w, errors.New("root folder is empty"))
			return
		}
		if s.Recolors == nil {
			s.Recolors = getSettings().Recolors
		}
		if err := validRecolors(s.Recolors); err != nil {
			fail(w, err)
			return
		}
		settingsMu.Lock()
		settings = s
		err := saveSettings()
		settingsMu.Unlock()
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("/api/open", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Year, Month, Folder, Select string }
		json.NewDecoder(r.Body).Decode(&body)
		p, err := target(body.Year, body.Month, body.Folder)
		if err != nil {
			fail(w, err)
			return
		}
		sel := ""
		if body.Select != "" {
			if n, err := cleanName(body.Select); err == nil && n != "" {
				sel = filepath.Join(p, n)
			}
		}
		if err := openExplorer(p, sel); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})

	addUpdateRoutes(mux)
	addRecolorRoutes(mux)
	return mux
}

func main() {
	url := "http://" + listenAddr + "/"

	updated := len(os.Args) > 1 && os.Args[1] == "--updated"
	ln, err := net.Listen("tcp", listenAddr)
	for i := 0; updated && err != nil && i < 40; i++ {
		// the previous version is still shutting down
		time.Sleep(250 * time.Millisecond)
		ln, err = net.Listen("tcp", listenAddr)
	}
	if err != nil {
		// Already running? Just open another window onto it.
		if resp, e := http.Get(url + "api/ping"); e == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(b) == pingMarker {
				openWindow(url)
				return
			}
		}
		showError("Could not start: port 47821 is in use.\n\n" + err.Error())
		return
	}

	loadSettings()
	go brandWindows()

	// Quit when the window has been closed (no heartbeat for a while).
	go func() {
		for range time.Tick(5 * time.Second) {
			pingMu.Lock()
			idle := time.Since(lastPing)
			pingMu.Unlock()
			if idle > 45*time.Second {
				os.Exit(0)
			}
		}
	}()

	if updated {
		// the existing window reconnects by itself
		go cleanupOldExe()
	} else {
		go func() {
			time.Sleep(150 * time.Millisecond)
			openWindow(url)
		}()
	}
	http.Serve(ln, routes())
}
