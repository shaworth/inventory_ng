package main

import (
	"encoding/binary"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

// advertise.go announces this server over mDNS so browsers can reach it by name
// (e.g. http://blackview.local:8080/).
//
// Android/Termux cannot RECEIVE multicast (no WifiManager.MulticastLock), so the
// phone cannot answer mDNS queries. It can, however, SEND multicast, and mDNS
// clients cache unsolicited announcements — so we periodically broadcast our
// records (A + _http._tcp PTR/SRV/TXT) and peers pick the name up. Verified to
// make <hostname>.local resolve on macOS.

const mdnsAnnounceInterval = 30 * time.Second

// Ports for the extra services we announce.
const (
	mdnsSSHPort  = 8022 // Termux sshd (Android has no :22)
	mdnsMQTTPort = 1883 // embedded MQTT broker
)

func startMDNSAdvertise(hostname string, httpPort int) *mdnsAdvertiser {
	a := &mdnsAdvertiser{
		host:     hostname + ".local",
		httpPort: uint16(httpPort),
		done:     make(chan struct{}),
	}
	go a.loop()
	return a
}

type mdnsAdvertiser struct {
	host     string
	httpPort uint16
	conn     *net.UDPConn
	dst      *net.UDPAddr
	done     chan struct{}
	once     sync.Once
}

// advertisedService is one DNS-SD service instance we announce.
type advertisedService struct {
	typ      string // "_http._tcp"
	instance string // "Vessel Inventory"
	port     uint16
	txt      []string
}

func (a *mdnsAdvertiser) services() []advertisedService {
	name := strings.TrimSuffix(a.host, ".local")
	return []advertisedService{
		{"_http._tcp", "Vessel Inventory", a.httpPort, []string{"path=/"}},
		{"_ssh._tcp", name, mdnsSSHPort, nil},
		{"_sftp-ssh._tcp", name, mdnsSSHPort, nil},
		{"_mqtt._tcp", "Vessel Inventory MQTT", mdnsMQTTPort, []string{"topics=" + scanTopic}},
		{"_workstation._tcp", name, 0, nil},
	}
}

func (a *mdnsAdvertiser) loop() {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 0})
	if err != nil {
		log.Printf("mdns advertise disabled: %v", err)
		return
	}
	defer conn.Close()
	a.conn = conn
	a.dst = &net.UDPAddr{IP: net.ParseIP(mdnsGroup), Port: mdnsPort}

	if ip := lanIPv4(); ip != nil {
		log.Printf("mdns: advertising %s as %s (%d services)", a.host, ip, len(a.services()))
	} else {
		log.Printf("mdns: no LAN IPv4 yet; %s not announced until one appears", a.host)
	}

	a.announce(false)
	ticker := time.NewTicker(mdnsAnnounceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.done:
			return
		case <-ticker.C:
			a.announce(false)
		}
	}
}

func (a *mdnsAdvertiser) announce(goodbye bool) {
	if a.conn == nil || a.dst == nil {
		return
	}
	if pkt, ok := a.packet(goodbye); ok {
		a.conn.WriteToUDP(pkt, a.dst)
	}
}

// shutdown announces a goodbye (TTL=0) so peers drop our records immediately.
func (a *mdnsAdvertiser) shutdown() {
	a.once.Do(func() {
		for i := 0; i < 2; i++ {
			a.announce(true)
			time.Sleep(50 * time.Millisecond)
		}
		close(a.done)
	})
}

// packet builds one mDNS response carrying all of our records. When goodbye is
// set every record's TTL is 0 (an mDNS "goodbye").
func (a *mdnsAdvertiser) packet(goodbye bool) ([]byte, bool) {
	ip := lanIPv4()
	if ip == nil {
		return nil, false // no LAN interface, nothing to announce
	}

	host := a.host // "blackview.local"
	svcs := a.services()

	ttl := func(v uint32) uint32 {
		if goodbye {
			return 0
		}
		return v
	}

	var answers, additional []byte
	for _, s := range svcs {
		// service-type enumeration: _services._dns-sd._udp.local -> _type._tcp.local
		answers = append(answers, encodeRR("_services._dns-sd._udp.local", dnsTypePTR, ttl(4500), encodeName(s.typ+".local"))...)

		inst := s.instance + "." + s.typ + ".local"
		answers = append(answers, encodeRR(s.typ+".local", dnsTypePTR, ttl(4500), encodeName(inst))...)

		srv := make([]byte, 6)
		binary.BigEndian.PutUint16(srv[4:6], s.port)
		srv = append(srv, encodeName(host)...)
		additional = append(additional, encodeRR(inst, dnsTypeSRV, ttl(120), srv)...)
		additional = append(additional, encodeRR(inst, dnsTypeTXT, ttl(4500), encodeTXT(s.txt...))...)
	}
	additional = append(additional, encodeRR(host, dnsTypeA, ttl(120), ip.To4())...)

	header := make([]byte, 12)
	binary.BigEndian.PutUint16(header[2:4], 0x8400)                  // QR=1, AA=1
	binary.BigEndian.PutUint16(header[6:8], uint16(len(svcs)*2))     // ANCOUNT
	binary.BigEndian.PutUint16(header[10:12], uint16(len(svcs)*2+1)) // ARCOUNT

	pkt := append(header, answers...)
	pkt = append(pkt, additional...)
	return pkt, true
}

func encodeRR(name string, rtype uint16, ttl uint32, rdata []byte) []byte {
	out := encodeName(name)
	var tail [10]byte
	binary.BigEndian.PutUint16(tail[0:2], rtype)
	binary.BigEndian.PutUint16(tail[2:4], dnsClassIN)
	binary.BigEndian.PutUint32(tail[4:8], ttl)
	binary.BigEndian.PutUint16(tail[8:10], uint16(len(rdata)))
	out = append(out, tail[:]...)
	return append(out, rdata...)
}

func encodeTXT(pairs ...string) []byte {
	var out []byte
	for _, s := range pairs {
		out = append(out, byte(len(s)))
		out = append(out, s...)
	}
	if len(out) == 0 {
		out = append(out, 0)
	}
	return out
}

// lanIPv4 returns the best guess at the LAN-facing IPv4 address, preferring a
// Wi-Fi / Ethernet interface over VPN tunnels or cellular.
func lanIPv4() net.IP {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var fallback net.IP
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		ip := ifaceIPv4(ifc)
		if ip == nil {
			continue
		}
		n := ifc.Name
		if strings.HasPrefix(n, "wlan") || strings.HasPrefix(n, "en") || strings.HasPrefix(n, "eth") {
			return ip
		}
		if fallback == nil {
			fallback = ip
		}
	}
	return fallback
}

func ifaceIPv4(ifc net.Interface) net.IP {
	addrs, err := ifc.Addrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			if ip4 := ipnet.IP.To4(); ip4 != nil && !ip4.IsLoopback() {
				return ip4
			}
		}
	}
	return nil
}
