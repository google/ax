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
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/ax/internal/install/executil"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"go.ate.dev/substrate/pkg/postgressetup"
	"gopkg.in/yaml.v3"
)

//go:embed manifests/*
var embeddedManifests embed.FS

const (
	NamespaceAteSystem = "ate-system"
	NamespacePodCert   = "podcertificate-controller-system"
	NamespaceWorkers   = "ate-workers"

	versionAnnotation = "ax.io/substrate-version"
	envHashAnnotation = "ate.dev/env-hash"

	inClusterIssuer = "https://kubernetes.default.svc"
)

var expectedTrustBundles = []string{
	"podidentity.podcert.ate.dev:identity:primary-bundle",
	"servicedns.podcert.ate.dev:identity:primary-bundle",
	"postgres.podcert.ate.dev:identity:primary-bundle",
}

// BootstrapOptions configures local Agent Substrate installation.
type BootstrapOptions struct {
	KubeContext    string
	AXNamespace    string
	WorkerReplicas int
	RolloutTimeout time.Duration
	Runner         executil.Runner
	ToolURLs       executil.ToolURLs
	Out            io.Writer
}

// EmbeddedVersion returns the Substrate version compiled into the binary.
func EmbeddedVersion() string {
	b, _ := embeddedManifests.ReadFile("manifests/version.txt")
	return strings.TrimSpace(string(b))
}

// Bootstrap installs or reconciles Agent Substrate (control plane, bundled
// mTLS Postgres, RustFS snapshot store, k8s-credential-provider, atenet-egress,
// and gVisor WorkerPool) on a local cluster.
func Bootstrap(ctx context.Context, opts BootstrapOptions) error {
	r := opts.Runner
	if r == nil {
		r = executil.OSRunner{}
	}
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	axNS := opts.AXNamespace
	if axNS == "" {
		axNS = v1alpha1.DefaultSystemNamespace
	}
	workers := opts.WorkerReplicas
	if workers <= 0 {
		workers = 2
	}
	timeout := opts.RolloutTimeout
	if timeout <= 0 {
		timeout = 4 * time.Minute
	}

	kubectlBin, err := executil.EnsureKubectl(ctx, r, opts.ToolURLs, out)
	if err != nil {
		return err
	}
	k := &kubeHelper{runner: r, kubectl: kubectlBin, context: opts.KubeContext}
	version := EmbeddedVersion()

	// Fast-path: if Substrate at this exact version and the worker pool are
	// already healthy, skip re-applying control-plane bundles.
	if k.alreadyHealthy(ctx, version, workers) {
		fmt.Fprintf(out, "==> Substrate %s is already healthy on context %q.\n", version, opts.KubeContext)
		return nil
	}

	fmt.Fprintf(out, "==> Installing Agent Substrate %s (local profile)...\n", version)

	// 1. Namespaces & CRDs.
	for _, ns := range []string{NamespacePodCert, NamespaceAteSystem, NamespaceWorkers} {
		if err := k.ensureNamespace(ctx, ns); err != nil {
			return err
		}
	}
	crdsYAML, err := embeddedManifests.ReadFile("manifests/01-crds.yaml")
	if err != nil {
		return err
	}
	if err := k.applyServerSide(ctx, crdsYAML); err != nil {
		return fmt.Errorf("applying Substrate CRDs: %w", err)
	}
	if err := k.waitCRDsEstablished(ctx); err != nil {
		return err
	}

	// 2. PodCertificate controller & signer CA pools (Ed25519).
	for _, secretName := range []string{"service-dns-ca-pool", "pod-identity-ca-pool", "postgres-ca-pool"} {
		if err := ensureCAPoolSecret(ctx, k, NamespacePodCert, secretName, KeyTypeED25519); err != nil {
			return err
		}
	}
	podcertYAML, err := embeddedManifests.ReadFile("manifests/02-podcert-controller.yaml")
	if err != nil {
		return err
	}
	if err := k.apply(ctx, podcertYAML); err != nil {
		return fmt.Errorf("applying podcertificate-controller: %w", err)
	}
	if err := k.rolloutStatus(ctx, NamespacePodCert, "deployment/podcertificate-controller", timeout); err != nil {
		return err
	}
	if err := k.waitClusterTrustBundles(ctx, expectedTrustBundles, 90*time.Second); err != nil {
		return err
	}

	// 3. SandboxConfig (gvisor-default).
	sandboxCfgYAML, err := embeddedManifests.ReadFile("manifests/03-sandboxconfig.yaml")
	if err != nil {
		return err
	}
	if err := k.apply(ctx, sandboxCfgYAML); err != nil {
		return fmt.Errorf("applying SandboxConfig: %w", err)
	}

	// 4. Bundled mTLS PostgreSQL + schema/role initialization.
	fmt.Fprintf(out, "==> Starting Substrate PostgreSQL and initializing mTLS roles...\n")
	pgYAML, err := embeddedManifests.ReadFile("manifests/04-postgres.yaml")
	if err != nil {
		return err
	}
	if err := k.apply(ctx, pgYAML); err != nil {
		return fmt.Errorf("applying Substrate PostgreSQL: %w", err)
	}
	if err := k.rolloutStatus(ctx, NamespaceAteSystem, "statefulset/postgres", timeout); err != nil {
		return err
	}
	if err := setupBundledPostgresRoles(ctx, k); err != nil {
		return err
	}

	// 5. ate-system PKI pools & ate-api-server ConfigMaps/Secrets.
	if err := ensureJWTPoolSecret(ctx, k, NamespaceAteSystem, "actor-id-jwt-pool"); err != nil {
		return err
	}
	if err := ensureCAPoolSecret(ctx, k, NamespaceAteSystem, "actor-id-ca-pool", KeyTypeECDSAP256); err != nil {
		return err
	}
	if err := ensureActorIDCACerts(ctx, k); err != nil {
		return err
	}
	if err := ensureCAPoolSecret(ctx, k, NamespaceAteSystem, "egress-mitm-ca-pool", KeyTypeECDSAP256); err != nil {
		return err
	}
	envHash, err := ensureAPIServerConfigAndEnv(ctx, k)
	if err != nil {
		return err
	}

	// 6. Control plane bundle (pre-wired with ax-server bootstrap authz so zero
	// extra restarts happen during Step 3 `ax install`), k8s-credential-provider,
	// and atenet-egress.
	fmt.Fprintf(out, "==> Deploying Substrate control plane, router, and k8s-credential-provider...\n")
	if err := ensureInitialCredProviderPolicy(ctx, k, axNS); err != nil {
		return err
	}

	rawSystemYAML, err := embeddedManifests.ReadFile("manifests/05-ate-system.yaml")
	if err != nil {
		return err
	}
	patchedSystemYAML, err := RenderSystemBundle(rawSystemYAML, envHash, axNS)
	if err != nil {
		return err
	}
	if err := k.apply(ctx, patchedSystemYAML); err != nil {
		return fmt.Errorf("applying ate-system control plane: %w", err)
	}

	credProviderYAML, err := embeddedManifests.ReadFile("manifests/06-credprovider.yaml")
	if err != nil {
		return err
	}
	if err := k.apply(ctx, credProviderYAML); err != nil {
		return fmt.Errorf("applying k8s-credential-provider: %w", err)
	}

	egressYAML, err := embeddedManifests.ReadFile("manifests/07-atenet-egress.yaml")
	if err != nil {
		return err
	}
	if err := k.apply(ctx, egressYAML); err != nil {
		return fmt.Errorf("applying atenet-egress: %w", err)
	}

	for _, workload := range []string{
		"deployment/ate-api-server",
		"deployment/ate-idp-server",
		"deployment/ate-controller",
		"deployment/atenet-router",
		"deployment/atenet-egress",
		"deployment/k8s-credential-provider",
		"daemonset/atelet",
	} {
		if err := k.rolloutStatus(ctx, NamespaceAteSystem, workload, timeout); err != nil {
			return err
		}
	}

	// 7. Default gVisor WorkerPool.
	fmt.Fprintf(out, "==> Deploying gVisor WorkerPool (%d replica(s))...\n", workers)
	wpTemplate, err := embeddedManifests.ReadFile("manifests/08-workerpool.yaml")
	if err != nil {
		return err
	}
	wpYAML := strings.ReplaceAll(string(wpTemplate), "${AX_WORKER_REPLICAS}", strconv.Itoa(workers))
	if err := k.apply(ctx, []byte(wpYAML)); err != nil {
		return fmt.Errorf("applying WorkerPool: %w", err)
	}
	if err := k.waitDeploymentCreatedAndReady(ctx, NamespaceWorkers, "ax-workers", timeout); err != nil {
		return err
	}

	_, _ = k.run(ctx, nil, "annotate", "namespace", NamespaceAteSystem, versionAnnotation+"="+version, "--overwrite")
	return nil
}

// RenderSystemBundle stamps ate.dev/env-hash on ate-api-server and pre-wires
// initial bootstrapOwners and ingressAllowedSPIFFEIDs for axNamespace so
// initial `ax local up` needs zero post-install rolling restarts.
func RenderSystemBundle(raw []byte, envHash, axNamespace string) ([]byte, error) {
	if axNamespace == "" {
		axNamespace = v1alpha1.DefaultSystemNamespace
	}
	owners := fmt.Sprintf("system:serviceaccount:ate-system:ate-client,system:serviceaccount:%s:ax-server", axNamespace)
	spiffeID := fmt.Sprintf("spiffe://cluster.local/ns/%s/sa/ax-server", axNamespace)

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)

	stampedAPI := false
	stampedRouter := false
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decoding ate-system manifest: %w", err)
		}
		if len(doc) == 0 {
			continue
		}
		kind, _ := doc["kind"].(string)
		meta, _ := doc["metadata"].(map[string]any)
		name, _ := meta["name"].(string)

		if kind == "Deployment" && name == "ate-api-server" {
			setPodTemplateAnnotation(doc, envHashAnnotation, envHash)
			if err := setContainerFlag(doc, "ate-api-server", "--authz-bootstrap-owners", owners); err != nil {
				return nil, err
			}
			stampedAPI = true
		}
		if kind == "Deployment" && name == "atenet-router" {
			if err := setContainerFlag(doc, "atenet-router", "--ingress-allowed-spiffe-ids", spiffeID); err != nil {
				return nil, err
			}
			stampedRouter = true
		}

		if err := enc.Encode(doc); err != nil {
			return nil, fmt.Errorf("encoding ate-system manifest: %w", err)
		}
	}
	_ = enc.Close()

	if !stampedAPI || !stampedRouter {
		return nil, fmt.Errorf("ate-system bundle missing Deployment (ate-api-server=%v, atenet-router=%v)", stampedAPI, stampedRouter)
	}
	return out.Bytes(), nil
}

func setPodTemplateAnnotation(dep map[string]any, key, val string) {
	spec := ensureMap(dep, "spec")
	tmpl := ensureMap(spec, "template")
	meta := ensureMap(tmpl, "metadata")
	ann := ensureMap(meta, "annotations")
	ann[key] = val
}

func setContainerFlag(dep map[string]any, containerName, flagName, flagVal string) error {
	spec, _ := dep["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tmpl["spec"].(map[string]any)
	containers, _ := podSpec["containers"].([]any)
	for _, c := range containers {
		cm, ok := c.(map[string]any)
		if !ok || cm["name"] != containerName {
			continue
		}
		rawArgs, _ := cm["args"].([]any)
		var kept []any
		for _, a := range rawArgs {
			s, _ := a.(string)
			if s == flagName || strings.HasPrefix(s, flagName+"=") {
				continue
			}
			kept = append(kept, a)
		}
		kept = append(kept, flagName+"="+flagVal)
		cm["args"] = kept
		return nil
	}
	return fmt.Errorf("container %q not found in Deployment", containerName)
}

func ensureMap(parent map[string]any, key string) map[string]any {
	if existing, ok := parent[key].(map[string]any); ok && existing != nil {
		return existing
	}
	m := make(map[string]any)
	parent[key] = m
	return m
}

func bundledPostgresDSN(user string) string {
	bundle := "/run/postgres.podcert.ate.dev/" + user + ".pem"
	return fmt.Sprintf(
		"postgresql://%s@postgres.ate-system.svc:5432/atepg?sslmode=verify-full&sslrootcert=/run/servicedns.podcert.ate.dev/trust-bundle.pem&sslcert=%s&sslkey=%s",
		user, bundle, bundle,
	)
}

func setupBundledPostgresRoles(ctx context.Context, k *kubeHelper) error {
	args := []string{
		"-n", NamespaceAteSystem,
		"exec", "-i", "postgres-0", "-c", "postgres", "--",
		"psql", "--no-psqlrc", "--set=ON_ERROR_STOP=1", "--username", "postgres", "--dbname", "atepg",
	}
	args = append(args, postgressetup.DefaultConfig().PSQLArgs()...)
	if _, err := k.run(ctx, strings.NewReader(postgressetup.Script()), args...); err != nil {
		return fmt.Errorf("configuring Substrate PostgreSQL identities: %w", err)
	}
	return nil
}

func ensureCAPoolSecret(ctx context.Context, k *kubeHelper, namespace, name string, kt KeyType) error {
	if k.resourceExists(ctx, namespace, "secret", name) {
		return nil
	}
	bundle, err := GenerateCAPool(kt)
	if err != nil {
		return err
	}
	return k.applySecret(ctx, namespace, name, "kubernetes.io/tls", map[string][]byte{
		"pool":    bundle.PoolJSON,
		"tls.crt": bundle.CertPEM,
		"tls.key": bundle.KeyPEM,
	})
}

func ensureJWTPoolSecret(ctx context.Context, k *kubeHelper, namespace, name string) error {
	if k.resourceExists(ctx, namespace, "secret", name) {
		return nil
	}
	bundle, err := GenerateES256JWTPool()
	if err != nil {
		return err
	}
	return k.applySecret(ctx, namespace, name, "Opaque", map[string][]byte{
		"pool": bundle.PoolJSON,
		"jwks": bundle.JWKSJSON,
	})
}

func ensureActorIDCACerts(ctx context.Context, k *kubeHelper) error {
	if k.resourceExists(ctx, NamespaceAteSystem, "secret", "actor-id-ca-certs") {
		return nil
	}
	res, err := k.run(ctx, nil, "-n", NamespaceAteSystem, "get", "secret", "actor-id-ca-pool", "-o", "jsonpath={.data.tls\\.crt}")
	if err != nil {
		return fmt.Errorf("reading actor-id-ca-pool certificate: %w", err)
	}
	certPEM, err := base64.StdEncoding.DecodeString(strings.TrimSpace(res.Stdout))
	if err != nil {
		return fmt.Errorf("decoding actor-id-ca-pool certificate: %w", err)
	}
	return k.applySecret(ctx, NamespaceAteSystem, "actor-id-ca-certs", "Opaque", map[string][]byte{
		"ca.crt": certPEM,
	})
}

func ensureAPIServerConfigAndEnv(ctx context.Context, k *kubeHelper) (string, error) {
	if !k.resourceExists(ctx, NamespaceAteSystem, "configmap", "ate-api-config") {
		if err := k.applyConfigMap(ctx, NamespaceAteSystem, "ate-api-config", map[string]string{
			"apiconfig.textproto": "logging: {\n  level: \"info\"\n}\n",
		}); err != nil {
			return "", err
		}
	}

	if !k.resourceExists(ctx, NamespaceAteSystem, "configmap", "ate-api-authentication") {
		issuer := k.discoverOIDCIssuer(ctx)
		authn := fmt.Sprintf("jwtProviders:\n- name: kubernetes\n  issuer: %s\n  audiences: [api.ate-system.svc]\n", issuer)
		if issuer == inClusterIssuer || issuer == inClusterIssuer+".cluster.local" {
			authn += "  certificateAuthorityFile: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt\n" +
				"  discoveryTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token"
		} else {
			authn = strings.TrimRight(authn, "\n")
		}
		if err := k.applyConfigMap(ctx, NamespaceAteSystem, "ate-api-authentication", map[string]string{
			"authentication.yaml": authn,
		}); err != nil {
			return "", err
		}
	}

	cmData := map[string]string{
		"ATE_API_ACTOR_JWT_ISSUER":                  "https://idp.ate-system.svc",
		"ATE_API_POSTGRES_OWNER_ROLE":               postgressetup.OwnerRole,
		"ATE_API_POSTGRES_READ_WRITE_ROLE":          postgressetup.ReadWriteRole,
		"ATE_API_SNAPSHOT_LOCATION":                 "gs://ate-snapshots/",
		"ATE_API_SNAPSHOT_TRANSFER_SERVICE_ACCOUNT": "",
	}
	if err := k.applyConfigMap(ctx, NamespaceAteSystem, "ate-api-server-envvars", cmData); err != nil {
		return "", err
	}

	secretData := map[string][]byte{
		"ATE_API_POSTGRES_READ_WRITE_CONNECTION_STRING": []byte(bundledPostgresDSN(postgressetup.ReadWriteUser)),
		"ATE_API_POSTGRES_OWNER_CONNECTION_STRING":      []byte(bundledPostgresDSN(postgressetup.OwnerUser)),
		"ATE_API_POSTGRES_SCHEMA":                       []byte(postgressetup.Schema),
	}
	if err := k.applySecret(ctx, NamespaceAteSystem, "ate-api-server-secret-envvars", "Opaque", secretData); err != nil {
		return "", err
	}

	return computeEnvHash(cmData, secretData), nil
}

func ensureInitialCredProviderPolicy(ctx context.Context, k *kubeHelper, axNamespace string) error {
	if k.resourceExists(ctx, NamespaceAteSystem, "configmap", "k8s-credential-provider-namespace-policy") {
		return nil
	}
	allowed := []string{"default"}
	if axNamespace != "" && axNamespace != "default" {
		allowed = []string{axNamespace, "default"}
	}
	policyObj := map[string]any{
		"policies": []map[string]any{{
			"atespace":          "default",
			"allowedNamespaces": allowed,
			"secretSelector": map[string]any{
				"matchLabels": map[string]string{
					"ax.io/credential": "true",
				},
			},
		}},
	}
	policyBytes, err := yaml.Marshal(policyObj)
	if err != nil {
		return err
	}
	return k.applyConfigMap(ctx, NamespaceAteSystem, "k8s-credential-provider-namespace-policy", map[string]string{
		"namespace-policy.yaml": string(policyBytes),
	})
}

func computeEnvHash(cm map[string]string, secret map[string][]byte) string {
	h := sha256.New()
	fmt.Fprint(h, "configmap\n")
	for _, k := range slices.Sorted(maps.Keys(cm)) {
		fmt.Fprintf(h, "%s=%s\n", k, cm[k])
	}
	fmt.Fprint(h, "secret\n")
	for _, k := range slices.Sorted(maps.Keys(secret)) {
		fmt.Fprintf(h, "%s=%s\n", k, secret[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

type kubeHelper struct {
	runner  executil.Runner
	kubectl string
	context string
}

func (k *kubeHelper) run(ctx context.Context, stdin io.Reader, args ...string) (executil.Result, error) {
	var fullArgs []string
	if k.context != "" {
		fullArgs = append(fullArgs, "--context", k.context)
	}
	fullArgs = append(fullArgs, args...)
	return k.runner.Run(ctx, executil.Spec{
		Name:  k.kubectl,
		Args:  fullArgs,
		Stdin: stdin,
	})
}

func (k *kubeHelper) apply(ctx context.Context, manifest []byte) error {
	_, err := k.run(ctx, bytes.NewReader(manifest), "apply", "-f", "-")
	return err
}

func (k *kubeHelper) applyServerSide(ctx context.Context, manifest []byte) error {
	_, err := k.run(ctx, bytes.NewReader(manifest), "apply", "--server-side", "--force-conflicts", "-f", "-")
	return err
}

func (k *kubeHelper) ensureNamespace(ctx context.Context, name string) error {
	doc := fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n", name)
	return k.apply(ctx, []byte(doc))
}

func (k *kubeHelper) resourceExists(ctx context.Context, namespace, kind, name string) bool {
	var args []string
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	args = append(args, "get", kind, name, "-o", "name")
	_, err := k.run(ctx, nil, args...)
	return err == nil
}

func (k *kubeHelper) applySecret(ctx context.Context, namespace, name, secretType string, data map[string][]byte) error {
	encoded := make(map[string]string, len(data))
	for key, val := range data {
		encoded[key] = base64.StdEncoding.EncodeToString(val)
	}
	obj := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       secretType,
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"data": encoded,
	}
	raw, err := yaml.Marshal(obj)
	if err != nil {
		return err
	}
	return k.apply(ctx, raw)
}

func (k *kubeHelper) applyConfigMap(ctx context.Context, namespace, name string, data map[string]string) error {
	obj := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"data": data,
	}
	raw, err := yaml.Marshal(obj)
	if err != nil {
		return err
	}
	return k.apply(ctx, raw)
}

func (k *kubeHelper) discoverOIDCIssuer(ctx context.Context) string {
	res, err := k.run(ctx, nil, "get", "--raw", "/.well-known/openid-configuration")
	if err != nil {
		return inClusterIssuer
	}
	var doc struct {
		Issuer string `json:"issuer"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &doc); err == nil && doc.Issuer != "" {
		return doc.Issuer
	}
	return inClusterIssuer
}

func (k *kubeHelper) rolloutStatus(ctx context.Context, namespace, target string, timeout time.Duration) error {
	_, err := k.run(ctx, nil, "-n", namespace, "rollout", "status", target, fmt.Sprintf("--timeout=%ds", int(timeout.Seconds())))
	if err != nil {
		return fmt.Errorf("waiting for %s/%s rollout: %w", namespace, target, err)
	}
	return nil
}

func (k *kubeHelper) waitCRDsEstablished(ctx context.Context) error {
	for _, crd := range []string{"sandboxconfigs.ate.dev", "workerpools.ate.dev"} {
		if _, err := k.run(ctx, nil, "wait", "--for=condition=Established", "crd/"+crd, "--timeout=60s"); err != nil {
			return fmt.Errorf("waiting for CRD %s: %w", crd, err)
		}
	}
	return nil
}

func (k *kubeHelper) waitClusterTrustBundles(ctx context.Context, names []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		allPresent := true
		for _, name := range names {
			if !k.resourceExists(ctx, "", "clustertrustbundle.certificates.k8s.io", name) {
				allPresent = false
				break
			}
		}
		if allPresent {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for ClusterTrustBundles %v", names)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (k *kubeHelper) waitDeploymentCreatedAndReady(ctx context.Context, namespace, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for !k.resourceExists(ctx, namespace, "deployment", name) {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for ate-controller to create %s/deployment/%s", namespace, name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	remaining := time.Until(deadline)
	if remaining < 30*time.Second {
		remaining = 30 * time.Second
	}
	return k.rolloutStatus(ctx, namespace, "deployment/"+name, remaining)
}

func (k *kubeHelper) alreadyHealthy(ctx context.Context, wantVersion string, wantWorkers int) bool {
	res, err := k.run(ctx, nil, "get", "namespace", NamespaceAteSystem, "-o", fmt.Sprintf("jsonpath={.metadata.annotations.%s}", strings.ReplaceAll(versionAnnotation, ".", "\\.")))
	if err != nil || strings.TrimSpace(res.Stdout) != wantVersion {
		return false
	}
	r, err := k.run(ctx, nil, "-n", NamespaceWorkers, "get", "deployment", "ax-workers", "-o", "jsonpath={.spec.replicas}")
	if err != nil {
		return false
	}
	specReplicas, convErr := strconv.Atoi(strings.TrimSpace(r.Stdout))
	if convErr != nil || specReplicas != wantWorkers {
		return false
	}
	// Wait for actual pod readiness (after `docker start` from `ax local stop`,
	// Deployment.status.availableReplicas in etcd can briefly reflect pre-stop
	// state while kubelet is still restarting containers).
	if _, err := k.run(ctx, nil, "-n", NamespaceAteSystem, "wait", "--for=condition=Ready", "pod/postgres-0", "--timeout=90s"); err != nil {
		return false
	}
	for _, selector := range []struct{ ns, sel string }{
		{NamespaceAteSystem, "app=ate-api-server"},
		{NamespaceAteSystem, "app=atenet-router"},
		{NamespaceWorkers, "ate.dev/worker-pool=ax-workers"},
	} {
		if _, err := k.run(ctx, nil, "-n", selector.ns, "wait", "--for=condition=Ready", "pod", "-l", selector.sel, "--timeout=90s"); err != nil {
			return false
		}
	}
	return true
}
