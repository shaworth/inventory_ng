package main

import (
	_ "embed"
	"html/template"
	"log"
	"net"
	"net/http"
	"strconv"

	qrcode "github.com/skip2/go-qrcode"
)

// qr.go serves a QR code encoding this server's own URL, so a phone camera can
// open the Network Hosts page without typing an IP or relying on mDNS.

//go:embed qr.html
var qrHTML string

var qrTmpl = template.Must(template.New("qr").Parse(qrHTML))

// qrURL is what the QR encodes: http://<this host's LAN IP>:<httpPort>/.
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
	if err := qrTmpl.Execute(w, struct{ URL string }{qrURL()}); err != nil {
		log.Printf("qr template render failed: %v", err)
	}
}

func handleQRImage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/qr.png" {
		http.NotFound(w, r)
		return
	}
	png, err := qrcode.Encode(qrURL(), qrcode.Medium, 512)
	if err != nil {
		http.Error(w, "qr encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(png)
}
