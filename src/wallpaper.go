package proxy

import (
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// ─── Wallpaper Helpers ──────────────────────────────────────────────────────

func endOfTodayUTC() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 0, time.UTC)
}

// serveWallpaperImage writes image data to the response with image/jpeg content type
// and optional Expires header. Used by both wallpaper handlers.
func serveWallpaperImage(w http.ResponseWriter, data []byte, expires *time.Time) {
	serveWallpaperImageTyped(w, data, expires, "image/jpeg")
}

// serveWallpaperImageTyped writes image data to the response with an optional
// Expires header and a caller-specified content type.
func serveWallpaperImageTyped(w http.ResponseWriter, data []byte, expires *time.Time, contentType string) {
	w.Header().Set("Content-Type", contentType)
	if expires != nil {
		w.Header().Set("Expires", expires.UTC().Format(http.TimeFormat))
	}
	w.Write(data)
}

// downloadImage fetches an image from the given URL with the specified User-Agent
// and timeout. Returns the raw image bytes, or nil on error.
func downloadImage(imageURL, userAgent string, timeout time.Duration) []byte {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest("GET", imageURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}
	return data
}

// saveCacheFile writes data to the cache file, creating the directory if needed.
// Returns true on success, false on error. Does not return 500 on failure.
func saveCacheFile(cacheFile string, data []byte) bool {
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0755); err != nil {
		return false
	}
	if err := os.WriteFile(cacheFile, data, 0644); err != nil {
		return false
	}
	return true
}

// isJPEG checks whether data starts with the JPEG magic bytes (0xFF 0xD8).
// Used only for freshly downloaded images, NOT for cached files (tests use fake data).
func isJPEG(data []byte) bool {
	return len(data) >= 2 && data[0] == 0xFF && data[1] == 0xD8
}

// isValidImage checks whether data starts with a recognized image format magic
// bytes (JPEG, PNG, or WebP). Used for validating freshly downloaded wallpapers.
func isValidImage(data []byte) bool {
	if len(data) < 2 {
		return false
	}
	// JPEG: FF D8
	if data[0] == 0xFF && data[1] == 0xD8 {
		return true
	}
	// PNG: 89 50 4E 47
	if len(data) >= 4 && data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47 {
		return true
	}
	// WebP: 52 49 46 46 ?? ?? ?? ?? 57 45 42 50 (RIFF....WEBP)
	if len(data) >= 12 && data[0] == 0x52 && data[1] == 0x49 && data[2] == 0x46 && data[3] == 0x46 &&
		data[8] == 0x57 && data[9] == 0x45 && data[10] == 0x42 && data[11] == 0x50 {
		return true
	}
	return false
}

// imageContentType returns the MIME content type for a valid image, or
// "image/jpeg" as fallback.
func imageContentType(data []byte) string {
	if len(data) >= 4 && data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47 {
		return "image/png"
	}
	if len(data) >= 12 && data[0] == 0x52 && data[1] == 0x49 && data[2] == 0x46 && data[3] == 0x46 &&
		data[8] == 0x57 && data[9] == 0x45 && data[10] == 0x42 && data[11] == 0x50 {
		return "image/webp"
	}
	return "image/jpeg"
}

// HandleRestart is the real restart handler routed from /api/restart.
// It responds with success and then exits the process with code 42 after
// a short delay so the HTTP response is flushed. §17.1 Route #13.
func (p *Proxy) HandleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "Restarting...",
	})
	// §29.2, §35.2: after 500ms, close HTTP server and exit(42).
	if p.httpServer == nil && p.exitFn == nil {
		return
	}
	time.AfterFunc(500*time.Millisecond, p.triggerRestart)
}

// upgradePeapixResolution replaces the resolution suffix in a peapix image URL
// with _3840 for UHD (3840x2160) output. Falls back to the original URL if the
// pattern doesn't match.
func upgradePeapixResolution(imageURL string) string {
	// Peapix URLs look like: https://img.peapix.com/<hash>_1920.jpg
	// Replace the _NNNN suffix before .jpg with _3840.
	re := regexp.MustCompile(`_\d+\.(jpg|jpeg|png)$`)
	return re.ReplaceAllString(imageURL, "_3840.$1")
}

// HandleBingWallpaper proxies the daily Bing wallpaper via peapix.com.
// Cache TTL: 24 hours (daily). Falls back to cached file on error.
func (p *Proxy) HandleBingWallpaper(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}
	cacheFile := ".cache/wallpaper.jpg"
	expires := endOfTodayUTC()

	// Fast path: check cache WITHOUT lock (read-only stat + read).
	// If file exists and was modified today (UTC), serve it immediately.
	if info, err := os.Stat(cacheFile); err == nil {
		today := time.Now().UTC().Format("2006-01-02")
		cachedDate := info.ModTime().UTC().Format("2006-01-02")
		if cachedDate == today {
			if data, err := os.ReadFile(cacheFile); err == nil {
				serveWallpaperImage(w, data, &expires)
				return
			}
		}
	}

	// Cache miss or expired — acquire lock to prevent duplicate fetches.
	p.wallpaperMu.Lock()
	defer p.wallpaperMu.Unlock()

	// Double-check: another goroutine may have refreshed the cache while we
	// waited for the lock.
	if info, err := os.Stat(cacheFile); err == nil {
		today := time.Now().UTC().Format("2006-01-02")
		cachedDate := info.ModTime().UTC().Format("2006-01-02")
		if cachedDate == today {
			if data, err := os.ReadFile(cacheFile); err == nil {
				serveWallpaperImage(w, data, &expires)
				return
			}
		}
	}

	// Fetch from peapix.com/bing/feed
	peapixURL := "https://peapix.com/bing/feed"
	peapixUA := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

	apiClient := &http.Client{Timeout: 15 * time.Second}
	apiReq, err := http.NewRequest("GET", peapixURL, nil)
	if err != nil {
		if data, err := os.ReadFile(cacheFile); err == nil {
			serveWallpaperImage(w, data, &expires)
			return
		}
		WriteJSON(w, http.StatusNotFound, map[string]interface{}{"error": "wallpaper not available"})
		return
	}
	apiReq.Header.Set("User-Agent", peapixUA)

	apiResp, err := apiClient.Do(apiReq)
	if err != nil {
		if data, err := os.ReadFile(cacheFile); err == nil {
			serveWallpaperImage(w, data, &expires)
			return
		}
		WriteJSON(w, http.StatusNotFound, map[string]interface{}{"error": "wallpaper not available"})
		return
	}
	defer apiResp.Body.Close()

	if apiResp.StatusCode != http.StatusOK {
		if data, err := os.ReadFile(cacheFile); err == nil {
			serveWallpaperImage(w, data, &expires)
			return
		}
		WriteJSON(w, http.StatusNotFound, map[string]interface{}{"error": "wallpaper not available"})
		return
	}

	body, err := io.ReadAll(apiResp.Body)
	if err != nil {
		if data, err := os.ReadFile(cacheFile); err == nil {
			serveWallpaperImage(w, data, &expires)
			return
		}
		WriteJSON(w, http.StatusNotFound, map[string]interface{}{"error": "wallpaper not available"})
		return
	}

	// Parse JSON: expect array of objects with fullUrl, imageUrl, or url field.
	var feed []map[string]interface{}
	if err := json.Unmarshal(body, &feed); err != nil {
		if data, err := os.ReadFile(cacheFile); err == nil {
			serveWallpaperImage(w, data, &expires)
			return
		}
		WriteJSON(w, http.StatusNotFound, map[string]interface{}{"error": "wallpaper not available"})
		return
	}
	if len(feed) == 0 {
		if data, err := os.ReadFile(cacheFile); err == nil {
			serveWallpaperImage(w, data, &expires)
			return
		}
		WriteJSON(w, http.StatusNotFound, map[string]interface{}{"error": "wallpaper not available"})
		return
	}

	// Extract image URL from first item: fullUrl, imageUrl, or url.
	var imageURL string
	if u, ok := feed[0]["fullUrl"].(string); ok && u != "" {
		imageURL = u
	} else if u, ok := feed[0]["imageUrl"].(string); ok && u != "" {
		imageURL = u
	} else if u, ok := feed[0]["url"].(string); ok && u != "" {
		imageURL = u
	}
	if imageURL == "" {
		if data, err := os.ReadFile(cacheFile); err == nil {
			serveWallpaperImage(w, data, &expires)
			return
		}
		WriteJSON(w, http.StatusNotFound, map[string]interface{}{"error": "wallpaper not available"})
		return
	}

	// Upgrade to UHD (3840) variant for large screens — replaces _1920, _640, etc.
	imageURL = upgradePeapixResolution(imageURL)

	// Download the image with 30s timeout and browser User-Agent.
	imageData := downloadImage(imageURL, peapixUA, 30*time.Second)
	if imageData == nil || !isJPEG(imageData) {
		if data, err := os.ReadFile(cacheFile); err == nil {
			serveWallpaperImage(w, data, &expires)
			return
		}
		WriteJSON(w, http.StatusNotFound, map[string]interface{}{"error": "wallpaper not available"})
		return
	}

	// Save to cache.
	saveCacheFile(cacheFile, imageData)

	// Serve the fresh image.
	serveWallpaperImage(w, imageData, &expires)
}

// HandleWallhavenWallpaper proxies a random top-listed wallpaper from wallhaven.cc.
// Cache TTL: 1 hour. Falls back to cached file on error.
func (p *Proxy) HandleWallhavenWallpaper(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		WriteOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}
	cacheFile := ".cache/wallpaper-haven.jpg"

	// Fast path: check cache WITHOUT lock (read-only stat + read).
	// If file exists and is < 1 hour old, serve it immediately.
	if info, err := os.Stat(cacheFile); err == nil {
		if time.Since(info.ModTime()) < time.Hour {
			if data, err := os.ReadFile(cacheFile); err == nil {
				serveWallpaperImage(w, data, nil) // no Expires for wallhaven (GAP 5)
				return
			}
		}
	}

	// Cache miss or expired — acquire lock.
	p.wallpaperMu.Lock()
	defer p.wallpaperMu.Unlock()

	// Double-check after acquiring lock.
	if info, err := os.Stat(cacheFile); err == nil {
		if time.Since(info.ModTime()) < time.Hour {
			if data, err := os.ReadFile(cacheFile); err == nil {
				serveWallpaperImage(w, data, nil)
				return
			}
		}
	}

	// Fetch from wallhaven.cc API — filter for >=1440p wallpapers
	wallhavenURL := "https://wallhaven.cc/api/v1/search?categories=100&purity=100&topRange=1M&sorting=toplist&order=desc&page=3&atleast=2560x1440"
	wallhavenUA := "umans-proxy/1.0"
	browserUA := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

	// Helper closure: fallback to stale cache, else 404.
	fallback := func() {
		if data, err := os.ReadFile(cacheFile); err == nil {
			serveWallpaperImage(w, data, nil)
			return
		}
		WriteJSON(w, http.StatusNotFound, map[string]interface{}{"error": "wallpaper not available"})
	}

	apiClient := &http.Client{Timeout: 15 * time.Second}
	apiReq, err := http.NewRequest("GET", wallhavenURL, nil)
	if err != nil {
		fallback()
		return
	}
	apiReq.Header.Set("User-Agent", wallhavenUA)

	apiResp, err := apiClient.Do(apiReq)
	if err != nil {
		fallback()
		return
	}
	defer apiResp.Body.Close()

	if apiResp.StatusCode != http.StatusOK {
		fallback()
		return
	}

	body, err := io.ReadAll(apiResp.Body)
	if err != nil {
		fallback()
		return
	}

	// Parse JSON: expect { "data": [ { "path": "https://..." }, ... ] }
	var result struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		fallback()
		return
	}
	if len(result.Data) == 0 {
		fallback()
		return
	}

	// Pick a random entry from the data array.
	pick := result.Data[rand.Intn(len(result.Data))]
	imageURL, ok := pick["path"].(string)
	if !ok || imageURL == "" {
		fallback()
		return
	}

	// Download the image with 30s timeout and browser User-Agent.
	imageData := downloadImage(imageURL, browserUA, 30*time.Second)
	if imageData == nil || !isValidImage(imageData) {
		fallback()
		return
	}

	// Save to cache.
	saveCacheFile(cacheFile, imageData)

	// Serve the fresh image.
	serveWallpaperImageTyped(w, imageData, nil, imageContentType(imageData))
}

// PerformVisionHandoff replaces images with text descriptions from the handoff
// model (SPEC §11.5). It accepts optional pre-collected image parts via variadic
// to avoid re-scanning the payload when parts have already been collected.
