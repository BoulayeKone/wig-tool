package main

// Self-update from GitHub Releases.
// A release's tag is the version (v1.2.0) and it carries the .exe as an asset.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Set at build time: -ldflags "-X main.version=1.2.0"
var version = "dev"

const updateRepo = "BoulayeKone/wig-tool"

type release struct {
	Tag   string `json:"tag"`
	Notes string `json:"notes"`
	URL   string `json:"-"`
}

var (
	updMu     sync.Mutex
	latestRel *release
)

func parseVer(s string) []int {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	out := []int{}
	for _, p := range strings.Split(s, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

func newer(tag, cur string) bool {
	a, b := parseVer(tag), parseVer(cur)
	if a == nil {
		return false
	}
	if b == nil { // dev build
		return false
	}
	for i := 0; i < len(a) || i < len(b); i++ {
		x, y := 0, 0
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

var httpc = &http.Client{Timeout: 20 * time.Second}

func fetchLatest() (*release, error) {
	api := "https://api.github.com"
	if v := os.Getenv("WIGTOOL_UPDATE_API"); v != "" { // for testing only
		api = v
	}
	req, _ := http.NewRequest("GET", api+"/repos/"+updateRepo+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "BlayUGC-WigTool/"+version)
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, nil // no release yet
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub: %s", resp.Status)
	}
	var gh struct {
		TagName string `json:"tag_name"`
		Body    string `json:"body"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&gh); err != nil {
		return nil, err
	}
	r := &release{Tag: gh.TagName, Notes: gh.Body}
	for _, a := range gh.Assets {
		if strings.HasSuffix(strings.ToLower(a.Name), ".exe") {
			r.URL = a.URL
			break
		}
	}
	if r.URL == "" {
		return nil, errors.New("latest release has no .exe attached")
	}
	return r, nil
}

func applyUpdate(r *release) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	newPath, oldPath := exe+".new", exe+".old"

	dl := &http.Client{Timeout: 5 * time.Minute}
	resp, err := dl.Get(r.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed: %s", resp.Status)
	}
	f, err := os.OpenFile(newPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil || n < 1<<20 {
		os.Remove(newPath)
		if err == nil {
			err = errors.New("downloaded file looks incomplete")
		}
		return err
	}
	// Windows can't overwrite a running exe, but it can rename it.
	os.Remove(oldPath)
	if err := os.Rename(exe, oldPath); err != nil {
		os.Remove(newPath)
		return err
	}
	if err := os.Rename(newPath, exe); err != nil {
		os.Rename(oldPath, exe)
		return err
	}
	cmd := exec.Command(exe, "--updated")
	if err := hidden(cmd).Start(); err != nil {
		// roll back to the version that works
		os.Remove(exe)
		os.Rename(oldPath, exe)
		return err
	}
	// Free the port so the new version can take over; the open window reconnects.
	go func() {
		time.Sleep(400 * time.Millisecond)
		os.Exit(0)
	}()
	return nil
}

func cleanupOldExe() {
	if exe, err := os.Executable(); err == nil {
		for i := 0; i < 10; i++ {
			if err := os.Remove(exe + ".old"); err == nil || os.IsNotExist(err) {
				return
			}
			time.Sleep(time.Second)
		}
	}
}

func addUpdateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"version": version})
	})

	mux.HandleFunc("/api/update/check", func(w http.ResponseWriter, r *http.Request) {
		rel, err := fetchLatest()
		if err != nil {
			writeJSON(w, map[string]any{"current": version, "available": false, "error": err.Error()})
			return
		}
		updMu.Lock()
		latestRel = rel
		updMu.Unlock()
		if rel == nil || !newer(rel.Tag, version) {
			writeJSON(w, map[string]any{"current": version, "available": false})
			return
		}
		writeJSON(w, map[string]any{"current": version, "available": true, "latest": rel.Tag, "notes": rel.Notes})
	})

	mux.HandleFunc("/api/update/apply", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", 405)
			return
		}
		updMu.Lock()
		rel := latestRel
		updMu.Unlock()
		if rel == nil || !newer(rel.Tag, version) {
			fail(w, errors.New("no update available"))
			return
		}
		if err := applyUpdate(rel); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]string{"restarting": rel.Tag})
	})
}
