package gateway

import (
	"testing"
	"time"
)

func TestPinnedCertificateEndpointSAN(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1", "gateway.example.test"} {
		t.Run(host, func(t *testing.T) {
			certificate, err := newSelfSignedCertificate(host, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if err := certificate.leaf.VerifyHostname(host); err != nil {
				t.Fatalf("certificate does not cover endpoint %q: %v", host, err)
			}
			if err := certificate.leaf.VerifyHostname("other.example.test"); err == nil {
				t.Fatal("certificate unexpectedly covers a different host")
			}
		})
	}
}
