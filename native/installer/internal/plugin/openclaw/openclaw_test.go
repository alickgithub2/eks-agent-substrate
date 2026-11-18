package openclaw

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/kustomize/kyaml/yaml"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin/efs"
	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/state"
)

const digest = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// Overrides in the tests: the published repositories with another digest.
const (
	gatewayRepo   = "ghcr.io/csantanapr/always-on-agent/openclaw-gateway"
	actorRepo     = "ghcr.io/csantanapr/always-on-agent/openclaw-actor"
	dashboardRepo = "ghcr.io/csantanapr/always-on-agent/openclaw-dashboard"
)

func testImages() images {
	return images{gateway: gatewayRepo + digest, actor: actorRepo + digest, dashboard: dashboardRepo + digest,
		worker: "ghcr.io/agent-substrate/substrate/ateworker-gvisor:v1.0.0" + digest}
}

// The defaults are the published images, each pinned by digest, all from
// one build (the eks-5bf95fa9017b tag).
func TestDefaultImagesArePinned(t *testing.T) {
	for repo, ref := range map[string]string{gatewayRepo: GatewayImage, actorRepo: ActorImage, dashboardRepo: DashboardImage} {
		if !digestRef.MatchString(ref) || !strings.HasPrefix(ref, repo+":eks-5bf95fa9017b@sha256:") {
			t.Errorf("default %s is not %s:eks-5bf95fa9017b pinned by digest", ref, repo)
		}
	}
}

func TestImageMustBePinned(t *testing.T) {
	// Unset: the pinned default.
	t.Setenv("OPENCLAW_GATEWAY_IMAGE", "")
	if ref, err := image(os.LookupEnv, "OPENCLAW_GATEWAY_IMAGE", GatewayImage); err != nil || ref != GatewayImage {
		t.Errorf("the default: %q, %v", ref, err)
	}
	// An override without a digest is refused, even over a pinned default.
	t.Setenv("OPENCLAW_GATEWAY_IMAGE", gatewayRepo+":v1")
	if _, err := image(os.LookupEnv, "OPENCLAW_GATEWAY_IMAGE", GatewayImage); err == nil || !strings.Contains(err.Error(), "OPENCLAW_GATEWAY_IMAGE=<image>@sha256:") {
		t.Errorf("an override pinned by tag only: %v", err)
	}
	// A pinned override wins.
	t.Setenv("OPENCLAW_GATEWAY_IMAGE", gatewayRepo+":v1"+digest)
	if ref, err := image(os.LookupEnv, "OPENCLAW_GATEWAY_IMAGE", GatewayImage); err != nil || ref != gatewayRepo+":v1"+digest {
		t.Errorf("a pinned override: %q, %v", ref, err)
	}
	// A malformed ref is refused though it ends in a digest, so it never
	// reaches the cluster; an upper-case tag, which OCI allows, is accepted
	// (review of 86ba13f, finding 1).
	for _, bad := range []string{gatewayRepo + ":v1:v2" + digest, ":" + digest} {
		t.Setenv("OPENCLAW_GATEWAY_IMAGE", bad)
		if _, err := image(os.LookupEnv, "OPENCLAW_GATEWAY_IMAGE", GatewayImage); err == nil || !strings.Contains(err.Error(), "OPENCLAW_GATEWAY_IMAGE=<image>@sha256:") {
			t.Errorf("a malformed override %s: %v", bad, err)
		}
	}
	t.Setenv("OPENCLAW_GATEWAY_IMAGE", gatewayRepo+":V1"+digest)
	if ref, err := image(os.LookupEnv, "OPENCLAW_GATEWAY_IMAGE", GatewayImage); err != nil || ref != gatewayRepo+":V1"+digest {
		t.Errorf("an upper-case tag: %q, %v", ref, err)
	}
	// An unpinned default is refused too (a later edit of the constants).
	t.Setenv("OPENCLAW_GATEWAY_IMAGE", "")
	if _, err := image(os.LookupEnv, "OPENCLAW_GATEWAY_IMAGE", gatewayRepo); err == nil {
		t.Error("accepted an unpinned default")
	}
}

func TestRenderAll(t *testing.T) {
	wa, _ := whatsappFor("")
	m, err := renderAll(testImages(), "tok-0123456789abcdef", "3", wa, SandboxGVisor)
	if err != nil {
		t.Fatal(err)
	}
	all := string(m.workerPool) + string(m.gatewayConfig) + string(m.gateway) + string(m.dashboard) + string(m.actorTemplate)
	if strings.Contains(all, "REPLACE_WITH_") {
		t.Error("a placeholder is left")
	}
	for name, c := range map[string]struct {
		got  []byte
		want []string
	}{
		"workerpool":     {m.workerPool, []string{"replicas: 3", "  workerImage: ghcr.io/agent-substrate/substrate/ateworker-gvisor:v1.0.0" + digest}},
		"gateway":        {m.gateway, []string{"image: " + gatewayRepo + digest, "storageClassName: " + StorageClass, m.configSHA, `value: "false"`, "type: ClusterIP"}},
		"dashboard":      {m.dashboard, []string{"image: " + dashboardRepo + digest, "type: ClusterIP"}},
		"actor template": {m.actorTemplate, []string{"image: " + actorRepo + digest, `value: "tok-0123456789abcdef"`}},
		"config":         {m.gatewayConfig, []string{`"allow": ["substrate"]`, `"bindings": []`, `"egressPolicy": {"rules":[{"http":`}},
	} {
		for _, w := range c.want {
			if !strings.Contains(string(c.got), w) {
				t.Errorf("%s lacks %q", name, w)
			}
		}
	}
	for _, never := range []string{"type: LoadBalancer", "type: NodePort", "kind: Ingress"} {
		if strings.Contains(all, never) {
			t.Errorf("rendered %s", never)
		}
	}
	// WhatsApp binds one number, and the gateway installs its plugin.
	wa, err = whatsappFor("+15555550100")
	if err != nil {
		t.Fatal(err)
	}
	m, err = renderAll(testImages(), "tok-0123456789abcdef", "3", wa, SandboxGVisor)
	if err != nil || !strings.Contains(string(m.gatewayConfig), `"id":"+15555550100"`) || !strings.Contains(string(m.gateway), `value: "true"`) {
		t.Errorf("WhatsApp: %v", err)
	}
	if _, err := whatsappFor("5555"); err == nil {
		t.Error("accepted a peer that is not E.164")
	}
}

func TestRenderRefusesAPlaceholderLeft(t *testing.T) {
	if _, err := render("image: REPLACE_WITH_NEW_IMAGE@x\n", strings.NewReplacer()); err == nil || !strings.Contains(err.Error(), "REPLACE_WITH_NEW_IMAGE") {
		t.Errorf("got %v", err)
	}
	if err := validConfig([]byte("apiVersion: v1\nkind: ConfigMap\ndata:\n  openclaw.json: |\n    {\"a\": [1,}\n")); err == nil {
		t.Error("accepted a config that is not JSON")
	}
}

// authTokenProblem says what is wrong with the gateway's auth in a rendered
// gateway ConfigMap, or "" when gateway.auth.token is the env SecretRef
// ${OPENCLAW_GATEWAY_TOKEN} and the token value tok is nowhere in it.
func authTokenProblem(cm []byte, tok string) string {
	n, err := yaml.Parse(string(cm))
	if err != nil {
		return err.Error()
	}
	var c struct {
		Gateway struct{ Auth struct{ Mode, Token string } }
	}
	if err := json.Unmarshal([]byte(n.GetDataMap()["openclaw.json"]), &c); err != nil {
		return err.Error()
	}
	switch {
	case c.Gateway.Auth.Mode != "token":
		return fmt.Sprintf("gateway.auth.mode is %q", c.Gateway.Auth.Mode)
	case c.Gateway.Auth.Token != "${OPENCLAW_GATEWAY_TOKEN}":
		return fmt.Sprintf("gateway.auth.token is %q", c.Gateway.Auth.Token)
	case strings.Contains(string(cm), tok):
		return "the token value is in the ConfigMap"
	}
	return ""
}

// T27: the gateway's HTTP API and Control UI accept the openclaw-secrets
// token only when gateway.auth.token resolves to it from the environment.
// With no token there, the init container's `doctor --fix` writes a random
// one into the state config, and the gateway prefers it. The check is proved
// both ways on fixtures first.
func TestGatewayAuthTokenIsTheEnvSecretRef(t *testing.T) {
	const tok = "tok-0123456789abcdef"
	cm := func(gateway string) []byte {
		return []byte("apiVersion: v1\nkind: ConfigMap\ndata:\n  openclaw.json: |\n    {\"gateway\": " + gateway + "}\n")
	}
	for gateway, want := range map[string]string{
		`{"auth": {"mode": "token"}}`:                                                           "gateway.auth.token",
		`{"auth": {"mode": "token", "token": "` + tok + `"}}`:                                   "gateway.auth.token",
		`{"auth": {"mode": "none", "token": "${OPENCLAW_GATEWAY_TOKEN}"}}`:                      "gateway.auth.mode",
		`{"auth": {"mode": "token", "token": "${OPENCLAW_GATEWAY_TOKEN}"}, "x": "` + tok + `"}`: "the token value",
	} {
		if p := authTokenProblem(cm(gateway), tok); !strings.HasPrefix(p, want) {
			t.Errorf("the check on %s: got %q, want %q", gateway, p, want)
		}
	}
	if p := authTokenProblem(cm(`{"auth": {"mode": "token", "token": "${OPENCLAW_GATEWAY_TOKEN}"}}`), tok); p != "" {
		t.Fatalf("the check refused a correct config: %s", p)
	}
	for _, peer := range []string{"", "+15555550100"} {
		wa, err := whatsappFor(peer)
		if err != nil {
			t.Fatal(err)
		}
		m, err := renderAll(testImages(), tok, "3", wa, SandboxGVisor)
		if err != nil {
			t.Fatal(err)
		}
		if p := authTokenProblem(m.gatewayConfig, tok); p != "" {
			t.Errorf("WHATSAPP_PEER=%q: %s", peer, p)
		}
	}
}

func TestGrantEditor(t *testing.T) {
	p := []byte(`{"metadata":{"name":"default","uid":"u"},"bindings":[{"role":"viewer","members":["user:x"]}]}`)
	out, changed, err := grantEditor(p, gatewayPrincipal)
	if err != nil || !changed || !strings.Contains(string(out), `{"members":["`+gatewayPrincipal+`"],"role":"editor"}`) || !strings.Contains(string(out), `"uid":"u"`) {
		t.Errorf("no editor binding: %s %v %v", out, changed, err)
	}
	p = []byte(`{"bindings":[{"role":"editor","members":["user:x"]}]}`)
	if out, changed, _ = grantEditor(p, gatewayPrincipal); !changed || !strings.Contains(string(out), `"members":["user:x","`+gatewayPrincipal+`"]`) {
		t.Errorf("an editor binding: %s", out)
	}
	p = []byte(`{"bindings":[{"role":"owner","members":["` + gatewayPrincipal + `"]}]}`)
	if _, changed, _ = grantEditor(p, gatewayPrincipal); changed {
		t.Error("changed a policy that already grants it")
	}
}

func TestKeepTemplate(t *testing.T) {
	img := actorRepo + digest
	tmpl := func(image, tok, gerr string) []byte {
		return []byte(fmt.Sprintf(`{"containers":[{"image":%q,"env":[{"name":"OPENCLAW_GATEWAY_TOKEN","value":%q}]}],"status":{"goldenSnapshotStatus":{"errorMessage":%q}}}`, image, tok, gerr))
	}
	if keep, why := keepTemplate(tmpl(img, "tok", ""), img, "tok"); !keep {
		t.Errorf("the same template: %s", why)
	}
	// The same image spelled with and without a tag keeps the template, its
	// golden snapshot and its actors (review of 86ba13f, finding 4).
	if keep, why := keepTemplate(tmpl(img, "tok", ""), actorRepo+":eks-5bf95fa9017b"+digest, "tok"); !keep {
		t.Errorf("the same digest, tagged: %s", why)
	}
	for why, t2 := range map[string][]byte{
		"another image": tmpl(actorRepo+"@sha256:"+strings.Repeat("f", 64), "tok", ""), "another gateway token": tmpl(img, "old", ""),
		"its golden snapshot failed": tmpl(img, "tok", "boom"),
	} {
		if keep, got := keepTemplate(t2, img, "tok"); keep || got != why {
			t.Errorf("%s: keep=%v why=%q", why, keep, got)
		}
	}
}

// sameImage is one repository and digest, a tag or none, and nothing else.
func TestSameImage(t *testing.T) {
	other := "@sha256:" + strings.Repeat("f", 64)
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{actorRepo + digest, actorRepo + ":v1" + digest, true},
		{actorRepo + ":v1" + digest, actorRepo + ":v2" + digest, true},
		{"nginx" + digest, "index.docker.io/library/nginx" + digest, true},
		{actorRepo + digest, actorRepo + other, false},
		{actorRepo + digest, gatewayRepo + digest, false},
		{actorRepo + ":v1", actorRepo + ":v1", false}, // not pinned: no digest to compare
	} {
		if got := sameImage(c.a, c.b); got != c.want {
			t.Errorf("sameImage(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

// Verify accepts a deployment that runs the pinned image spelled another
// way (with a tag where the setting has none), and still refuses another
// digest (review of 86ba13f, finding 4).
func TestVerifyComparesImagesByDigest(t *testing.T) {
	o := plugin.Options{Cluster: "demo", Region: "us-west-2", Kubeconfig: "/kc"}
	kc := "kubectl --kubeconfig /kc "
	for _, c := range []struct {
		running string
		pass    bool
	}{{gatewayRepo + ":eks-5bf95fa9017b" + digest, true}, {gatewayRepo + "@sha256:" + strings.Repeat("f", 64), false}} {
		t.Setenv("OPENCLAW_GATEWAY_IMAGE", gatewayRepo+digest)
		t.Setenv("OPENCLAW_DASHBOARD_IMAGE", dashboardRepo+digest)
		t.Setenv("OPENCLAW_ACTOR_IMAGE", "")
		fakeExec(t, map[string]string{
			kc + "-n ate-system get deployment atenet-router":                GatewayID + " " + DashboardID,
			kc + "-n openclaw get services":                                  `{"items":[{"metadata":{"name":"s0"},"spec":{"type":"ClusterIP"}}]}`,
			kc + "-n openclaw get deployment openclaw-gateway -o jsonpath":   c.running,
			kc + "-n openclaw get deployment openclaw-dashboard -o jsonpath": dashboardRepo + ":v1" + digest,
		})
		var out strings.Builder
		err := verify(context.Background(), &out, o)
		if passed := strings.Contains(out.String(), "PASS: the gateway and dashboard run the pinned images"); passed != c.pass {
			t.Errorf("gateway runs %s: passed the image check %v, want %v: %v\n%s", c.running, passed, c.pass, err, out.String())
		}
		// Refused by the image check itself, not by an earlier one.
		if !c.pass && (err == nil || !strings.Contains(err.Error(), "not the pinned "+gatewayRepo+digest)) {
			t.Errorf("gateway runs %s: %v", c.running, err)
		}
	}
}

// failAnswer, as fakeExec's answer to an argv prefix, makes that child fail
// with errKubectl and print nothing.
const failAnswer = "\x00fail"

var errKubectl = errors.New("the server is currently unable to handle the request")

// fakeExec answers kubectl reads by argv prefix and fails on a shell.
func fakeExec(t *testing.T, answers map[string]string) *[]string {
	var log []string
	prev, prevWait := plugin.Exec, efs.Wait
	efs.Wait = 0
	plugin.Exec = func(_ context.Context, w io.Writer, c plugin.Cmd) error {
		argv := strings.Join(c.Argv, " ")
		for _, a := range c.Argv {
			if a == "bash" || a == "sh" || a == "-c" {
				return fmt.Errorf("a shell ran: %s", argv)
			}
		}
		log = append(log, argv)
		for p, a := range answers {
			if a == failAnswer && strings.HasPrefix(argv, p) {
				return errKubectl
			}
		}
		for p, a := range answers {
			if strings.HasPrefix(argv, p) && c.Stdout != nil {
				io.WriteString(c.Stdout, a)
			}
		}
		return nil
	}
	t.Cleanup(func() { plugin.Exec, efs.Wait = prev, prevWait })
	return &log
}

func TestExposure(t *testing.T) {
	o := plugin.Options{Cluster: "demo", Region: "us-west-2", Kubeconfig: "/kc"}
	svc := func(types ...string) string {
		var items []string
		for i, ty := range types {
			items = append(items, fmt.Sprintf(`{"metadata":{"name":"s%d"},"spec":{"type":%q}}`, i, ty))
		}
		return `{"items":[` + strings.Join(items, ",") + `]}`
	}
	fakeExec(t, map[string]string{"kubectl --kubeconfig /kc -n openclaw get services": svc("ClusterIP", "ClusterIP")})
	if err := exposure(context.Background(), o); err != nil {
		t.Errorf("ClusterIP only: %v", err)
	}
	for _, ty := range []string{"NodePort", "LoadBalancer"} {
		fakeExec(t, map[string]string{"kubectl --kubeconfig /kc -n openclaw get services": svc("ClusterIP", ty)})
		if err := exposure(context.Background(), o); err == nil || !strings.Contains(err.Error(), ty) {
			t.Errorf("%s: %v", ty, err)
		}
	}
	fakeExec(t, map[string]string{"kubectl --kubeconfig /kc -n openclaw get services": svc("ClusterIP"),
		"kubectl --kubeconfig /kc -n openclaw get ingress": "ingress.networking.k8s.io/open\n"})
	if err := exposure(context.Background(), o); err == nil || !strings.Contains(err.Error(), "Ingress") {
		t.Errorf("an Ingress: %v", err)
	}
}

// A whole deploy against a cluster that already holds the gateway token
// ends by saying how to read that token for the gateway UI, which asks for
// it, and prints the token itself nowhere (review of 86ba13f, finding 2).
func TestDeploySaysHowToReadTheGatewayToken(t *testing.T) {
	out, _, err := fakeDeploy(t, nil)
	if err != nil {
		t.Fatalf("deploy: %v\n%s", err, out)
	}
	want := "  its token:   kubectl --kubeconfig /kc -n openclaw get secret openclaw-secrets -o jsonpath='{.data.gateway-token}' | base64 -d\n"
	if !strings.Contains(out, want) {
		t.Errorf("the output does not say how to read the token, %q:\n%s", want, out)
	}
	if strings.Contains(out, deployTok) {
		t.Errorf("the output holds the gateway token:\n%s", out)
	}
}

// deployTok is the gateway token fakeDeploy's cluster already holds.
const deployTok = "tok-0123456789abcdef"

// fakeDeploy runs a whole deploy against a fake cluster that already holds
// deployTok, with extra answers added, and returns what it printed and what
// it gave a child on stdin.
func fakeDeploy(t *testing.T, extra map[string]string) (string, []string, error) {
	t.Helper()
	const tok = deployTok
	for _, k := range append(slices.Clone(imageVars), "WHATSAPP_PEER", "OPENCLAW_WORKERS") {
		t.Setenv(k, "")
	}
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prevPin := pinImage
	pinImage = func(_ context.Context, tagged string) (string, error) { return tagged + digest, nil }
	t.Cleanup(func() { pinImage = prevPin })
	o := plugin.Options{Cluster: "demo", Region: "us-west-2", Kubeconfig: "/kc",
		Release: plugin.Release{Tag: "v1.0.0", ImageRepo: "ghcr.io/agent-substrate/substrate", Tree: tree}}
	kc, ate := "kubectl --kubeconfig /kc ", efs.KubectlAtePath(o.Tag)+" "
	answers := map[string]string{
		kc + "-n ate-system get deployment ate-api-server":                              "deployment.apps/ate-api-server",
		kc + "get storageclass " + efs.StorageClass + " --ignore-not-found -o jsonpath": "fs-0123",
		kc + "-n openclaw get secret openclaw-secrets":                                  base64.StdEncoding.EncodeToString([]byte(tok)),
		kc + "-n openclaw get workerpool openclaw":                                      "3",
		kc + "-n openclaw get pvc openclaw-gateway-pvc -o jsonpath":                     "Bound efs-openclaw pv-1",
		kc + "get pv pv-1":        "efs.csi.aws.com fs-0123::fsap-0abc",
		ate + "get access-policy": `{"bindings":[{"role":"editor","members":["` + gatewayPrincipal + `"]}]}`,
		ate + "get actor-template": fmt.Sprintf(`{"containers":[{"image":%q,"env":[{"name":"OPENCLAW_GATEWAY_TOKEN","value":%q}]}],`+
			`"status":{"goldenSnapshotStatus":{"goldenTag":{"name":"g1"}}}}`, ActorImage, tok),
	}
	maps.Copy(answers, extra)
	fakeExec(t, answers)
	var stdins []string
	inner := plugin.Exec
	plugin.Exec = func(ctx context.Context, w io.Writer, c plugin.Cmd) error {
		if c.Stdin != nil {
			stdins = append(stdins, string(c.Stdin))
		}
		return inner(ctx, w, c)
	}
	t.Cleanup(func() { plugin.Exec = inner })
	var out strings.Builder
	err := deploy(context.Background(), &out, o)
	return out.String(), stdins, err
}

// recordExec records every child and fails each one, so a hook stops at
// its first child: what it ran shows whether it got past the image check.
func recordExec(t *testing.T) *[]string {
	var log []string
	prev := plugin.Exec
	plugin.Exec = func(_ context.Context, _ io.Writer, c plugin.Cmd) error {
		log = append(log, strings.Join(c.Argv, " "))
		return fmt.Errorf("stop")
	}
	t.Cleanup(func() { plugin.Exec = prev })
	return &log
}

var imageVars = []string{"OPENCLAW_GATEWAY_IMAGE", "OPENCLAW_ACTOR_IMAGE", "OPENCLAW_DASHBOARD_IMAGE"}

// Overrides without a digest make Deploy and Verify refuse, naming every
// such variable, and start no child: the cluster is never reached.
func TestHooksRefuseUnpinnedOverridesBeforeTheCluster(t *testing.T) {
	t.Setenv("OPENCLAW_GATEWAY_IMAGE", gatewayRepo+":v1")
	t.Setenv("OPENCLAW_ACTOR_IMAGE", actorRepo+":v1")
	t.Setenv("OPENCLAW_DASHBOARD_IMAGE", dashboardRepo)
	o := plugin.Options{Cluster: "demo", Region: "us-west-2", Kubeconfig: "/kc", Release: plugin.Release{Tag: "v1.0.0", ImageRepo: "ghcr.io/agent-substrate/substrate"}}
	for name, hook := range map[string]func(context.Context, io.Writer, plugin.Options) error{"deploy": deploy, "verify": verify} {
		log := recordExec(t)
		err := hook(context.Background(), io.Discard, o)
		if err == nil || !strings.Contains(err.Error(), "set OPENCLAW_GATEWAY_IMAGE, OPENCLAW_ACTOR_IMAGE, OPENCLAW_DASHBOARD_IMAGE to <image>@sha256:<digest>") {
			t.Errorf("%s: %v", name, err)
		}
		if len(*log) > 0 {
			t.Errorf("%s ran %v before refusing", name, *log)
		}
	}
	// One bad override is named alone; the other two take their defaults.
	t.Setenv("OPENCLAW_GATEWAY_IMAGE", "")
	t.Setenv("OPENCLAW_DASHBOARD_IMAGE", "")
	if _, err := pinnedImages(os.LookupEnv); err == nil || !strings.Contains(err.Error(), "set OPENCLAW_ACTOR_IMAGE to") {
		t.Errorf("one unpinned: %v", err)
	}
}

// Through plugin.Run, as --plugin-action runs it: a bad override stops a
// deploy or verify of openclaw before any child starts, so neither the
// record nor efs, which openclaw requires and which goes first (its
// setup-aws, Deploy and Verify), is touched. With the pinned defaults Run
// goes on to the cluster (review of 86ba13f, finding 3).
func TestRunRefusesABadOverrideBeforeAnyChild(t *testing.T) {
	o, first := runOptions(t)
	for _, action := range []string{plugin.ActionDeploy, plugin.ActionVerify} {
		t.Setenv("OPENCLAW_ACTOR_IMAGE", actorRepo+":v1:v2"+digest)
		log := recordExec(t)
		err := plugin.Run(context.Background(), io.Discard, action, []string{ID}, o)
		if err == nil || !strings.Contains(err.Error(), "plugin openclaw: ") || !strings.Contains(err.Error(), "set OPENCLAW_ACTOR_IMAGE to") {
			t.Errorf("%s, a bad override: %v", action, err)
		}
		if len(*log) > 0 {
			t.Errorf("%s, a bad override: started %q before refusing", action, *log)
		}
		t.Setenv("OPENCLAW_ACTOR_IMAGE", "")
		log = recordExec(t)
		_ = plugin.Run(context.Background(), io.Discard, action, []string{ID}, o)
		if len(*log) == 0 || !strings.HasPrefix((*log)[0], first[action]) {
			t.Errorf("%s, the pinned defaults: first child %q, want %s", action, *log, first[action])
		}
	}
}

// runOptions are Options for plugin.Run with a kubectl-ate tree, every
// openclaw setting empty, and what each action starts first once it is past
// the settings check: deploy reads the record, verify builds kubectl-ate for
// efs's Verify.
func runOptions(t *testing.T) (plugin.Options, map[string]string) {
	t.Helper()
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, k := range settingVars {
		t.Setenv(k, "")
	}
	// The nodes openclaw (gvisor) and efs (system) run on.
	return plugin.Options{Cluster: "demo", Region: "us-west-2", Kubeconfig: "/kc", SetupAWS: "/setup-aws", Pools: []string{"gvisor", "system"},
			Release: plugin.Release{Tag: "v1.0.0", ImageRepo: "ghcr.io/agent-substrate/substrate", Tree: tree}},
		map[string]string{plugin.ActionDeploy: "kubectl --kubeconfig /kc -n kube-system get configmap substrate-eks-plugins",
			plugin.ActionVerify: "go build -o "}
}

var settingVars = append(slices.Clone(imageVars), "WHATSAPP_PEER", "OPENCLAW_WORKERS")

// The settings openclaw's Check covers besides the images: each malformed
// value with the variable it names, and values that are fine. Each bad
// value is distinctive, so finding it in an error means it was printed.
var (
	badSettings = []struct{ key, value string }{
		{"WHATSAPP_PEER", "15555550123"}, {"WHATSAPP_PEER", "+1234"}, {"OPENCLAW_WORKERS", "-7"}, {"OPENCLAW_WORKERS", "seven"},
	}
	goodSettings = []struct{ key, value string }{
		{"WHATSAPP_PEER", "+15555550123"}, {"OPENCLAW_WORKERS", "5"}, {"WHATSAPP_PEER", ""}, {"OPENCLAW_WORKERS", ""},
	}
)

// openclaw's Check covers every setting Deploy reads, so a malformed
// WHATSAPP_PEER or OPENCLAW_WORKERS stops a run before any child, as a bad
// image does. The refusal names the variable but never prints the value:
// WHATSAPP_PEER is a phone number, and Review shows Check errors. Valid and
// empty values go on (review of deb95a7, RISK A).
func TestRunRefusesABadPeerOrWorkersBeforeAnyChild(t *testing.T) {
	o, first := runOptions(t)
	for _, action := range []string{plugin.ActionDeploy, plugin.ActionVerify} {
		for _, s := range badSettings {
			t.Setenv(s.key, s.value)
			log := recordExec(t)
			err := plugin.Run(context.Background(), io.Discard, action, []string{ID}, o)
			if err == nil || !strings.Contains(err.Error(), "plugin openclaw: ") || !strings.Contains(err.Error(), s.key) || strings.Contains(err.Error(), s.value) {
				t.Errorf("%s, %s=%s: want a refusal naming %s without its value, got %v", action, s.key, s.value, s.key, err)
			}
			if len(*log) > 0 {
				t.Errorf("%s, %s=%s: started %q before refusing", action, s.key, s.value, *log)
			}
			t.Setenv(s.key, "")
		}
		for _, s := range goodSettings {
			t.Setenv(s.key, s.value)
			log := recordExec(t)
			_ = plugin.Run(context.Background(), io.Discard, action, []string{ID}, o)
			if len(*log) == 0 || !strings.HasPrefix((*log)[0], first[action]) {
				t.Errorf("%s, %s=%q: first child %q, want %s", action, s.key, s.value, *log, first[action])
			}
			t.Setenv(s.key, "")
		}
	}
}

// The same refusal from the call Review makes before it lets y start the
// install (ui's settingsErr: plugin.CheckSettings on the chosen plugins with
// state.Config.Lookup), with each value only in config.local.env.
func TestReviewsLookupRefusesABadPeerOrWorkers(t *testing.T) {
	for _, k := range settingVars {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	ps, err := plugin.Resolve([]string{ID})
	if err != nil {
		t.Fatal(err)
	}
	check := func(key, value string) error {
		dir := t.TempDir()
		for name, body := range map[string]string{"config.env": "", "config.local.env": key + "=" + value + "\n"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cfg, err := state.LoadConfig(dir, os.LookupEnv)
		if err != nil {
			t.Fatal(err)
		}
		return plugin.CheckSettings(ps, cfg.Lookup)
	}
	for _, s := range badSettings {
		if err := check(s.key, s.value); err == nil || !strings.Contains(err.Error(), s.key) || strings.Contains(err.Error(), s.value) {
			t.Errorf("%s=%s in config.local.env: want a refusal naming %s without its value, got %v", s.key, s.value, s.key, err)
		}
	}
	for _, s := range goodSettings {
		if err := check(s.key, s.value); err != nil {
			t.Errorf("%s=%q in config.local.env: %v", s.key, s.value, err)
		}
	}
}

// With nothing set (the pinned defaults), and with pinned overrides,
// Deploy and Verify go on to the cluster, with those images.
func TestHooksProceedWithPinnedImages(t *testing.T) {
	o := plugin.Options{Cluster: "demo", Region: "us-west-2", Kubeconfig: "/kc", Release: plugin.Release{Tag: "v1.0.0", ImageRepo: "ghcr.io/agent-substrate/substrate"}}
	for _, tc := range []struct {
		name string
		env  []string
		want images
	}{
		{"defaults", []string{"", "", ""}, images{gateway: GatewayImage, actor: ActorImage, dashboard: DashboardImage}},
		{"overrides", []string{gatewayRepo + digest, actorRepo + digest, dashboardRepo + digest}, testImages()},
	} {
		for i, e := range imageVars {
			t.Setenv(e, tc.env[i])
		}
		im, err := pinnedImages(os.LookupEnv)
		if err != nil || im.gateway != tc.want.gateway || im.actor != tc.want.actor || im.dashboard != tc.want.dashboard {
			t.Errorf("%s: %+v, %v", tc.name, im, err)
		}
		for name, first := range map[string]string{
			"deploy": "kubectl --kubeconfig /kc -n ate-system get deployment ate-api-server",
			"verify": "kubectl --kubeconfig /kc -n ate-system get deployment atenet-router",
		} {
			log := recordExec(t)
			hook := map[string]func(context.Context, io.Writer, plugin.Options) error{"deploy": deploy, "verify": verify}[name]
			if err := hook(context.Background(), io.Discard, o); err == nil || strings.Contains(err.Error(), "not pinned") {
				t.Errorf("%s %s: %v", tc.name, name, err)
			}
			if len(*log) == 0 || !strings.HasPrefix((*log)[0], first) {
				t.Errorf("%s %s: first child %v, want %s", tc.name, name, *log, first)
			}
		}
	}
}

// Both ways: the gateway's worker pool runs the install's release's gVisor
// worker, a fork's when a plugin replaced the release.
func TestWorkerIsTheReleases(t *testing.T) {
	prev := pinImage
	t.Cleanup(func() { pinImage = prev })
	pinImage = func(_ context.Context, tagged string) (string, error) { return tagged + digest, nil }
	for _, rel := range []plugin.Release{
		{Tag: "v1.0.0", ImageRepo: "ghcr.io/agent-substrate/substrate"},
		{Tag: "v1.0.0-mode3.9f36a0d", ImageRepo: "ghcr.io/csantanapr/substrate"},
	} {
		im, err := withWorker(context.Background(), plugin.Options{Release: rel}, testImages())
		if want := rel.ImageRepo + "/ateworker-gvisor:" + rel.Tag + digest; err != nil || im.worker != want {
			t.Errorf("%s: worker %q, %v; want %q", rel.Tag, im.worker, err, want)
		}
	}
}
