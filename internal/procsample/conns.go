package procsample

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ═══════════════════════════════════════════════════════════════════════════
// ***THIS FILE EXISTS BECAUSE `rest_client_requests_total` CANNOT SEE A WATCH,
// AND THE FIRST DRAFT OF THIS BENCHMARK CLAIMED IT COULD.***
//
// The counter is labelled by HTTP METHOD, not by Kubernetes verb. A LIST and a
// WATCH are both `GET`, and a WATCH is a GET that does not complete — so it does
// not increment the counter while it is held. Measured at N=1 after a 45-second
// window: `GET=3 PATCH=74`, with the informer's watch open the whole time and
// contributing nothing to either number.
//
// So the request counter answers "how much work did this arm ask the apiserver
// to do", and it cannot answer "what is this arm holding open". The second
// question is the operator tax — N managers means N caches, and a cache is a
// connection and a watch-cache subscriber, not a request rate. A socket is the
// thing that is actually held, so a socket is what this counts.
//
// Measured on engix99, one controller-runtime manager, `/proc/<pid>/net/tcp`:
//
//	AA98000A:A414 0100600A:01BB 01     10.0.152.170:42004 -> 10.96.0.1:443 ESTABLISHED
//
// ONE connection, not several: client-go speaks HTTP/2, so the list, the watch
// and every patch are multiplexed over a single socket. That is a real finding
// and it is the opposite of what "one watch per informer" suggests — the cost
// per instance is one connection and one server-side watcher, not one per
// informer.
// ═══════════════════════════════════════════════════════════════════════════

// tcpEstablished is the state byte /proc/net/tcp uses for ESTABLISHED.
const tcpEstablished = "01"

// APIServerService is the apiserver's in-cluster ClusterIP, as a POD in the
// cluster network sees it: cilium's translation happens below the socket, so a
// pod's own socket table names the Service.
var APIServerService = netip.MustParseAddrPort("10.96.0.1:443")

// ═══════════════════════════════════════════════════════════════════════════
// ***NOT EVERY CLIENT REACHES THE APISERVER BY THE ClusterIP, AND MATCHING ONLY
// THAT ADDRESS REPORTS A PROCESS HOLDING A WATCH AS HOLDING NOTHING.***
//
// Measured: radiant's only established socket is
//
//	remote=0232A8C0:192B   ->   192.168.50.2:6443
//
// the apiserver's REAL endpoint, not 10.96.0.1:443. With a single-address match
// the shared-host column printed `0 conn` for the one process in arm B that
// certainly holds watches — a flattering zero, for the arm this benchmark's
// author would prefer to win, produced by the instrument rather than by the
// system. Exactly the failure the rest of this package exists to prevent.
//
// So the match is a SET, built from the ClusterIP plus whatever the kubeconfig
// names, and a caller that cannot enumerate the endpoints gets an error rather
// than a zero.
// ═══════════════════════════════════════════════════════════════════════════

// CountConnsAnyOf sums a process's ESTABLISHED connections to ANY of want.
func (s Set) CountConnsAnyOf(want []netip.AddrPort) (total int, complete bool) {
	complete = true
	for _, p := range s.Samples {
		n, err := connsToAny(p.PID, want)
		if err != nil {
			complete = false

			continue
		}
		total += n
	}

	return total, complete
}

func connsToAny(pid int, want []netip.AddrPort) (int, error) {
	n := 0
	for _, w := range want {
		c, err := ConnsTo(pid, w)
		if err != nil {
			return 0, err
		}
		n += c
	}

	return n, nil
}

// ConnsTo counts a process's ESTABLISHED TCP connections to one address.
//
// READ FROM THE PROCESS'S OWN /proc/<pid>/net/tcp, which is its NETWORK
// NAMESPACE's socket table — so a pod's connections are visible from the host
// without entering the namespace, and a connection is attributed to the process
// that holds it rather than to a node-wide total nobody can divide.
func ConnsTo(pid int, want netip.AddrPort) (int, error) {
	f, err := os.Open(filepath.Join("/proc", strconv.Itoa(pid), "net", "tcp"))
	if err != nil {
		return 0, fmt.Errorf("procsample: socket table for %d: %w", pid, err)
	}
	defer f.Close()

	n := 0
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || fields[3] != tcpEstablished {
			continue
		}
		remote, err := parseProcAddr(fields[2])
		if err != nil {
			continue
		}
		if remote == want {
			n++
		}
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("procsample: read socket table for %d: %w", pid, err)
	}

	return n, nil
}

// parseProcAddr decodes /proc/net/tcp's `AA98000A:A414` form.
//
// ***THE ADDRESS IS LITTLE-ENDIAN HEX AND THE PORT IS BIG-ENDIAN HEX, IN THE
// SAME FIELD.*** The kernel prints the in-memory u32 of an IPv4 address, which
// on x86 is byte-reversed, and prints the port as a plain number. Reading both
// the same way yields a plausible address that is wrong — `10.96.0.1` comes out
// as `1.0.96.10` — and a comparison against it silently counts zero
// connections, which reads as "this arm holds nothing open".
func parseProcAddr(s string) (netip.AddrPort, error) {
	host, portHex, ok := strings.Cut(s, ":")
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("procsample: %q is not addr:port", s)
	}
	raw, err := hex.DecodeString(host)
	if err != nil || len(raw) != 4 {
		// IPv6 rows are 32 hex characters. Skipped rather than mis-parsed: this
		// cluster's Service network is IPv4 and a half-decoded v6 address would
		// compare unequal for the wrong reason.
		return netip.AddrPort{}, fmt.Errorf("procsample: %q is not an IPv4 address", host)
	}
	addr := netip.AddrFrom4([4]byte(binary.LittleEndian.AppendUint32(nil, binary.BigEndian.Uint32(raw))))

	port, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("procsample: port %q: %w", portHex, err)
	}

	return netip.AddrPortFrom(addr, uint16(port)), nil
}

// CountConns sums ConnsTo across a sampled set.
//
// A process whose socket table cannot be read contributes nothing AND sets
// complete to false, for the same reason PSS does: a total that silently omits
// a process is a smaller number, not a measurement.
func (s Set) CountConns(want netip.AddrPort) (total int, complete bool) {
	complete = true
	for _, p := range s.Samples {
		n, err := ConnsTo(p.PID, want)
		if err != nil {
			complete = false

			continue
		}
		total += n
	}

	return total, complete
}
