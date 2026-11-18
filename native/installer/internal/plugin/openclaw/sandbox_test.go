package openclaw

import (
	"strings"
	"testing"

	"github.com/csantanapr/eks-agent-substrate/native/installer/internal/plugin"
)

func TestSandboxFor(t *testing.T) {
	for _, c := range []struct {
		v     string
		pools []string
		want  string
		bad   bool
	}{
		{"", []string{"gvisor", "system"}, SandboxGVisor, false},
		{"", []string{"isolated", "system"}, SandboxNitro, false},
		{"", []string{"gvisor", "isolated"}, SandboxGVisor, false},
		{"", nil, SandboxGVisor, false},
		{"nitro", []string{"gvisor"}, SandboxNitro, false},
		{"gvisor", []string{"isolated"}, SandboxGVisor, false},
		{"kata", nil, "", true},
	} {
		got, err := sandboxFor(c.v, c.pools)
		if (err != nil) != c.bad || got != c.want {
			t.Errorf("sandboxFor(%q, %v) = %q, %v; want %q, bad=%v", c.v, c.pools, got, err, c.want, c.bad)
		}
	}
}

func TestRenderNitroPool(t *testing.T) {
	m, err := renderAll(testImages(), "tok-0123456789abcdef", "2", mustWA(t), SandboxNitro)
	if err != nil {
		t.Fatal(err)
	}
	pool := string(m.workerPool)
	for _, want := range []string{
		"runtimeClassName: isolated-sandbox", "ate.dev/pool: isolated", "kubernetes.io/arch: amd64",
		"value: isolated", "sandbox.amazonaws.com/overlay-disk-iops: disabled", "replicas: 2",
		"workerImage: " + testImages().worker,
	} {
		if !strings.Contains(pool, want) {
			t.Errorf("nitro pool lacks %q:\n%s", want, pool)
		}
	}
	if strings.Contains(pool, "ate.dev/pool: gvisor") || strings.Contains(pool, "REPLACE_WITH") {
		t.Errorf("nitro pool still names gvisor or a placeholder:\n%s", pool)
	}
	g, err := renderAll(testImages(), "tok-0123456789abcdef", "2", mustWA(t), SandboxGVisor)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(g.workerPool), "isolated-sandbox") {
		t.Error("the gvisor pool names isolated-sandbox")
	}
}

func TestAltNodes(t *testing.T) {
	p, ok := plugin.Get(ID)
	if !ok {
		t.Fatal("openclaw is not registered")
	}
	if miss := plugin.MissingNodes(p, []string{"system", "isolated"}); len(miss) != 0 {
		t.Errorf("isolated nodes should stand in for gvisor, missing %v", miss)
	}
	if miss := plugin.MissingNodes(p, []string{"system"}); len(miss) != 1 || miss[0] != "gvisor" {
		t.Errorf("no gvisor and no isolated nodes: missing %v, want [gvisor]", miss)
	}
}

func TestCheckRefusesSandbox(t *testing.T) {
	err := check(func(k string) (string, bool) {
		if k == "OPENCLAW_SANDBOX" {
			return "kata", true
		}
		return "", false
	})
	if err == nil || !strings.Contains(err.Error(), "OPENCLAW_SANDBOX") {
		t.Errorf("check accepted OPENCLAW_SANDBOX=kata: %v", err)
	}
}

func mustWA(t *testing.T) whatsapp {
	t.Helper()
	wa, err := whatsappFor("")
	if err != nil {
		t.Fatal(err)
	}
	return wa
}
