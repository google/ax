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

package substrate

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"testing"
)

func TestGenerateCAPoolFormats(t *testing.T) {
	for _, tc := range []struct {
		name string
		kt   KeyType
	}{
		{"ed25519", KeyTypeED25519},
		{"ecdsa-p256", KeyTypeECDSAP256},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := GenerateCAPool(tc.kt)
			if err != nil {
				t.Fatalf("GenerateCAPool: %v", err)
			}
			var wire serializedCAPool
			if err := json.Unmarshal(b.PoolJSON, &wire); err != nil {
				t.Fatalf("Unmarshal PoolJSON: %v", err)
			}
			if wire.ActiveForSigning != "1" || len(wire.CAs) != 1 {
				t.Fatalf("unexpected pool wire state: %+v", wire)
			}
			cert, err := x509.ParseCertificate(wire.CAs[0].RootCertificateDER)
			if err != nil {
				t.Fatalf("ParseCertificate: %v", err)
			}
			if !cert.IsCA || cert.Subject.CommonName == "" {
				t.Fatalf("expected non-empty CA Subject.CommonName, got %+v", cert.Subject)
			}
			key, err := x509.ParsePKCS8PrivateKey(wire.CAs[0].SigningKeyPKCS8)
			if err != nil {
				t.Fatalf("ParsePKCS8PrivateKey: %v", err)
			}
			switch tc.kt {
			case KeyTypeED25519:
				if _, ok := key.(ed25519.PrivateKey); !ok {
					t.Fatalf("expected ed25519.PrivateKey, got %T", key)
				}
			case KeyTypeECDSAP256:
				if _, ok := key.(*ecdsa.PrivateKey); !ok {
					t.Fatalf("expected *ecdsa.PrivateKey, got %T", key)
				}
			}

			block, _ := pem.Decode(b.CertPEM)
			if block == nil || block.Type != "CERTIFICATE" {
				t.Fatal("invalid CertPEM")
			}
			keyBlock, _ := pem.Decode(b.KeyPEM)
			if keyBlock == nil || keyBlock.Type != "PRIVATE KEY" {
				t.Fatal("invalid KeyPEM")
			}
		})
	}
}

func TestGenerateES256JWTPool(t *testing.T) {
	b, err := GenerateES256JWTPool()
	if err != nil {
		t.Fatalf("GenerateES256JWTPool: %v", err)
	}
	var pool serializedJWTPool
	if err := json.Unmarshal(b.PoolJSON, &pool); err != nil {
		t.Fatalf("Unmarshal PoolJSON: %v", err)
	}
	if len(pool.Authorities) != 1 || pool.ActiveForSigning != b.KeyID || pool.Authorities[0].Algorithm != "ES256" {
		t.Fatalf("unexpected JWT pool: %+v", pool)
	}
	var jwks struct {
		Keys []jwkEntry `json:"keys"`
	}
	if err := json.Unmarshal(b.JWKSJSON, &jwks); err != nil {
		t.Fatalf("Unmarshal JWKSJSON: %v", err)
	}
	if len(jwks.Keys) != 1 || jwks.Keys[0].KeyID != b.KeyID || jwks.Keys[0].Curve != "P-256" {
		t.Fatalf("unexpected JWKS: %+v", jwks)
	}
}
