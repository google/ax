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
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	serviceDNSSignerName  = "servicedns.podcert.ate.dev/identity"
	podIdentitySignerName = "podidentity.podcert.ate.dev/identity"
	liveBundleSelector    = "podcert.ate.dev/canarying=live"
	routerServerName      = "atenet-router.ate-system.svc"
)

type cachedRouterTLS struct {
	TrustBundlePEM string    `json:"trust_bundle_pem"`
	CertChainPEM   string    `json:"cert_chain_pem"`
	KeyPEM         string    `json:"key_pem"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// RouterTLSConfig builds a TLS configuration for connecting to Substrate's
// atenet-router over mTLS. It fetches the router's serving CA from the live
// servicedns ClusterTrustBundle and requests a short-lived client certificate
// from the podidentity signer for the given namespace and ServiceAccount.
func RouterTLSConfig(ctx context.Context, kubeContext, namespace, serviceAccount string) (*tls.Config, error) {
	ctxName, err := CurrentContext(kubeContext)
	if err != nil {
		ctxName = kubeContext
	}

	if cfg, err := loadCachedRouterTLS(ctxName, namespace, serviceAccount); err == nil && cfg != nil {
		return cfg, nil
	}

	trustPEM, roots, err := serviceDNSTrustPool(ctx, ctxName)
	if err != nil {
		return nil, fmt.Errorf("loading servicedns trust bundle: %w", err)
	}

	cert, chainPEM, keyPEM, err := requestPodIdentityCert(ctx, ctxName, namespace, serviceAccount)
	if err != nil {
		return nil, fmt.Errorf("requesting podidentity client certificate: %w", err)
	}

	_ = saveCachedRouterTLS(ctxName, namespace, serviceAccount, &cachedRouterTLS{
		TrustBundlePEM: trustPEM,
		CertChainPEM:   chainPEM,
		KeyPEM:         keyPEM,
		ExpiresAt:      time.Now().Add(30 * time.Minute),
	})

	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		RootCAs:      roots,
		Certificates: []tls.Certificate{cert},
		ServerName:   routerServerName,
	}, nil
}

func routerTLSCachePath(ctxName, namespace, serviceAccount string) (string, error) {
	dir, err := TunnelDir()
	if err != nil {
		return "", err
	}
	key := SanitizeContext(ctxName + "-" + namespace + "-" + serviceAccount)
	return filepath.Join(dir, key+"-router-tls.json"), nil
}

func loadCachedRouterTLS(ctxName, namespace, serviceAccount string) (*tls.Config, error) {
	path, err := routerTLSCachePath(ctxName, namespace, serviceAccount)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cached cachedRouterTLS
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil, err
	}
	if time.Now().After(cached.ExpiresAt) {
		return nil, fmt.Errorf("cached router TLS credentials expired")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(cached.TrustBundlePEM)) {
		return nil, fmt.Errorf("cached trust bundle contains no valid certificates")
	}
	cert, err := tls.X509KeyPair([]byte(cached.CertChainPEM), []byte(cached.KeyPEM))
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		RootCAs:      pool,
		Certificates: []tls.Certificate{cert},
		ServerName:   routerServerName,
	}, nil
}

func saveCachedRouterTLS(ctxName, namespace, serviceAccount string, cached *cachedRouterTLS) error {
	path, err := routerTLSCachePath(ctxName, namespace, serviceAccount)
	if err != nil {
		return err
	}
	data, err := json.Marshal(cached)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func kubectlArgs(ctxName string, args ...string) []string {
	if ctxName != "" {
		return append([]string{"--context", ctxName}, args...)
	}
	return args
}

func serviceDNSTrustPool(ctx context.Context, ctxName string) (string, *x509.CertPool, error) {
	args := kubectlArgs(ctxName, "get", "clustertrustbundles.certificates.k8s.io", "-l", liveBundleSelector, "-o", "json")
	out, err := exec.CommandContext(ctx, "kubectl", args...).CombinedOutput()
	if err != nil {
		return "", nil, fmt.Errorf("kubectl get clustertrustbundles: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				SignerName  string `json:"signerName"`
				TrustBundle string `json:"trustBundle"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return "", nil, fmt.Errorf("decoding ClusterTrustBundle list: %w", err)
	}

	pool := x509.NewCertPool()
	var pemBuf strings.Builder
	found := false
	for _, item := range list.Items {
		if item.Spec.SignerName != serviceDNSSignerName {
			continue
		}
		if !pool.AppendCertsFromPEM([]byte(item.Spec.TrustBundle)) {
			return "", nil, fmt.Errorf("ClusterTrustBundle %q contains no valid certificates", item.Metadata.Name)
		}
		pemBuf.WriteString(item.Spec.TrustBundle)
		if !strings.HasSuffix(item.Spec.TrustBundle, "\n") {
			pemBuf.WriteByte('\n')
		}
		found = true
	}
	if !found {
		return "", nil, fmt.Errorf("no live ClusterTrustBundle found for signer %q", serviceDNSSignerName)
	}
	return pemBuf.String(), pool, nil
}

func requestPodIdentityCert(ctx context.Context, ctxName, namespace, serviceAccount string) (tls.Certificate, string, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", "", fmt.Errorf("generating key: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, "", "", fmt.Errorf("marshaling key: %w", err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))

	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return tls.Certificate{}, "", "", fmt.Errorf("creating CSR: %w", err)
	}

	var suffixBytes [4]byte
	_, _ = rand.Read(suffixBytes[:])
	name := "ax-ssh-" + hex.EncodeToString(suffixBytes[:])

	var createErr error
	created := false
	for _, apiVersion := range []string{"certificates.k8s.io/v1", "certificates.k8s.io/v1beta1"} {
		manifest := map[string]any{
			"apiVersion": apiVersion,
			"kind":       "PodCertificateRequest",
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
			},
			"spec": map[string]any{
				"signerName":           podIdentitySignerName,
				"podName":              name,
				"podUID":               randomUUID(),
				"serviceAccountName":   serviceAccount,
				"serviceAccountUID":    randomUUID(),
				"nodeName":             "ax-cli",
				"nodeUID":              randomUUID(),
				"maxExpirationSeconds": 3600,
				"stubPKCS10Request":    base64.StdEncoding.EncodeToString(csr),
			},
		}
		body, err := json.Marshal(manifest)
		if err != nil {
			return tls.Certificate{}, "", "", err
		}
		cmd := exec.CommandContext(ctx, "kubectl", kubectlArgs(ctxName, "create", "-f", "-")...)
		cmd.Stdin = bytes.NewReader(body)
		out, err := cmd.CombinedOutput()
		if err == nil {
			created = true
			break
		}
		createErr = fmt.Errorf("%w (%s)", err, strings.TrimSpace(string(out)))
	}
	if !created {
		return tls.Certificate{}, "", "", fmt.Errorf("creating PodCertificateRequest %s/%s: %w", namespace, name, createErr)
	}

	defer func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(delCtx, "kubectl",
			kubectlArgs(ctxName, "-n", namespace, "delete", "podcertificaterequests.certificates.k8s.io", name, "--ignore-not-found")...,
		).Run()
	}()

	pollCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var chainPEM string
	for {
		out, err := exec.CommandContext(pollCtx, "kubectl",
			kubectlArgs(ctxName, "-n", namespace, "get", "podcertificaterequests.certificates.k8s.io", name, "-o", "json")...,
		).CombinedOutput()
		if err == nil {
			var pcr struct {
				Status struct {
					CertificateChain string `json:"certificateChain"`
					Conditions       []struct {
						Type    string `json:"type"`
						Status  string `json:"status"`
						Reason  string `json:"reason"`
						Message string `json:"message"`
					} `json:"conditions"`
				} `json:"status"`
			}
			if json.Unmarshal(out, &pcr) == nil {
				if pcr.Status.CertificateChain != "" {
					chainPEM = pcr.Status.CertificateChain
					break
				}
				for _, c := range pcr.Status.Conditions {
					if c.Status != "True" {
						continue
					}
					if c.Type == "Issued" && pcr.Status.CertificateChain != "" {
						chainPEM = pcr.Status.CertificateChain
						break
					}
					if c.Type == "Denied" || c.Type == "Failed" {
						return tls.Certificate{}, "", "", fmt.Errorf("PodCertificateRequest %s: %s: %s", c.Type, c.Reason, c.Message)
					}
				}
				if chainPEM != "" {
					break
				}
			}
		}

		select {
		case <-pollCtx.Done():
			return tls.Certificate{}, "", "", fmt.Errorf("waiting for PodCertificateRequest %s/%s to be issued: %w", namespace, name, pollCtx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}

	cert, err := tls.X509KeyPair([]byte(chainPEM), []byte(keyPEM))
	if err != nil {
		return tls.Certificate{}, "", "", fmt.Errorf("parsing issued certificate chain: %w", err)
	}
	return cert, chainPEM, keyPEM, nil
}

func randomUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
