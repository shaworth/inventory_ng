package main

import (
	"bytes"
	_ "embed"
	"html/template"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"net"
	"net/http"
	"strconv"

	qrcode "github.com/skip2/go-qrcode"
)

// qr.go serves a QR code encoding the Services Finder page, so a phone camera
// can open it without typing an IP or relying on mDNS.

//go:embed qr.html
var qrHTML string

var qrTmpl = template.Must(template.New("qr").Parse(qrHTML))

// qrURL is what the QR encodes: the Services Finder page on this host.
func qrURL() string {
	host := "localhost"
	if ip := lanIPv4(); ip != nil {
		host = ip.String()
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(httpPort)) + "/"
}

func handleQRPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/qr" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := qrTmpl.Execute(w, struct{ URL string }{qrURL()}); err != nil {
		log.Printf("qr template render failed: %v", err)
	}
}

func handleQRImage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/qr.png" {
		http.NotFound(w, r)
		return
	}
	data, err := qrcode.Encode(qrURL(), qrcode.Medium, 512)
	if err != nil {
		http.Error(w, "qr encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

// ---- web-app manifest / icon / service worker (home-screen install) ----

// manifestJSON makes /qr installable so it can be pinned to the home screen as
// an icon that opens the QR full-screen.
const manifestJSON = `{
  "name": "Vessel QR",
  "short_name": "QR",
  "start_url": "/qr",
  "scope": "/",
  "display": "standalone",
  "background_color": "#141923",
  "theme_color": "#00adb5",
  "icons": [
    { "src": "/icon-192.png", "sizes": "192x192", "type": "image/png" },
    { "src": "/icon-512.png", "sizes": "512x512", "type": "image/png" }
  ]
}`

// serviceWorkerJS is a no-op worker; Chrome requires one for "Install app".
const serviceWorkerJS = `self.addEventListener('install', function (e) { self.skipWaiting(); });
self.addEventListener('activate', function (e) { e.waitUntil(self.clients.claim()); });
self.addEventListener('fetch', function () {});
`

func handleManifest(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/manifest.webmanifest" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Write([]byte(manifestJSON))
}

func handleServiceWorker(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/sw.js" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(serviceWorkerJS))
}

func handleIcon(w http.ResponseWriter, r *http.Request) {
	size := 0
	switch r.URL.Path {
	case "/icon-192.png":
		size = 192
	case "/icon-512.png":
		size = 512
	default:
		http.NotFound(w, r)
		return
	}
	data, err := iconPNG(size)
	if err != nil {
		http.Error(w, "icon encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(data)
}

// iconPNG draws a simple QR-style app icon (teal field, white finder squares)
// so the home-screen shortcut looks intentional.
func iconPNG(size int) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	teal := color.RGBA{0x00, 0xad, 0xb5, 0xff}
	white := color.RGBA{0xff, 0xff, 0xff, 0xff}

	draw.Draw(img, img.Bounds(), &image.Uniform{teal}, image.Point{}, draw.Src)

	fill := func(x, y, w, h int, c color.RGBA) {
		draw.Draw(img, image.Rect(x, y, x+w, y+h), &image.Uniform{c}, image.Point{}, draw.Src)
	}

	margin := size / 8
	w := (size - 2*margin) * 2 / 5
	t := w / 7
	if t < 1 {
		t = 1
	}
	finder := func(x, y int) {
		fill(x, y, w, w, white)
		fill(x+t, y+t, w-2*t, w-2*t, teal)
		fill(x+2*t, y+2*t, w-4*t, w-4*t, white)
	}
	finder(margin, margin)
	finder(size-margin-w, margin)
	finder(margin, size-margin-w)

	// a few data modules, lower-right
	u := t
	fill(size-margin-2*u, size-margin-2*u, u, u, white)
	fill(size-margin-4*u, size-margin-u, u, u, white)
	fill(size-margin-u, size-margin-4*u, u, u, white)

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
