// Package oce is the oce plugin: OpenClaw Enterprise (OCE) on Agent
// Substrate. OCE deploys and governs OpenClaw Agents; with this plugin each
// OCE Agent's conversations run as Substrate actors of the openclaw plugin's
// golden template, suspended when idle and resumed on the next message. OCE
// itself is not modified.
//
//   - Deploy: the OCE runtime image with the substrate and
//     oce-substrate-bridge plugins (image.go) in the deployment's ECR; OCE's
//     PostgreSQL, Secrets, prepared bootstrap volume and API relay; OCE's
//     chart at ChartRevision (helm); the gateway admission policy; then one
//     Agent (OCE_AGENT_NAME) made and deployed through OCE's API by a Job in
//     the cluster, and the wiring OCE does not do: its tenant namespace's
//     RoleBindings and labels, the actor token, the tenant NetworkPolicies
//     and trust-bundle read, editor on the atespace for the Agent's
//     ServiceAccount, and its SPIFFE ID in atenet-router's allowlist.
//   - Verify: the Agent's gateway Pod carries the Substrate wiring, and two
//     turns through its HTTP chat API (loopback, OCE's gateway password)
//     run on one conversation actor that remembers the first.
//   - Remove: the Agent wiring, the admission policy, OCE's release and
//     namespaces.
//
// Every Service stays ClusterIP; reach OCE's API with
//
//	kubectl -n openclaw-system port-forward deploy/occ-relay 8080:8080
package oce

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/controlplane"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/efs"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/llmgateway"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/openclaw"
	"github.com/csantanapr/eks-agent-substrate/native/tools/setup-aws/awsload"
)

// ID is the plugin ID.
const ID = "oce"

const (
	// ChartRevision is the OCE commit the chart comes from: the source
	// revision of the published stable controller and runtime images.
	ChartRevision = "54bfa7fc1b99e362450beb5f9211e164991fa7b4"
	chartRepo     = "https://github.com/openclaw/openclaw-enterprise.git"
	// ControllerImage is ghcr.io/openclaw/openclaw-enterprise-controller:stable
	// at ChartRevision, by digest.
	ControllerImage = "ghcr.io/openclaw/openclaw-enterprise-controller@sha256:968ef69a70c0f2ac927e0e7515ba8c07c9887e7b23211351f58bbf706662ffed"
	// Namespace is OCE's control plane namespace, Release its Helm release.
	Namespace = "openclaw-system"
	Release   = "oce"
	// RuntimeRepo is the runtime image's repository under <cluster>/.
	RuntimeRepo = "oce-runtime"
	// DefaultAgent is the Agent's name when OCE_AGENT_NAME is empty.
	DefaultAgent = "substrate-demo"
	// apiService is the OCC API Service the relay and the Job reach.
	apiService = "openclaw-enterprise-api.openclaw-system.svc.cluster.local"
	// tenantLabel marks the namespaces OCE's Kubernetes Compute manages.
	tenantLabel = "openclaw.dev/namespace"
	// enabledLabel opts a tenant namespace into the gateway admission policy.
	enabledLabel = "oce.substrate/enabled"
	actorSecret  = "oce-substrate-actor"
	setupJob     = "oce-agent-setup"
	policyName   = "oce-substrate-gateway"
)

var (
	//go:embed manifests/postgres.yaml
	postgresYAML string
	//go:embed manifests/prepare-bootstrap.yaml
	prepareYAML string
	//go:embed manifests/relay.yaml
	relayYAML string
	//go:embed manifests/values.yaml
	valuesYAML string
	//go:embed manifests/installation.yaml
	installationYAML string
	//go:embed manifests/gateway-policy.yaml
	gatewayPolicyYAML []byte
	//go:embed manifests/tenant.yaml
	tenantYAML string
	//go:embed manifests/agent-setup.yaml
	agentSetupYAML string
	//go:embed check/chat.mjs
	chatScript []byte
)

func init() {
	plugin.Register(plugin.Plugin{
		ID:    ID,
		Title: "OpenClaw Enterprise Agents on Substrate",
		Summary: "OpenClaw Enterprise (OCE) from its published images and chart, and one OCE Agent whose conversations run as " +
			"Substrate actors of the OpenClaw template (oce-substrate-bridge), suspended when idle. Reached with kubectl port-forward only.",
		Order:    50,
		Minutes:  25,
		Requires: []string{openclaw.ID, llmgateway.ID},
		Check:    check,
		Deploy:   deploy,
		Verify:   verify,
		Remove:   remove,
	})
}

var agentNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// agentName is OCE_AGENT_NAME, DefaultAgent when empty.
func agentName(v string) (string, error) {
	if v == "" {
		return DefaultAgent, nil
	}
	if !agentNameRe.MatchString(v) {
		return "", errors.New("OCE_AGENT_NAME must be 1-40 lowercase letters, digits or hyphens")
	}
	return v, nil
}

func check(lookup func(string) (string, bool)) error {
	v, _ := lookup("OCE_AGENT_NAME")
	_, err := agentName(v)
	return err
}

// agentHash is the 12 hex digits OCE's Kubernetes Compute names an Agent's
// objects with: gateway-<hash>, ServiceAccount agent-<hash>.
func agentHash(agentID string) string {
	sum := sha256.Sum256([]byte(agentID))
	return hex.EncodeToString(sum[:])[:12]
}

func spiffeID(tenant, agentID string) string {
	return "spiffe://cluster.local/ns/" + tenant + "/sa/agent-" + agentHash(agentID)
}

func principal(tenant, agentID string) string {
	return "user:system:serviceaccount:" + tenant + ":agent-" + agentHash(agentID)
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func fill(t string, kv ...string) []byte {
	return []byte(strings.NewReplacer(kv...).Replace(t))
}

func kubectl(ctx context.Context, o plugin.Options, args ...string) (string, error) {
	out, err := efs.Out(ctx, o, args...)
	return strings.TrimSpace(out), err
}

func exists(ctx context.Context, o plugin.Options, args ...string) bool {
	out, err := kubectl(ctx, o, append(args, "--ignore-not-found", "-o", "name")...)
	return err == nil && out != ""
}

func awsConfig(ctx context.Context, o plugin.Options) (aws.Config, error) {
	return awsload.Config(ctx, o.Region, o.Profile)
}

// ensureRepo makes <cluster>/RuntimeRepo when it is missing, owner-tagged;
// the core teardown deletes the repositories under <cluster>/.
func ensureRepo(ctx context.Context, w io.Writer, o plugin.Options, ac aws.Config) (string, error) {
	repo := o.Cluster + "/" + RuntimeRepo
	c := ecr.NewFromConfig(ac)
	if _, err := c.DescribeRepositories(ctx, &ecr.DescribeRepositoriesInput{RepositoryNames: []string{repo}}); err == nil {
		return repo, nil
	} else {
		var nf *ecrtypes.RepositoryNotFoundException
		if !errors.As(err, &nf) {
			return "", fmt.Errorf("describe ECR repository %s: %w", repo, err)
		}
	}
	tags, err := o.OwnerTags()
	if err != nil {
		return "", err
	}
	var t []ecrtypes.Tag
	for k, v := range tags {
		t = append(t, ecrtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	if _, err := c.CreateRepository(ctx, &ecr.CreateRepositoryInput{RepositoryName: aws.String(repo), Tags: t,
		ImageScanningConfiguration: &ecrtypes.ImageScanningConfiguration{ScanOnPush: true}}); err != nil {
		return "", fmt.Errorf("create ECR repository %s: %w", repo, err)
	}
	fmt.Fprintf(w, "ECR repository %s created\n", repo)
	return repo, nil
}

// runtimeTag names the runtime image by its inputs: the chart revision and
// the bridge, so a changed bridge is a new tag.
func runtimeTag() string {
	sum := sha256.Sum256(append([]byte(RuntimeImage+PluginSourceImage), bridgeDigest()...))
	return ChartRevision[:7] + "-" + hex.EncodeToString(sum[:])[:10]
}

func bridgeDigest() []byte {
	h := sha256.New()
	_ = walkBridge(func(name string, b []byte) { h.Write([]byte(name)); h.Write(b) })
	return h.Sum(nil)
}

func deploy(ctx context.Context, w io.Writer, o plugin.Options) error {
	name, err := agentName(os.Getenv("OCE_AGENT_NAME"))
	if err != nil {
		return err
	}
	if _, err := kubectl(ctx, o, "-n", openclaw.Namespace, "get", "secret", "openclaw-secrets", "-o", "name"); err != nil {
		return fmt.Errorf("the openclaw plugin's Secret openclaw-secrets is missing: deploy the openclaw plugin first")
	}
	ac, err := awsConfig(ctx, o)
	if err != nil {
		return err
	}
	repo, err := ensureRepo(ctx, w, o, ac)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "OCE runtime image")
	runtime, err := BuildRuntime(ctx, w, ac, repo, runtimeTag())
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "  "+runtime)
	sc := controlplane.StorageClassName(o.AutoMode)
	if err := database(ctx, w, o, sc); err != nil {
		return err
	}
	if err := secrets(ctx, w, o, runtime, sc); err != nil {
		return err
	}
	if err := install(ctx, w, o, sc); err != nil {
		return err
	}
	fmt.Fprintln(w, "Gateway admission policy "+policyName)
	if err := efs.Apply(ctx, w, o, gatewayPolicyYAML); err != nil {
		return err
	}
	tenant, err := tenantNamespace(ctx, o)
	if err != nil {
		return err
	}
	if err := wireTenant(ctx, w, o, tenant); err != nil {
		return err
	}
	agentID, err := setupAgent(ctx, w, o, name)
	if err != nil {
		return err
	}
	return wireAgent(ctx, w, o, tenant, agentID)
}

// database runs OCE's PostgreSQL with the two roles OCE requires, their
// passwords random hex in Secret occ-postgres-roles (made once).
func database(ctx context.Context, w io.Writer, o plugin.Options, sc string) error {
	fmt.Fprintln(w, "PostgreSQL for OCE (namespace "+Namespace+")")
	ns := fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n", Namespace)
	if err := efs.Apply(ctx, w, o, []byte(ns)); err != nil {
		return err
	}
	if !exists(ctx, o, "-n", Namespace, "get", "secret", "occ-postgres-roles") {
		var vals []string
		for range 3 {
			v, err := randomHex(24)
			if err != nil {
				return err
			}
			vals = append(vals, v)
		}
		s := fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata:\n  name: occ-postgres-roles\n  namespace: %s\ntype: Opaque\nstringData:\n  postgres: %q\n  occ_migrator: %q\n  occ_app: %q\n",
			Namespace, vals[0], vals[1], vals[2])
		if err := efs.Apply(ctx, w, o, []byte(s)); err != nil {
			return err
		}
	}
	if err := efs.Apply(ctx, w, o, fill(postgresYAML, "REPLACE_WITH_STORAGE_CLASS", sc)); err != nil {
		return err
	}
	return efs.Run(ctx, w, o, "-n", Namespace, "rollout", "status", "statefulset/occ-postgres", "--timeout=10m")
}

func secretValue(ctx context.Context, o plugin.Options, ns, name, key string) (string, error) {
	out, err := kubectl(ctx, o, "-n", ns, "get", "secret", name, "-o", "jsonpath={.data."+key+"}")
	if err != nil {
		return "", err
	}
	b, err := base64.StdEncoding.DecodeString(out)
	if err != nil || len(b) == 0 {
		return "", fmt.Errorf("secret %s/%s has no %s", ns, name, key)
	}
	return string(b), nil
}

// secrets makes OCE's database URLs, auth secret and Installation startup
// configuration. Values stay in memory and go to kubectl on stdin.
func secrets(ctx context.Context, w io.Writer, o plugin.Options, runtime, sc string) error {
	host := "occ-postgres." + Namespace + ".svc.cluster.local"
	if !exists(ctx, o, "-n", Namespace, "get", "secret", "occ-database") {
		app, err := secretValue(ctx, o, Namespace, "occ-postgres-roles", "occ_app")
		if err != nil {
			return err
		}
		mig, err := secretValue(ctx, o, Namespace, "occ-postgres-roles", "occ_migrator")
		if err != nil {
			return err
		}
		s := fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata:\n  name: occ-database\n  namespace: %s\ntype: Opaque\nstringData:\n  application-url: %q\n  migration-url: %q\n",
			Namespace, "postgresql://occ_app:"+app+"@"+host+":5432/openclaw_enterprise", "postgresql://occ_migrator:"+mig+"@"+host+":5432/openclaw_enterprise")
		if err := efs.Apply(ctx, w, o, []byte(s)); err != nil {
			return err
		}
	}
	if !exists(ctx, o, "-n", Namespace, "get", "secret", "occ-auth") {
		v, err := randomHex(32)
		if err != nil {
			return err
		}
		s := fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata:\n  name: occ-auth\n  namespace: %s\ntype: Opaque\nstringData:\n  secret: %q\n", Namespace, v)
		if err := efs.Apply(ctx, w, o, []byte(s)); err != nil {
			return err
		}
	}
	inst := fill(installationYAML, "REPLACE_WITH_RUNTIME_IMAGE", runtime, "REPLACE_WITH_STORAGE_CLASS", sc, "REPLACE_WITH_CLUSTER", o.Cluster)
	old, _ := secretValue(ctx, o, Namespace, "occ-installation-startup", `installation\.yaml`)
	b, _ := json.Marshal(string(inst))
	s := fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata:\n  name: occ-installation-startup\n  namespace: %s\ntype: Opaque\nstringData:\n  installation.yaml: %s\n", Namespace, b)
	if err := efs.Apply(ctx, w, o, []byte(s)); err != nil {
		return err
	}
	// OCC reads its Installation at startup: a running install restarts to
	// take a changed one (a new runtime image).
	if old != "" && old != string(inst) && exists(ctx, o, "-n", Namespace, "get", "deployment", "openclaw-enterprise-api") {
		fmt.Fprintln(w, "Installation changed: restarting the OCE API and worker")
		for _, d := range []string{"openclaw-enterprise-api", "openclaw-enterprise-worker"} {
			if err := efs.Run(ctx, w, o, "-n", Namespace, "rollout", "restart", "deployment/"+d); err != nil {
				return err
			}
			if err := efs.Run(ctx, w, o, "-n", Namespace, "rollout", "status", "deployment/"+d, "--timeout=10m"); err != nil {
				return err
			}
		}
	}
	return nil
}

// ipBlocks renders /32 NetworkPolicy peers, indented for a "to:" list.
func ipBlocks(ips []string, indent string) string {
	var b strings.Builder
	for _, ip := range ips {
		fmt.Fprintf(&b, "%s- ipBlock: { cidr: %s/32 }\n", indent, ip)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// apiAddresses are the Kubernetes API's Service IP and endpoint addresses,
// which OCE's chart and the tenant policy allow by /32.
func apiAddresses(ctx context.Context, o plugin.Options) ([]string, error) {
	svc, err := kubectl(ctx, o, "get", "svc", "kubernetes", "-n", "default", "-o", "jsonpath={.spec.clusterIP}")
	if err != nil {
		return nil, err
	}
	eps, err := kubectl(ctx, o, "get", "endpointslices", "-n", "default", "-l", "kubernetes.io/service-name=kubernetes",
		"-o", "jsonpath={range .items[*].endpoints[*]}{.addresses[0]} {end}")
	if err != nil {
		return nil, err
	}
	return append([]string{svc}, strings.Fields(eps)...), nil
}

func quoted(ips []string) string {
	var q []string
	for _, ip := range ips {
		q = append(q, strconv.Quote(ip+"/32"))
	}
	return strings.Join(q, ", ")
}

// install prepares the bootstrap volume on a fresh install, runs the relay,
// and installs OCE's chart at ChartRevision with helm.
func install(ctx context.Context, w io.Writer, o plugin.Options, sc string) error {
	fresh := !exists(ctx, o, "-n", Namespace, "get", "deployment", "openclaw-enterprise-api")
	if fresh && !exists(ctx, o, "-n", Namespace, "get", "pvc", "occ-bootstrap") {
		fmt.Fprintln(w, "Bootstrap volume (1000:1000, mode 0700)")
		if err := efs.Apply(ctx, w, o, fill(prepareYAML, "REPLACE_WITH_STORAGE_CLASS", sc, "REPLACE_WITH_CONTROLLER_IMAGE", ControllerImage)); err != nil {
			return err
		}
		if err := efs.Run(ctx, w, o, "-n", Namespace, "wait", "--for=jsonpath={.status.phase}=Succeeded", "pod/occ-bootstrap-prepare", "--timeout=10m"); err != nil {
			return err
		}
		_ = efs.Run(ctx, w, o, "-n", Namespace, "delete", "pod", "occ-bootstrap-prepare", "--ignore-not-found")
	}
	if err := efs.Apply(ctx, w, o, fill(relayYAML, "REPLACE_WITH_CONTROLLER_IMAGE", ControllerImage, "REPLACE_WITH_API_SERVICE", apiService)); err != nil {
		return err
	}
	pg, err := kubectl(ctx, o, "-n", Namespace, "get", "pod", "occ-postgres-0", "-o", "jsonpath={.status.podIP}")
	if err != nil {
		return err
	}
	pgSvc, err := kubectl(ctx, o, "-n", Namespace, "get", "svc", "occ-postgres", "-o", "jsonpath={.spec.clusterIP}")
	if err != nil {
		return err
	}
	api, err := apiAddresses(ctx, o)
	if err != nil {
		return err
	}
	values := fill(valuesYAML, "REPLACE_WITH_CONTROLLER_IMAGE", ControllerImage,
		"REPLACE_WITH_DATABASE_CIDRS", quoted([]string{pg, pgSvc}), "REPLACE_WITH_CLUSTER_CIDRS", quoted(api))
	chart, err := chartDir(ctx, w, o)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "oce-values-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(values); err != nil {
		return err
	}
	f.Close()
	fmt.Fprintf(w, "OCE chart %s at %s (helm release %s)\n", chartRepo, ChartRevision[:12], Release)
	if err := plugin.Exec(ctx, w, plugin.Cmd{Env: []string{"KUBECONFIG=" + o.Kubeconfig}, Argv: []string{"helm", "upgrade", "--install", Release,
		filepath.Join(chart, "deploy", "helm", "openclaw-enterprise"), "-n", Namespace, "-f", f.Name(), "--wait", "--timeout", "15m"}}); err != nil {
		return fmt.Errorf("helm install of OCE: %w", err)
	}
	return efs.Run(ctx, w, o, "-n", Namespace, "rollout", "status", "deployment/openclaw-enterprise-api", "--timeout=10m")
}

// chartDir is OCE's source at ChartRevision, cloned once under the tree cache.
func chartDir(ctx context.Context, w io.Writer, o plugin.Options) (string, error) {
	dir := filepath.Join(o.TreeRoot, "openclaw-enterprise-"+ChartRevision[:12])
	if _, err := os.Stat(filepath.Join(dir, "deploy", "helm", "openclaw-enterprise", "Chart.yaml")); err == nil {
		return dir, nil
	}
	_ = os.RemoveAll(dir)
	if err := plugin.Exec(ctx, w, plugin.Cmd{Argv: []string{"git", "clone", "--quiet", "--filter=blob:none", "--no-checkout", chartRepo, dir}}); err != nil {
		return "", err
	}
	if err := plugin.Exec(ctx, w, plugin.Cmd{Dir: dir, Argv: []string{"git", "checkout", "--quiet", ChartRevision}}); err != nil {
		return "", err
	}
	return dir, nil
}

// tenantNamespace is the Kubernetes namespace of OCE's default Namespace,
// which bootstrap makes; it waits for Compute to create it.
func tenantNamespace(ctx context.Context, o plugin.Options) (string, error) {
	var ns string
	efs.Poll(ctx, 90, func() bool {
		ns, _ = kubectl(ctx, o, "get", "ns", "-l", tenantLabel+",app.kubernetes.io/managed-by=openclaw-enterprise", "-o", "jsonpath={.items[0].metadata.name}")
		return ns != ""
	})
	if ns == "" {
		return "", errors.New("OCE made no tenant namespace (label " + tenantLabel + "): check the worker's logs")
	}
	return ns, nil
}

// wireTenant gives a tenant namespace what OCE leaves to the operator and
// what its Agents need for Substrate.
func wireTenant(ctx context.Context, w io.Writer, o plugin.Options, tenant string) error {
	fmt.Fprintln(w, "Tenant namespace "+tenant)
	var rb strings.Builder
	for _, b := range []struct{ name, role, sa string }{
		{"openclaw-enterprise-worker", "oce-openclaw-tenant-worker", "openclaw-enterprise-worker"},
		{"openclaw-enterprise-api-secrets", "oce-openclaw-tenant-api", "openclaw-enterprise-api"},
		{"openclaw-enterprise-api-configuration", "oce-openclaw-tenant-configuration", "openclaw-enterprise-api"},
	} {
		fmt.Fprintf(&rb, "---\napiVersion: rbac.authorization.k8s.io/v1\nkind: RoleBinding\nmetadata:\n  name: %s\n  namespace: %s\nroleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: ClusterRole\n  name: %s\nsubjects:\n- kind: ServiceAccount\n  name: %s\n  namespace: %s\n",
			b.name, tenant, b.role, b.sa, Namespace)
	}
	if err := efs.Apply(ctx, w, o, []byte(rb.String())); err != nil {
		return err
	}
	if err := efs.Run(ctx, w, o, "label", "ns", tenant, "ate.dev/api-access=true", enabledLabel+"=true", "--overwrite"); err != nil {
		return err
	}
	tok, err := secretValue(ctx, o, openclaw.Namespace, "openclaw-secrets", "gateway-token")
	if err != nil {
		return err
	}
	s := fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\n  namespace: %s\ntype: Opaque\nstringData:\n  actor-token: %q\n", actorSecret, tenant, tok)
	if err := efs.Apply(ctx, w, o, []byte(s)); err != nil {
		return err
	}
	api, err := apiAddresses(ctx, o)
	if err != nil {
		return err
	}
	return efs.Apply(ctx, w, o, fill(tenantYAML, "REPLACE_WITH_TENANT", tenant, "REPLACE_WITH_KUBE_API_BLOCKS", ipBlocks(api, "        ")))
}

var agentLine = regexp.MustCompile(`(?m)^agent (agt_[0-9a-f-]+) `)

// setupAgent runs the agent-setup Job and returns the Agent's ID.
func setupAgent(ctx context.Context, w io.Writer, o plugin.Options, name string) (string, error) {
	fmt.Fprintln(w, "OCE Agent "+name+" (Job "+setupJob+")")
	_ = efs.Run(ctx, w, o, "-n", Namespace, "delete", "job", setupJob, "--ignore-not-found", "--wait=true")
	job := fill(agentSetupYAML, "REPLACE_WITH_CONTROLLER_IMAGE", ControllerImage, "REPLACE_WITH_AGENT_NAME", name,
		"REPLACE_WITH_ATESPACE", openclaw.Atespace, "REPLACE_WITH_TEMPLATE", openclaw.Template)
	if err := efs.Apply(ctx, w, o, job); err != nil {
		return "", err
	}
	waitErr := efs.Run(ctx, w, o, "-n", Namespace, "wait", "--for=condition=complete", "job/"+setupJob, "--timeout=20m")
	logs, _ := kubectl(ctx, o, "-n", Namespace, "logs", "job/"+setupJob)
	fmt.Fprintln(w, logs)
	if waitErr != nil {
		return "", fmt.Errorf("job %s did not complete: %w", setupJob, waitErr)
	}
	m := agentLine.FindStringSubmatch(logs)
	if m == nil {
		return "", fmt.Errorf("job %s printed no Agent ID", setupJob)
	}
	return m[1], nil
}

// wireAgent lets the Agent's gateway act on Substrate: editor on the
// atespace and its SPIFFE ID in atenet-router's allowlist; then it replaces
// a gateway Pod admitted before the wiring.
func wireAgent(ctx context.Context, w io.Writer, o plugin.Options, tenant, agentID string) error {
	k, err := efs.BuildAte(ctx, w, o)
	if err != nil {
		return err
	}
	pol, err := k.Do(ctx, nil, "get", "access-policy", "-a", openclaw.Atespace, "-o", "json")
	if err != nil {
		return fmt.Errorf("read the access policy of %s: %w", openclaw.Atespace, err)
	}
	updated, changed, err := addEditor(pol, principal(tenant, agentID))
	if err != nil {
		return err
	}
	if changed {
		if _, err := k.Do(ctx, updated, "update", "access-policy", "-a", openclaw.Atespace, "-f", "-"); err != nil {
			return fmt.Errorf("update the access policy of %s: %w", openclaw.Atespace, err)
		}
	}
	fmt.Fprintf(w, "Atespace %s: %s is editor\n", openclaw.Atespace, principal(tenant, agentID))
	if err := allowRouter(ctx, w, o, spiffeID(tenant, agentID)); err != nil {
		return err
	}
	sel := "openclaw.dev/workload-role=gateway,openclaw.dev/agent=" + agentID
	pods, _ := kubectl(ctx, o, "-n", tenant, "get", "pods", "-l", sel, "-o", "jsonpath={range .items[*]}{.metadata.name} {.spec.volumes[*].name}{\"\\n\"}{end}")
	for _, line := range strings.Split(pods, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && !slices.Contains(f[1:], "kube-api") {
			fmt.Fprintln(w, "Replacing gateway Pod "+f[0]+" (admitted before the Substrate wiring)")
			_ = efs.Run(ctx, w, o, "-n", tenant, "delete", "pod", f[0], "--wait=true")
		}
	}
	return efs.Run(ctx, w, o, "-n", tenant, "wait", "--for=condition=Ready", "pod", "-l", sel, "--timeout=10m")
}

// addEditor adds member to the editor binding of an access policy
// (kubectl-ate's JSON), making the binding when there is none.
func addEditor(policy []byte, member string) ([]byte, bool, error) {
	var p map[string]any
	if err := json.Unmarshal(policy, &p); err != nil {
		return nil, false, fmt.Errorf("read the access policy: %w", err)
	}
	bindings, _ := p["bindings"].([]any)
	for _, b := range bindings {
		m, _ := b.(map[string]any)
		if m["role"] != "editor" {
			continue
		}
		members, _ := m["members"].([]any)
		for _, x := range members {
			if x == member {
				return policy, false, nil
			}
		}
		m["members"] = append(members, member)
		out, err := json.Marshal(p)
		return out, true, err
	}
	p["bindings"] = append(bindings, map[string]any{"role": "editor", "members": []any{member}})
	out, err := json.Marshal(p)
	return out, true, err
}

const routerFlag = "--ingress-allowed-spiffe-ids="

// withID returns the router argument with id appended, or "" when it has it.
func withID(arg, id string) string {
	ids := strings.Split(strings.TrimPrefix(arg, routerFlag), ",")
	if slices.Contains(ids, id) {
		return ""
	}
	return arg + "," + id
}

// allowRouter adds id to atenet-router's --ingress-allowed-spiffe-ids.
// shortcut: a later --deploy-control-plane renders the flag from the
// plugins' static needs and drops it; deploy this plugin again after one.
func allowRouter(ctx context.Context, w io.Writer, o plugin.Options, id string) error {
	out, err := kubectl(ctx, o, "-n", "ate-system", "get", "deployment", "atenet-router", "-o", "json")
	if err != nil {
		return err
	}
	var d struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Args []string `json:"args"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		return err
	}
	for ci, c := range d.Spec.Template.Spec.Containers {
		for ai, a := range c.Args {
			if !strings.HasPrefix(a, routerFlag) {
				continue
			}
			next := withID(a, id)
			if next == "" {
				fmt.Fprintln(w, "atenet-router already admits "+id)
				return nil
			}
			patch, _ := json.Marshal([]map[string]any{{"op": "replace", "path": fmt.Sprintf("/spec/template/spec/containers/%d/args/%d", ci, ai), "value": next}})
			if err := efs.Run(ctx, w, o, "-n", "ate-system", "patch", "deployment", "atenet-router", "--type=json", "-p", string(patch)); err != nil {
				return err
			}
			fmt.Fprintln(w, "atenet-router admits "+id)
			return efs.Run(ctx, w, o, "-n", "ate-system", "rollout", "status", "deployment/atenet-router", "--timeout=5m")
		}
	}
	return errors.New("atenet-router has no " + routerFlag + " argument")
}

// agentID reads the Agent's ID off its gateway Deployment in tenant.
func agentID(ctx context.Context, o plugin.Options, tenant string) (string, error) {
	id, err := kubectl(ctx, o, "-n", tenant, "get", "deployments", "-l", "openclaw.dev/workload-role=gateway",
		"-o", `jsonpath={.items[0].metadata.labels.openclaw\.dev/agent}`)
	if err != nil || id == "" {
		return "", fmt.Errorf("no OCE Agent gateway in %s: deploy the oce plugin", tenant)
	}
	return id, nil
}

var httpReply = regexp.MustCompile(`(?m)^HTTP (\d+) .*?: (.*)$`)

// verify checks the gateway Pod's wiring, then sends two turns through the
// Agent's HTTP chat API on one conversation: the second must recall the first.
func verify(ctx context.Context, w io.Writer, o plugin.Options) error {
	tenant, err := tenantNamespace(ctx, o)
	if err != nil {
		return err
	}
	id, err := agentID(ctx, o, tenant)
	if err != nil {
		return err
	}
	sel := "openclaw.dev/workload-role=gateway,openclaw.dev/agent=" + id
	pod, err := kubectl(ctx, o, "-n", tenant, "get", "pods", "-l", sel, "-o", "jsonpath={.items[0].metadata.name}")
	if err != nil || pod == "" {
		return fmt.Errorf("no gateway Pod for Agent %s in %s", id, tenant)
	}
	vols, _ := kubectl(ctx, o, "-n", tenant, "get", "pod", pod, "-o", "jsonpath={.spec.volumes[*].name}")
	for _, v := range []string{"ate-api", "router-client", "kube-api"} {
		if !slices.Contains(strings.Fields(vols), v) {
			return fmt.Errorf("gateway Pod %s lacks volume %s: the admission policy %s did not admit it", pod, v, policyName)
		}
	}
	fmt.Fprintln(w, "PASS: gateway Pod "+pod+" carries the Substrate token, router certificate and Kubernetes token")
	word, err := randomHex(4)
	if err != nil {
		return err
	}
	word = "cw" + word
	conv := "oce-verify-" + word
	out, err := kubectl(ctx, o, "-n", tenant, "exec", pod, "-c", "gateway", "--", "node", "--input-type=module", "-e", string(chatScript),
		conv, "Remember the codeword "+word+". Reply only with OK.", "What was the codeword? Reply with just the word.")
	fmt.Fprint(w, out)
	if err != nil {
		return fmt.Errorf("the chat turns failed: %w", err)
	}
	replies := httpReply.FindAllStringSubmatch(out, -1)
	if len(replies) != 2 || replies[0][1] != "200" || replies[1][1] != "200" {
		return errors.New("the Agent's HTTP chat API did not answer both turns with 200")
	}
	if !strings.Contains(strings.ToLower(replies[1][2]), word) {
		return fmt.Errorf("the second turn did not recall the codeword %s: %s", word, replies[1][2])
	}
	fmt.Fprintf(w, "PASS: two turns through Agent %s's HTTP chat API ran on one Substrate actor, which recalled the codeword\n", id)
	return nil
}

// remove takes down the Agent wiring, the admission policy and OCE.
func remove(ctx context.Context, w io.Writer, o plugin.Options) error {
	tenants, _ := kubectl(ctx, o, "get", "ns", "-l", tenantLabel+",app.kubernetes.io/managed-by=openclaw-enterprise", "-o", "jsonpath={.items[*].metadata.name}")
	for _, t := range strings.Fields(tenants) {
		_ = efs.Run(ctx, w, o, "delete", "clusterrolebinding", "oce-substrate-trust-bundles-"+t, "--ignore-not-found")
		_ = efs.Run(ctx, w, o, "-n", "llm-gateway", "delete", "networkpolicy", "llm-gateway-from-oce-"+t, "--ignore-not-found")
	}
	_ = efs.Run(ctx, w, o, "delete", "mutatingadmissionpolicybinding", policyName, "--ignore-not-found")
	_ = efs.Run(ctx, w, o, "delete", "mutatingadmissionpolicy", policyName, "--ignore-not-found")
	_ = plugin.Exec(ctx, w, plugin.Cmd{Env: []string{"KUBECONFIG=" + o.Kubeconfig}, Argv: []string{"helm", "uninstall", Release, "-n", Namespace, "--ignore-not-found", "--wait"}})
	for _, t := range strings.Fields(tenants) {
		_ = efs.Run(ctx, w, o, "delete", "ns", t, "--ignore-not-found", "--wait=false")
	}
	return efs.Run(ctx, w, o, "delete", "ns", Namespace, "--ignore-not-found", "--wait=true", "--timeout=10m")
}
