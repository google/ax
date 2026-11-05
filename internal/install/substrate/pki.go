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

// Package substrate embeds Agent Substrate's release manifests and bootstraps
// a local Kubernetes cluster without requiring a Substrate source checkout or
// ate-setup binary.
package substrate

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"time"
)

const (
	caPoolKeyID = "1"
	caValidity  = 365 * 24 * time.Hour
)

// KeyType selects the signing algorithm for a Substrate CA pool.
type KeyType int

const (
	// KeyTypeED25519 is used by podcertificate-controller signer pools.
	KeyTypeED25519 KeyType = iota
	// KeyTypeECDSAP256 is used by actor-id-ca-pool and egress-mitm-ca-pool
	// (external relying parties such as GCP WIF reject Ed25519).
	KeyTypeECDSAP256
)

// CAPoolBundle holds the three keys written to a Substrate CA pool Secret
// (kubernetes.io/tls: "pool", "tls.crt", "tls.key").
type CAPoolBundle struct {
	PoolJSON []byte
	CertPEM  []byte
	KeyPEM   []byte
}

type serializedCAPool struct {
	CAs              []*serializedCA
	ActiveForSigning string
}

type serializedCA struct {
	ID                 string
	SigningKeyPKCS8    []byte
	RootCertificateDER []byte
}

// GenerateCAPool creates a single-root CA pool matching Substrate's
// internal/localca wire format.
func GenerateCAPool(keyType KeyType) (CAPoolBundle, error) {
	var privKey crypto.PrivateKey
	var pubKey crypto.PublicKey
	switch keyType {
	case KeyTypeED25519:
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return CAPoolBundle{}, fmt.Errorf("generating Ed25519 CA key: %w", err)
		}
		privKey = priv
		pubKey = pub
	case KeyTypeECDSAP256:
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return CAPoolBundle{}, fmt.Errorf("generating ECDSA P-256 CA key: %w", err)
		}
		privKey = key
		pubKey = &key.PublicKey
	default:
		return CAPoolBundle{}, fmt.Errorf("unsupported CA key type %d", keyType)
	}

	// Non-empty random Subject CommonName avoids Go x509 treating empty-Subject
	// SPIFFE leaf certificates as self-signed when Subject == Issuer.
	cn, err := randomHex(16)
	if err != nil {
		return CAPoolBundle{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(caValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pubKey, privKey)
	if err != nil {
		return CAPoolBundle{}, fmt.Errorf("creating root CA certificate: %w", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(privKey)
	if err != nil {
		return CAPoolBundle{}, fmt.Errorf("marshaling PKCS#8 CA key: %w", err)
	}

	poolJSON, err := json.Marshal(&serializedCAPool{
		CAs: []*serializedCA{{
			ID:                 caPoolKeyID,
			SigningKeyPKCS8:    pkcs8,
			RootCertificateDER: der,
		}},
		ActiveForSigning: caPoolKeyID,
	})
	if err != nil {
		return CAPoolBundle{}, fmt.Errorf("marshaling CA pool JSON: %w", err)
	}

	return CAPoolBundle{
		PoolJSON: poolJSON,
		CertPEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:   pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}),
	}, nil
}

// JWTPoolBundle holds the keys written to ate-system/actor-id-jwt-pool ("pool"
// and RFC 7517 "jwks").
type JWTPoolBundle struct {
	PoolJSON []byte
	JWKSJSON []byte
	KeyID    string
}

type serializedJWTPool struct {
	Authorities      []*serializedJWTAuthority
	ActiveForSigning string
}

type serializedJWTAuthority struct {
	ID              string
	Algorithm       string
	SigningKeyPKCS8 []byte
}

type jwkEntry struct {
	KeyType   string `json:"kty"`
	KeyID     string `json:"kid"`
	Use       string `json:"use"`
	Algorithm string `json:"alg"`
	Curve     string `json:"crv,omitempty"`
	X         string `json:"x,omitempty"`
	Y         string `json:"y,omitempty"`
}

// GenerateES256JWTPool creates an ES256 (ECDSA P-256) JWT signing pool and its
// corresponding public JWK Set matching Substrate's internal/localjwtauthority.
func GenerateES256JWTPool() (JWTPoolBundle, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return JWTPoolBundle{}, fmt.Errorf("generating ES256 JWT key: %w", err)
	}
	spkiDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return JWTPoolBundle{}, fmt.Errorf("marshaling ES256 public key: %w", err)
	}
	sum := sha256.Sum256(spkiDER)
	kid := base64.RawURLEncoding.EncodeToString(sum[:])

	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return JWTPoolBundle{}, fmt.Errorf("marshaling ES256 private key: %w", err)
	}
	poolJSON, err := json.Marshal(&serializedJWTPool{
		Authorities: []*serializedJWTAuthority{{
			ID:              kid,
			Algorithm:       "ES256",
			SigningKeyPKCS8: pkcs8,
		}},
		ActiveForSigning: kid,
	})
	if err != nil {
		return JWTPoolBundle{}, fmt.Errorf("marshaling JWT pool: %w", err)
	}

	// Uncompressed P-256 point: 0x04 || X (32 bytes) || Y (32 bytes).
	point, err := key.PublicKey.Bytes()
	if err != nil {
		return JWTPoolBundle{}, fmt.Errorf("encoding P-256 public point: %w", err)
	}
	jwksJSON, err := json.Marshal(struct {
		Keys []jwkEntry `json:"keys"`
	}{
		Keys: []jwkEntry{{
			KeyType:   "EC",
			KeyID:     kid,
			Use:       "sig",
			Algorithm: "ES256",
			Curve:     "P-256",
			X:         base64.RawURLEncoding.EncodeToString(point[1:33]),
			Y:         base64.RawURLEncoding.EncodeToString(point[33:65]),
		}},
	})
	if err != nil {
		return JWTPoolBundle{}, fmt.Errorf("marshaling JWKS: %w", err)
	}

	return JWTPoolBundle{
		PoolJSON: poolJSON,
		JWKSJSON: jwksJSON,
		KeyID:    kid,
	}, nil
}

func randomHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
