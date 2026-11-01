package main

// Recolor: turns the black base texture into every hair colour, the same way
// Photoshop's Color Overlay layer style does (blend mode "Color" or
// "Linear Dodge (Add)", 100% opacity, transparency kept).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Recolor struct {
	Name string `json:"name"`
	Hex  string `json:"hex"`
	Mode string `json:"mode"` // "color" or "add"
}

var defaultRecolors = []Recolor{
	{"Brown", "#180901", "color"},
	{"Blonde", "#C59369", "add"},
	{"LightBlonde", "#DCB38A", "add"},
	{"Ginger", "#994C23", "add"},
	{"Red", "#700000", "add"},
	{"Teal", "#1EC8C2", "add"},
	{"Pink", "#FF389A", "add"},
}

func validRecolors(list []Recolor) error {
	seen := map[string]bool{}
	for _, r := range list {
		n, err := cleanName(r.Name)
		if err != nil || n == "" || n != r.Name {
			return fmt.Errorf("invalid colour name %q", r.Name)
		}
		if skinSuffix.MatchString(n) {
			return fmt.Errorf("colour name %q can't end with SkinN", n)
		}
		if seen[strings.ToLower(n)] {
			return fmt.Errorf("colour name %q is used twice", n)
		}
		seen[strings.ToLower(n)] = true
		if _, err := parseHex(r.Hex); err != nil {
			return fmt.Errorf("%s: %v", n, err)
		}
		if r.Mode != "color" && r.Mode != "add" {
			return fmt.Errorf("%s: unknown blend mode %q", n, r.Mode)
		}
	}
	return nil
}

// --- Photoshop blend math (non-separable "Color" mode uses the PDF/PS formulas) ---

func lum(r, g, b float64) float64 { return 0.3*r + 0.59*g + 0.11*b }

func clipColor(r, g, b float64) (float64, float64, float64) {
	l := lum(r, g, b)
	n := min(r, g, b)
	x := max(r, g, b)
	if n < 0 {
		r = l + (r-l)*l/(l-n)
		g = l + (g-l)*l/(l-n)
		b = l + (b-l)*l/(l-n)
	}
	if x > 1 {
		r = l + (r-l)*(1-l)/(x-l)
		g = l + (g-l)*(1-l)/(x-l)
		b = l + (b-l)*(1-l)/(x-l)
	}
	return r, g, b
}

func setLum(r, g, b, l float64) (float64, float64, float64) {
	d := l - lum(r, g, b)
	return clipColor(r+d, g+d, b+d)
}

func to8(v float64) uint8 {
	v = v*255 + 0.5
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

func recolorImage(src image.Image, rc Recolor) (*image.NRGBA, error) {
	c, err := parseHex(rc.Hex)
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	img := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(img, img.Bounds(), src, b.Min, draw.Src)

	// Lookup tables: results depend only on the base pixel's value per channel
	// ("add") or on its luminosity ("color"), so precompute.
	var addR, addG, addB [256]uint8
	for v := 0; v < 256; v++ {
		addR[v] = uint8(min(255, v+int(c.R)))
		addG[v] = uint8(min(255, v+int(c.G)))
		addB[v] = uint8(min(255, v+int(c.B)))
	}
	cr, cg, cb := float64(c.R)/255, float64(c.G)/255, float64(c.B)/255

	p := img.Pix
	for i := 0; i < len(p); i += 4 {
		if p[i+3] == 0 {
			continue
		}
		switch rc.Mode {
		case "add":
			p[i], p[i+1], p[i+2] = addR[p[i]], addG[p[i+1]], addB[p[i+2]]
		case "color":
			l := lum(float64(p[i])/255, float64(p[i+1])/255, float64(p[i+2])/255)
			r, g, bb := setLum(cr, cg, cb, l)
			p[i], p[i+1], p[i+2] = to8(r), to8(g), to8(bb)
		}
	}
	return img, nil
}

func addRecolorRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/recolor", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(128 << 20); err != nil {
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
		var wanted []string
		json.Unmarshal([]byte(r.FormValue("colors")), &wanted)
		keepBlack := r.FormValue("keepBlack") == "1"
		withSkins := r.FormValue("skins") == "1"
		blackName := strings.TrimSpace(r.FormValue("blackName"))
		if blackName == "" {
			blackName = "Black"
		}
		if n, err := cleanName(blackName); err != nil || n == "" || skinSuffix.MatchString(n) {
			fail(w, fmt.Errorf("invalid name for the black texture: %q", blackName))
			return
		}

		fhs := r.MultipartForm.File["file"]
		if len(fhs) != 1 {
			fail(w, errors.New("drop one black texture at a time"))
			return
		}
		f, err := fhs[0].Open()
		if err != nil {
			fail(w, err)
			return
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			fail(w, err)
			return
		}
		src, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			fail(w, fmt.Errorf("could not read PNG: %v", err))
			return
		}
		if !hasTransparency(src) {
			fail(w, errors.New("this texture has no transparent background — export it from Photoshop with the background hidden"))
			return
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fail(w, err)
			return
		}

		s := getSettings()
		skinSettings := s
		if !withSkins {
			skinSettings.Skins = nil
		}

		var jobs []Recolor
		used := map[string]bool{}
		for _, rc := range s.Recolors {
			for _, n := range wanted {
				if strings.EqualFold(n, rc.Name) && !used[strings.ToLower(rc.Name)] {
					jobs = append(jobs, rc)
					used[strings.ToLower(rc.Name)] = true
				}
			}
		}
		// one-off colours picked on the colour wheel
		var custom []Recolor
		if v := r.FormValue("custom"); v != "" {
			if err := json.Unmarshal([]byte(v), &custom); err != nil {
				fail(w, fmt.Errorf("bad custom colours: %v", err))
				return
			}
			if err := validRecolors(custom); err != nil {
				fail(w, err)
				return
			}
		}
		presetNames := map[string]bool{}
		for _, rc := range s.Recolors {
			presetNames[strings.ToLower(rc.Name)] = true
		}
		for _, rc := range custom {
			if used[strings.ToLower(rc.Name)] || presetNames[strings.ToLower(rc.Name)] {
				fail(w, fmt.Errorf("%q is already a preset name, pick another name for the custom colour", rc.Name))
				return
			}
			used[strings.ToLower(rc.Name)] = true
			jobs = append(jobs, rc)
		}
		if keepBlack && used[strings.ToLower(blackName)] {
			fail(w, fmt.Errorf("%q is used twice", blackName))
			return
		}

		results := make([]FileResult, len(jobs))
		var wg sync.WaitGroup
		sem := make(chan struct{}, 4) // keep memory reasonable on 2K/4K textures
		for i, rc := range jobs {
			wg.Add(1)
			go func(i int, rc Recolor) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				img, err := recolorImage(src, rc)
				if err != nil {
					results[i] = FileResult{Name: rc.Name, Error: err.Error()}
					return
				}
				var buf bytes.Buffer
				if err := png.Encode(&buf, img); err != nil {
					results[i] = FileResult{Name: rc.Name, Error: err.Error()}
					return
				}
				results[i] = processOne(dir, rc.Name+".png", buf.Bytes(), skinSettings)
			}(i, rc)
		}
		var blackRes []FileResult
		if keepBlack {
			blackRes = []FileResult{processOne(dir, blackName+".png", data, skinSettings)}
		}
		wg.Wait()
		results = append(blackRes, results...)
		writeJSON(w, map[string]any{"path": filepath.Clean(dir), "results": results})
	})
}
