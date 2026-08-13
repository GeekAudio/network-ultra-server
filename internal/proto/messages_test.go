package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWelcomeOmitsUDPContractWhenDisabled(t *testing.T) {
	b, err := json.Marshal(WelcomeData{PeerID: "peer", ServerVersion: "1.4.0"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "udpEndpoint") || strings.Contains(s, "udpToken") {
		t.Fatalf("disabled UDP leaked an advertised capability: %s", s)
	}
}

func TestWelcomeIncludesEndpointAndTokenTogetherWhenEnabled(t *testing.T) {
	b, err := json.Marshal(WelcomeData{
		PeerID: "peer", ServerVersion: "1.4.0",
		UdpEndpoint: "audio.example:18902", UdpToken: "opaque",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"udpEndpoint":"audio.example:18902"`) || !strings.Contains(s, `"udpToken":"opaque"`) {
		t.Fatalf("enabled UDP contract incomplete: %s", s)
	}
}
