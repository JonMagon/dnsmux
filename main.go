// dnsmux is a tiny UDP DNS multiplexer: it listens on a single :53 socket,
// looks at the QNAME suffix of each incoming query, and forwards the raw
// packet to one of several backend DNS servers based on a configured
// suffix-to-backend table. The backend's response is sent back to the
// original client unchanged.
package main

import (
	"flag"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

type route struct {
	suffix  string
	backend *net.UDPAddr
}

type routeTable struct {
	mu sync.RWMutex
	rs []route
}

func (r *routeTable) lookup(qname string) (*net.UDPAddr, bool) {
	q := strings.ToLower(qname)
	if !strings.HasSuffix(q, ".") {
		q += "."
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, rt := range r.rs {
		if strings.HasSuffix(q, rt.suffix) {
			return rt.backend, true
		}
	}
	return nil, false
}

// extractQName decodes the first question section's QNAME from a raw DNS
// packet. Returns the name (lowercase, with trailing dot) and true on
// success. Question-section labels are uncompressed by spec, so we don't
// follow compression pointers here.
func extractQName(pkt []byte) (string, bool) {
	if len(pkt) < 12 {
		return "", false
	}
	qdcount := uint16(pkt[4])<<8 | uint16(pkt[5])
	if qdcount == 0 {
		return "", false
	}
	off := 12
	var sb strings.Builder
	for off < len(pkt) {
		l := int(pkt[off])
		off++
		if l == 0 {
			return strings.ToLower(sb.String()), true
		}
		if l&0xC0 != 0 || off+l > len(pkt) {
			return "", false
		}
		sb.Write(pkt[off : off+l])
		sb.WriteByte('.')
		off += l
	}
	return "", false
}

// refusedResponse builds a minimal DNS response with RCODE=REFUSED echoing
// the query header + question section. Returned to clients whose QNAME
// matches no configured route.
func refusedResponse(query []byte) []byte {
	if len(query) < 12 {
		return nil
	}
	resp := make([]byte, len(query))
	copy(resp, query)
	resp[2] |= 0x80                    // QR=1
	resp[3] = (resp[3] &^ 0x0F) | 0x05 // RCODE=REFUSED
	resp[6], resp[7] = 0, 0            // ANCOUNT=0
	resp[8], resp[9] = 0, 0            // NSCOUNT=0
	resp[10], resp[11] = 0, 0          // ARCOUNT=0
	return resp
}

type arrayFlags []string

func (a *arrayFlags) String() string     { return strings.Join(*a, ",") }
func (a *arrayFlags) Set(v string) error { *a = append(*a, v); return nil }

func main() {
	listenAddr := flag.String("listen", "0.0.0.0:53", "UDP address to listen on")
	timeout := flag.Duration("timeout", 2*time.Second, "backend response timeout")
	verbose := flag.Bool("v", false, "log every query and routing decision")
	var routeFlags arrayFlags
	flag.Var(&routeFlags, "route", "route as suffix=host:port; may be repeated")
	flag.Parse()

	if len(routeFlags) == 0 {
		log.Fatal("at least one -route is required, e.g. -route t.example.com=127.0.0.1:5301")
	}

	rt := &routeTable{}
	for _, rf := range routeFlags {
		eq := strings.IndexByte(rf, '=')
		if eq < 0 {
			log.Fatalf("bad route %q (expected suffix=host:port)", rf)
		}
		suffix := strings.ToLower(strings.TrimSpace(rf[:eq]))
		backend := strings.TrimSpace(rf[eq+1:])
		if !strings.HasSuffix(suffix, ".") {
			suffix += "."
		}
		ua, err := net.ResolveUDPAddr("udp", backend)
		if err != nil {
			log.Fatalf("bad backend %q: %v", backend, err)
		}
		rt.rs = append(rt.rs, route{suffix: suffix, backend: ua})
	}
	// Longest suffix wins, so a more specific zone overrides a less specific
	// parent if both are configured.
	sort.Slice(rt.rs, func(i, j int) bool {
		return len(rt.rs[i].suffix) > len(rt.rs[j].suffix)
	})

	laddr, err := net.ResolveUDPAddr("udp", *listenAddr)
	if err != nil {
		log.Fatalf("bad listen addr %q: %v", *listenAddr, err)
	}
	pc, err := net.ListenUDP("udp", laddr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer pc.Close()
	log.Printf("dnsmux listening on %s", pc.LocalAddr())
	for _, r := range rt.rs {
		log.Printf("  route %s -> %s", r.suffix, r.backend)
	}

	buf := make([]byte, 4096)
	for {
		n, raddr, err := pc.ReadFromUDP(buf)
		if err != nil {
			log.Printf("read: %v", err)
			continue
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		go handle(pc, raddr, pkt, rt, *timeout, *verbose)
	}
}

func handle(pc *net.UDPConn, raddr *net.UDPAddr, query []byte, rt *routeTable, to time.Duration, verbose bool) {
	qname, ok := extractQName(query)
	if !ok {
		if verbose {
			log.Printf("drop malformed query from %s (%d bytes)", raddr, len(query))
		}
		return
	}
	backend, ok := rt.lookup(qname)
	if !ok {
		if verbose {
			log.Printf("REFUSED %s from %s (no matching route)", qname, raddr)
		}
		if r := refusedResponse(query); r != nil {
			pc.WriteToUDP(r, raddr)
		}
		return
	}
	if verbose {
		log.Printf("%s from %s -> %s", qname, raddr, backend)
	}
	conn, err := net.DialUDP("udp", nil, backend)
	if err != nil {
		log.Printf("dial %v: %v", backend, err)
		return
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(to))
	if _, err := conn.Write(query); err != nil {
		if verbose {
			log.Printf("backend write %v: %v", backend, err)
		}
		return
	}
	resp := make([]byte, 4096)
	n, err := conn.Read(resp)
	if err != nil {
		if verbose {
			log.Printf("backend read %v: %v", backend, err)
		}
		return
	}
	pc.WriteToUDP(resp[:n], raddr)
}
