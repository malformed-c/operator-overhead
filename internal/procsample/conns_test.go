package procsample

import (
	"net/netip"
	"testing"
)

// The two encodings share one field and differ in endianness. Reading both the
// same way produces a plausible, wrong address — `10.96.0.1` as `1.0.96.10` —
// and the comparison then counts zero connections, which reads as "this arm
// holds nothing open" rather than as a parse bug.
func TestParseProcAddrEndianness(t *testing.T) {
	cases := map[string]string{
		// Verbatim from a crrelay worker's /proc/<pid>/net/tcp on engix99.
		"0100600A:01BB": "10.96.0.1:443",
		"AA98000A:A414": "10.0.152.170:42004",
		"0100007F:1F90": "127.0.0.1:8080",
	}
	for raw, want := range cases {
		got, err := parseProcAddr(raw)
		if err != nil {
			t.Errorf("parseProcAddr(%q): %v", raw, err)

			continue
		}
		if got.String() != want {
			t.Errorf("parseProcAddr(%q) = %s, want %s", raw, got, want)
		}
	}
}

func TestParseProcAddrSkipsIPv6(t *testing.T) {
	// A 32-character row is IPv6. Half-decoding it would produce an address that
	// compares unequal for the wrong reason.
	if _, err := parseProcAddr("00000000000000000000000001000000:0050"); err == nil {
		t.Error("an IPv6 row parsed as IPv4")
	}
	if _, err := parseProcAddr("nonsense"); err == nil {
		t.Error("a malformed field parsed")
	}
}

func TestAPIServerServiceIsTheClusterIP(t *testing.T) {
	// Pinned because the socket's remote end carries the SERVICE address, not a
	// node address: cilium translates below the socket. A benchmark pointed at a
	// node IP would count zero connections on a healthy arm.
	if want := netip.MustParseAddrPort("10.96.0.1:443"); APIServerService != want {
		t.Errorf("APIServerService = %s, want %s", APIServerService, want)
	}
}
