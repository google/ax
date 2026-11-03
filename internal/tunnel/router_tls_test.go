// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tunnel

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

// selfSignedPEM returns a self-signed certificate valid until notAfter and
// its private key, both PEM-encoded.
func selfSignedPEM(t *testing.T, notAfter time.Time) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "ax-test"},
		NotBefore:    notAfter.Add(-2 * time.Hour),
		NotAfter:     notAfter,
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

func TestLoadCachedRouterTLS_CertExpiry(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name     string
		notAfter time.Time
		wantErr  bool
	}{
		{"valid", now.Add(time.Hour), false},
		{"expired", now.Add(-time.Minute), true},
		{"within expiry margin", now.Add(routerCertExpiry / 2), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AX_HOME", t.TempDir())
			certPEM, keyPEM := selfSignedPEM(t, tt.notAfter)
			// The cache entry itself has not expired; only the certificate may have.
			if err := saveCachedRouterTLS("ctx", "ns", "sa", &cachedRouterTLS{
				TrustBundlePEM: certPEM,
				CertChainPEM:   certPEM,
				KeyPEM:         keyPEM,
				ExpiresAt:      now.Add(30 * time.Minute),
			}); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadCachedRouterTLS("ctx", "ns", "sa")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("loadCachedRouterTLS() = %v, want error", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadCachedRouterTLS() error = %v", err)
			}
			if len(cfg.Certificates) != 1 {
				t.Fatalf("got %d certificates, want 1", len(cfg.Certificates))
			}
		})
	}
}
