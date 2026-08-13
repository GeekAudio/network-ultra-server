package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadText(t *testing.T, body string) error {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	return err
}

func TestDefaultIsLoopbackAndUDPDisabled(t *testing.T) {
	c := Default()
	if c.Server.Listen != "127.0.0.1:18900" || c.Server.UdpListen != "" {
		t.Fatalf("unsafe default: listen=%q udp=%q", c.Server.Listen, c.Server.UdpListen)
	}
}

func TestRejectsPublicPlaintextAndUnencryptedUDPByDefault(t *testing.T) {
	if err := loadText(t, "[server]\nlisten='0.0.0.0:18900'\n"); err == nil || !strings.Contains(err.Error(), "public plaintext") {
		t.Fatalf("expected public plaintext rejection, got %v", err)
	}
	if err := loadText(t, "[server]\nlisten='127.0.0.1:18900'\nudp_listen='0.0.0.0:18902'\n"); err == nil || !strings.Contains(err.Error(), "unencrypted") {
		t.Fatalf("expected UDP rejection, got %v", err)
	}
}

func TestRejectsPublicOrImplicitHealthListener(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:18901", ""} {
		err := loadText(t, "[server]\nlisten='127.0.0.1:18900'\nhealth_listen='"+listen+"'\n")
		if err == nil || !strings.Contains(err.Error(), "health_listen") {
			t.Fatalf("health_listen=%q should be rejected, got %v", listen, err)
		}
	}
}

func TestHealthURLCanonicalizesOnlyLoopback(t *testing.T) {
	for _, tc := range []struct {
		listen, want string
		wantErr      bool
	}{
		{"127.0.0.1:29001", "http://127.0.0.1:29001/healthz", false},
		{"127.42.0.9:29002", "http://127.42.0.9:29002/healthz", false},
		{"[::1]:29003", "http://[::1]:29003/healthz", false},
		{"localhost:29004", "", true},
		{"0.0.0.0:29005", "", true},
		{"203.0.113.10:29006", "", true},
		{"[::]:29007", "", true},
		{"127.0.0.1:0", "", true},
		{"127.0.0.1:bad", "", true},
		{"127.0.0.1:29008/path", "", true},
	} {
		got, err := HealthURL(tc.listen)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("HealthURL(%q)=(%q,%v), want (%q, err=%v)", tc.listen, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestRejectsUnimplementedAutoLetsEncrypt(t *testing.T) {
	err := loadText(t, "[server]\nlisten='127.0.0.1:18900'\n[tls]\nenabled=true\nauto_letsencrypt=true\ndomain='example.test'\n")
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("expected Auto-LE fail closed, got %v", err)
	}
}

func TestExplicitInsecureOptInsRemainAvailableForTrustedNetworks(t *testing.T) {
	err := loadText(t, "[server]\nlisten='0.0.0.0:18900'\nallow_insecure_public=true\nudp_listen='0.0.0.0:18902'\nallow_insecure_udp=true\n")
	if err != nil {
		t.Fatalf("explicit compatibility opt-in rejected: %v", err)
	}
}

func TestRejectsPeerCountsOutsideProtocolV1SlotLimit(t *testing.T) {
	for _, count := range []string{"0", "9", "100"} {
		err := loadText(t, "[server]\nlisten='127.0.0.1:18900'\nmax_peers_per_room="+count+"\n")
		if err == nil || !strings.Contains(err.Error(), "between 1 and 8") {
			t.Fatalf("max_peers_per_room=%s should be rejected, got %v", count, err)
		}
	}
	for _, count := range []string{"1", "8"} {
		if err := loadText(t, "[server]\nlisten='127.0.0.1:18900'\nmax_peers_per_room="+count+"\n"); err != nil {
			t.Fatalf("max_peers_per_room=%s rejected: %v", count, err)
		}
	}
}

func TestTrustedProxyEntriesFailClosed(t *testing.T) {
	for _, entry := range []string{
		"127.0.0.1", "127.0.0.0/8", "::1", "2001:db8::/32",
	} {
		body := "[server]\nlisten='127.0.0.1:18900'\ntrusted_proxies=['" + entry + "']\n"
		if err := loadText(t, body); err != nil {
			t.Fatalf("trusted proxy %q rejected: %v", entry, err)
		}
	}
	for _, entry := range []string{
		"", "not-an-ip", "0.0.0.0/0", "::/0", "::ffff:192.0.2.1",
	} {
		body := "[server]\nlisten='127.0.0.1:18900'\ntrusted_proxies=['" + entry + "']\n"
		err := loadText(t, body)
		if err == nil || !strings.Contains(err.Error(), "trusted_proxies") {
			t.Fatalf("unsafe trusted proxy %q should be rejected, got %v", entry, err)
		}
	}
}

func TestServerPasswordHonorsBcryptByteLimit(t *testing.T) {
	valid := strings.Repeat("p", 72)
	if err := loadText(t, "[server]\nlisten='127.0.0.1:18900'\npassword='"+valid+"'\n"); err != nil {
		t.Fatalf("72-byte password rejected: %v", err)
	}
	tooLong := strings.Repeat("密", 25) // 75 UTF-8 bytes
	err := loadText(t, "[server]\nlisten='127.0.0.1:18900'\npassword='"+tooLong+"'\n")
	if err == nil || !strings.Contains(err.Error(), "72") {
		t.Fatalf("75-byte password should be rejected, got %v", err)
	}
}
