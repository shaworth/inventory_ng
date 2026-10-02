package main

import (
	_ "embed"
	"encoding/binary"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// hosts.go implements a small, dependency-free mDNS / DNS-SD browser that backs
// the "/" landing page: it lists the hosts found on the local network, their
// addresses, the services they advertise and, where possible, a link to them.
//
// Android note: Termux cannot receive multicast (no WifiManager.MulticastLock),
// so queries are sent as *legacy unicast* queries (source port != 5353). Per
// RFC 6762 section 5.4 responders answer those with a unicast reply, which
// Android delivers normally. This works against Avahi (Venus OS / Cerbo GX)
// and mDNSResponder alike; multicast queries would be silently dropped.

const (
	mdnsGroup = "224.0.0.251"
	mdnsPort  = 5353

	dnsTypeA    = 1
	dnsTypePTR  = 12
	dnsTypeTXT  = 16
	dnsTypeAAAA = 28
	dnsTypeSRV  = 33

	dnsClassIN = 1
	dnsClassQU = 1 << 15 // request a unicast response

	hostsCacheTTL = 30 * time.Second
)

//go:embed hosts.html
var hostsHTML string

var hostsTmpl = template.Must(template.New("hosts").Parse(hostsHTML))

//go:embed hosts_detail.html
var hostsDetailHTML string

var hostsDetailTmpl = template.Must(template.New("hosts_detail").Parse(hostsDetailHTML))

// Service is one advertised DNS-SD service instance (e.g. Victron._http._tcp).
type Service struct {
	Type     string   // _http._tcp
	Instance string   // Victron
	Host     string   // SRV target host, e.g. venus.local
	Port     uint16   // SRV port (0 = use the service default)
	Txt      []string // raw "key=value" TXT strings
	Addrs    []string // addresses of the host providing the service
	URL      string   // link, when a scheme can be inferred
	Target   string   // slug used as the <a target> window name

	srcIP string // responder address (fallback when no SRV/A is present)
}

// Link is a named interface URL (e.g. a web GUI or admin console) that we can
// point at a host but that is not itself advertised over mDNS.
type Link struct {
	Label  string
	URL    string
	Target string // slug used as the <a target> window name
}

// Host groups a discovered host and the services it advertises.
type Host struct {
	Name     string
	Addrs    []string
	Services []Service
	Links    []Link
}

// AddrList renders addresses for the template.
func (h Host) AddrList() string { return strings.Join(h.Addrs, ", ") }

// Slug is a URL-safe identifier used for the host's detail page.
func (h Host) Slug() string { return slug(h.Name) }

// TxtList renders the de-duplicated raw TXT records for the template. Repeated
// probes can surface the same record twice, so collapse duplicates.
func (s Service) TxtList() string {
	seen := make(map[string]bool, len(s.Txt))
	out := make([]string, 0, len(s.Txt))
	for _, t := range s.Txt {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return strings.Join(out, "  ")
}

// ---- DNS wire format ----

type question struct {
	name  string
	qtype uint16
}

type dnsRR struct {
	name   string
	rtype  uint16
	domain string   // PTR / SRV target
	port   uint16   // SRV
	ips    []string // A / AAAA
	txt    []string // TXT
}

func encodeName(name string) []byte {
	name = strings.TrimSuffix(name, ".")
	var b []byte
	if name != "" {
		for _, label := range strings.Split(name, ".") {
			if label == "" {
				continue
			}
			b = append(b, byte(len(label)))
			b = append(b, label...)
		}
	}
	return append(b, 0)
}

func buildQuery(qs []question) []byte {
	buf := make([]byte, 12)
	binary.BigEndian.PutUint16(buf[4:6], uint16(len(qs))) // QDCOUNT
	for _, q := range qs {
		buf = append(buf, encodeName(q.name)...)
		var tail [4]byte
		binary.BigEndian.PutUint16(tail[0:2], q.qtype)
		binary.BigEndian.PutUint16(tail[2:4], dnsClassIN|dnsClassQU)
		buf = append(buf, tail[:]...)
	}
	return buf
}

// parseName decodes a (possibly compressed) DNS name at off, returning the
// decoded name and the offset just past it.
func parseName(msg []byte, off int) (string, int, error) {
	var labels []string
	next := -1
	for {
		if off < 0 || off >= len(msg) {
			return "", 0, fmt.Errorf("name out of range")
		}
		l := int(msg[off])
		if l == 0 {
			off++
			if next == -1 {
				next = off
			}
			break
		}
		if l&0xC0 == 0xC0 {
			if off+1 >= len(msg) {
				return "", 0, fmt.Errorf("truncated pointer")
			}
			if next == -1 {
				next = off + 2
			}
			off = ((l & 0x3F) << 8) | int(msg[off+1])
			continue
		}
		if off+1+l > len(msg) {
			return "", 0, fmt.Errorf("label out of range")
		}
		labels = append(labels, string(msg[off+1:off+1+l]))
		off += 1 + l
	}
	return strings.Join(labels, "."), next, nil
}

func parseMessage(msg []byte) []dnsRR {
	if len(msg) < 12 {
		return nil
	}
	qd := int(binary.BigEndian.Uint16(msg[4:6]))
	an := int(binary.BigEndian.Uint16(msg[6:8]))
	ns := int(binary.BigEndian.Uint16(msg[8:10]))
	ar := int(binary.BigEndian.Uint16(msg[10:12]))

	off := 12
	for i := 0; i < qd; i++ {
		_, next, err := parseName(msg, off)
		if err != nil {
			return nil
		}
		off = next + 4
	}

	var rrs []dnsRR
	for i := 0; i < an+ns+ar; i++ {
		name, next, err := parseName(msg, off)
		if err != nil {
			break
		}
		off = next
		if off+10 > len(msg) {
			break
		}
		rtype := binary.BigEndian.Uint16(msg[off : off+2])
		rdlen := int(binary.BigEndian.Uint16(msg[off+8 : off+10]))
		off += 10
		if off+rdlen > len(msg) {
			break
		}
		rd := msg[off : off+rdlen]
		rr := dnsRR{name: name, rtype: rtype}
		switch rtype {
		case dnsTypePTR:
			if d, _, err := parseName(msg, off); err == nil {
				rr.domain = d
			}
		case dnsTypeSRV:
			if rdlen >= 6 {
				rr.port = binary.BigEndian.Uint16(rd[4:6])
				if d, _, err := parseName(msg, off+6); err == nil {
					rr.domain = d
				}
			}
		case dnsTypeTXT:
			for j := 0; j < len(rd); {
				ln := int(rd[j])
				j++
				if j+ln > len(rd) {
					break
				}
				rr.txt = append(rr.txt, string(rd[j:j+ln]))
				j += ln
			}
		case dnsTypeA:
			if rdlen == 4 {
				rr.ips = append(rr.ips, net.IP(rd).String())
			}
		case dnsTypeAAAA:
			if rdlen == 16 {
				rr.ips = append(rr.ips, net.IP(rd).String())
			}
		}
		off += rdlen
		rrs = append(rrs, rr)
	}
	return rrs
}

// ---- discovery ----

// fallbackServiceTypes are probed in addition to whatever the network
// advertises via _services._dns-sd._udp, so common devices are found even if
// they do not answer the service-type enumeration meta-query.
var fallbackServiceTypes = []string{
	"_http._tcp", "_https._tcp", "_ssh._tcp", "_sftp-ssh._tcp",
	"_smb._tcp", "_workstation._tcp", "_ipp._tcp", "_printer._tcp",
	"_airplay._tcp", "_raop._tcp", "_googlecast._tcp", "_garmin-mrn-html._tcp",
}

// ensureInstance returns (creating if needed) the service keyed by its instance
// FQDN. The instance and type are recovered from the FQDN itself
// ("<Instance>.<_type>.<_tcp>.local"), so a missing PTR still yields a type.
func ensureInstance(m map[string]*Service, fqdn string) *Service {
	trimmed := strings.TrimSuffix(fqdn, ".")
	key := strings.ToLower(trimmed)
	s := m[key]
	if s == nil {
		s = &Service{Instance: trimmed}
		labels := strings.Split(trimmed, ".")
		if len(labels) > 0 && labels[0] != "" {
			s.Instance = labels[0]
		}
		if len(labels) >= 3 && (strings.EqualFold(labels[2], "_tcp") || strings.EqualFold(labels[2], "_udp")) {
			s.Type = labels[1] + "." + labels[2]
		}
		m[key] = s
	}
	return s
}

func discoverHosts() ([]Host, time.Duration, error) {
	start := time.Now()

	// Ephemeral source port => a "legacy unicast query" => unicast replies,
	// which survive Android's multicast filter.
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 0})
	if err != nil {
		return nil, 0, err
	}
	defer conn.Close()

	dst := &net.UDPAddr{IP: net.ParseIP(mdnsGroup), Port: mdnsPort}

	serviceTypes := map[string]bool{}         // "...local" service type -> seen
	instances := map[string]*Service{}        // instance fqdn -> service
	hostAddrs := map[string]map[string]bool{} // host fqdn -> set of addresses

	collect := func(msg []byte, srcIP string) {
		for _, rr := range parseMessage(msg) {
			switch rr.rtype {
			case dnsTypePTR:
				base := strings.ToLower(strings.TrimSuffix(rr.name, "."))
				target := strings.TrimSuffix(rr.domain, ".")
				if base == "_services._dns-sd._udp.local" {
					if target != "" {
						serviceTypes[strings.ToLower(target)] = true
					}
				} else if strings.HasPrefix(base, "_") {
					s := ensureInstance(instances, target)
					if s.srcIP == "" {
						s.srcIP = srcIP
					}
				}
			case dnsTypeSRV:
				s := ensureInstance(instances, rr.name)
				if rr.domain != "" {
					s.Host = strings.ToLower(strings.TrimSuffix(rr.domain, "."))
				}
				s.Port = rr.port
				if s.srcIP == "" {
					s.srcIP = srcIP
				}
			case dnsTypeTXT:
				s := ensureInstance(instances, rr.name)
				s.Txt = append(s.Txt, rr.txt...)
			case dnsTypeA, dnsTypeAAAA:
				key := strings.ToLower(strings.TrimSuffix(rr.name, "."))
				if hostAddrs[key] == nil {
					hostAddrs[key] = map[string]bool{}
				}
				for _, ip := range rr.ips {
					hostAddrs[key][ip] = true
				}
			}
		}
	}

	// Round 1: enumerate the service types present on the network.
	sendQuery(conn, dst, buildQuery([]question{{"_services._dns-sd._udp.local", dnsTypePTR}}))
	readFor(conn, 1200*time.Millisecond, collect)

	// Round 2: enumerate instances of every known + common service type.
	var qs []question
	for t := range serviceTypes {
		qs = append(qs, question{t, dnsTypePTR})
	}
	for _, t := range fallbackServiceTypes {
		fq := t + ".local"
		if !serviceTypes[fq] {
			qs = append(qs, question{fq, dnsTypePTR})
		}
	}
	if len(qs) > 0 {
		sendQuery(conn, dst, buildQuery(qs))
		readFor(conn, 2200*time.Millisecond, collect)
	}

	// Round 3: resolve SRV target hosts that still have no address.
	var aqs []question
	for _, s := range instances {
		if s.Host != "" && hostAddrs[s.Host] == nil {
			aqs = append(aqs, question{s.Host, dnsTypeA})
		}
	}
	if len(aqs) > 0 {
		sendQuery(conn, dst, buildQuery(aqs))
		readFor(conn, 1000*time.Millisecond, collect)
	}

	return assembleHosts(instances, hostAddrs), time.Since(start), nil
}

// sendQuery writes the packet twice for reliability (a single multicast datagram
// can be lost, and there is no acknowledgement to trigger a retry).
func sendQuery(conn *net.UDPConn, dst *net.UDPAddr, pkt []byte) {
	conn.WriteToUDP(pkt, dst)
	time.Sleep(120 * time.Millisecond)
	conn.WriteToUDP(pkt, dst)
}

func readFor(conn *net.UDPConn, d time.Duration, fn func([]byte, string)) {
	end := time.Now().Add(d)
	buf := make([]byte, 9000)
	for {
		conn.SetReadDeadline(end)
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		fn(buf[:n], src.IP.String())
	}
}

func assembleHosts(instances map[string]*Service, hostAddrs map[string]map[string]bool) []Host {
	hostMap := map[string]*Host{}
	ensure := func(name string) *Host {
		name = strings.ToLower(strings.TrimSuffix(name, "."))
		h := hostMap[name]
		if h == nil {
			h = &Host{Name: name}
			hostMap[name] = h
		}
		return h
	}

	for name, set := range hostAddrs {
		ensure(name).Addrs = sortedKeys(set)
	}

	for _, s := range instances {
		hostName := s.Host
		if hostName == "" {
			hostName = s.srcIP
		}
		if hostName == "" {
			continue
		}
		h := ensure(hostName)
		if len(h.Addrs) == 0 {
			if set := hostAddrs[s.Host]; len(set) > 0 {
				h.Addrs = sortedKeys(set)
			} else if s.srcIP != "" {
				h.Addrs = []string{s.srcIP}
			}
		}
		h.Services = append(h.Services, *s)
	}

	hosts := make([]Host, 0, len(hostMap))
	for _, h := range hostMap {
		for j := range h.Services {
			h.Services[j].URL = serviceURL(&h.Services[j], h.Addrs)
			if h.Services[j].URL != "" {
				h.Services[j].Target = windowName(h.Name, serviceName(h.Services[j].Type))
			}
		}
		if isVictron(h.Services) {
			// The Cerbo's MQTT broker and the inventory topic are not
			// advertised over mDNS, so surface them as a synthetic entry.
			if !hasServiceType(h.Services, "_mqtt._tcp") {
				h.Services = append(h.Services, Service{
					Type:     "_mqtt._tcp",
					Instance: "MQTT",
					Port:     mdnsMQTTPort,
					Txt:      []string{"topics=" + scanTopic},
				})
			}
			if ip := firstIPv4(h.Addrs); ip != "" {
				h.Links = append(h.Links,
					Link{Label: "GUI v2", URL: "http://" + ip + "/gui-v2/"},
					Link{Label: "Node-RED", URL: "https://" + net.JoinHostPort(ip, "1881") + "/"},
					Link{Label: "Signal K", URL: "http://" + net.JoinHostPort(ip, "3000") + "/"},
				)
			}
		}
		for k := range h.Links {
			h.Links[k].Target = windowName(h.Name, slug(h.Links[k].Label))
		}
		sort.Slice(h.Services, func(a, b int) bool {
			if h.Services[a].Type != h.Services[b].Type {
				return h.Services[a].Type < h.Services[b].Type
			}
			return h.Services[a].Instance < h.Services[b].Instance
		})
		hosts = append(hosts, *h)
	}
	sort.Slice(hosts, func(a, b int) bool { return hosts[a].Name < hosts[b].Name })
	return hosts
}

// serviceURL builds a clickable link for services whose scheme is known.
func serviceURL(s *Service, addrs []string) string {
	var scheme string
	var defPort uint16
	switch s.Type {
	case "_http._tcp", "_garmin-mrn-html._tcp":
		scheme, defPort = "http", 80
	case "_https._tcp":
		scheme, defPort = "https", 443
	default:
		return ""
	}

	ip := firstIPv4(addrs)
	if ip == "" {
		return ""
	}

	port := s.Port
	if port == 0 {
		port = defPort
	}

	path := txtValue(s.Txt, "urlpath")
	if path == "" {
		path = txtValue(s.Txt, "path")
	}
	if path != "" && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	hostport := ip
	if port != defPort {
		hostport = net.JoinHostPort(ip, strconv.Itoa(int(port)))
	}
	return scheme + "://" + hostport + path
}

// slug converts a string into a lowercase, hyphen-separated token, e.g.
// "venus.local" -> "venus-local", "GUI v2" -> "gui-v2".
func slug(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	dash := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			dash = false
		case !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// serviceName extracts the DNS-SD service name from a type, e.g.
// "_http._tcp" -> "http", "_garmin-mrn-html._tcp" -> "garmin-mrn-html".
func serviceName(typ string) string {
	t := strings.TrimPrefix(strings.ToLower(typ), "_")
	if i := strings.IndexByte(t, '.'); i >= 0 {
		t = t[:i]
	}
	return slug(t)
}

// windowName builds the browser window name (<a target>) scoped to a host and
// service, so each host+service reuses one window and never spawns more than
// one.
func windowName(host, service string) string {
	h := slug(host)
	switch {
	case h != "" && service != "":
		return h + "-" + service
	case h != "":
		return h
	case service != "":
		return service
	default:
		return "service"
	}
}

func firstIPv4(addrs []string) string {
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && ip.To4() != nil {
			return a
		}
	}
	return ""
}

// isVictron reports whether the host looks like a Victron device (e.g. a Cerbo
// GX / Venus OS), detected via the "Victron" TXT flag or the instance name.
func isVictron(services []Service) bool {
	for _, s := range services {
		if strings.EqualFold(txtValue(s.Txt, "victron"), "true") {
			return true
		}
		if strings.EqualFold(s.Instance, "victron") {
			return true
		}
	}
	return false
}

// hasServiceType reports whether any service has the given DNS-SD type.
func hasServiceType(services []Service, typ string) bool {
	for _, s := range services {
		if strings.EqualFold(s.Type, typ) {
			return true
		}
	}
	return false
}

func txtValue(txt []string, key string) string {
	for _, kv := range txt {
		if i := strings.IndexByte(kv, '='); i > 0 && strings.EqualFold(kv[:i], key) {
			return kv[i+1:]
		}
	}
	return ""
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- HTTP handler ----

var (
	hostsMu  sync.Mutex
	hostsVal []Host
	hostsAt  time.Time
	hostsDur time.Duration
)

type hostsPageData struct {
	Hosts    []Host
	Duration string
	Cached   bool
}

func getHosts(force bool) ([]Host, time.Duration, bool) {
	hostsMu.Lock()
	defer hostsMu.Unlock()

	if !force && hostsVal != nil && time.Since(hostsAt) < hostsCacheTTL {
		return hostsVal, hostsDur, true
	}
	hosts, dur, err := discoverHosts()
	if err != nil {
		log.Printf("mdns discovery failed: %v", err)
		if hostsVal != nil {
			return hostsVal, hostsDur, true
		}
		return nil, 0, false
	}
	hostsVal, hostsAt, hostsDur = hosts, time.Now(), dur
	return hosts, dur, false
}

// handleHostDetail renders the drill-down page for a single host, keyed by the
// host's slug (e.g. /host/venus-local).
func handleHostDetail(w http.ResponseWriter, r *http.Request) {
	want := strings.Trim(strings.TrimPrefix(r.URL.Path, "/host/"), "/")
	if want == "" {
		http.NotFound(w, r)
		return
	}
	hosts, _, _ := getHosts(false)
	for i := range hosts {
		if hosts[i].Slug() == want {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			if err := hostsDetailTmpl.Execute(w, struct{ Host Host }{hosts[i]}); err != nil {
				log.Printf("host detail template render failed: %v", err)
			}
			return
		}
	}
	http.NotFound(w, r)
}

func handleHostsPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	hosts, dur, cached := getHosts(r.URL.Query().Has("refresh"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := hostsTmpl.Execute(w, hostsPageData{
		Hosts:    hosts,
		Duration: dur.Round(time.Millisecond).String(),
		Cached:   cached,
	}); err != nil {
		log.Printf("hosts template render failed: %v", err)
	}
}
