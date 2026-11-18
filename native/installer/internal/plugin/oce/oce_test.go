package oce

import (
	"encoding/json"
	"strings"
	"testing"
)

// The Agent ID and gateway hash of the Agent on alick-substrate: OCE names
// its Deployment gateway-6f5037dd7123 and ServiceAccount agent-6f5037dd7123.
func TestAgentHash(t *testing.T) {
	id := "agt_deb72691-f159-41b9-8027-b29bb94212a7"
	if got := agentHash(id); got != "6f5037dd7123" {
		t.Fatalf("agentHash = %s, want 6f5037dd7123", got)
	}
	if got := spiffeID("oce-515c433cf21eef7", id); got != "spiffe://cluster.local/ns/oce-515c433cf21eef7/sa/agent-6f5037dd7123" {
		t.Errorf("spiffeID = %s", got)
	}
	if got := principal("oce-515c433cf21eef7", id); got != "user:system:serviceaccount:oce-515c433cf21eef7:agent-6f5037dd7123" {
		t.Errorf("principal = %s", got)
	}
}

func TestAgentName(t *testing.T) {
	for v, ok := range map[string]bool{"": true, "substrate-demo": true, "a": true, "-x": false, "UPPER": false, "x_y": false, strings.Repeat("a", 41): false} {
		if _, err := agentName(v); (err == nil) != ok {
			t.Errorf("agentName(%q) err=%v, want ok=%v", v, err, ok)
		}
	}
}

func TestWithID(t *testing.T) {
	arg := routerFlag + "spiffe://a,spiffe://b"
	if got := withID(arg, "spiffe://c"); got != arg+",spiffe://c" {
		t.Errorf("withID add = %s", got)
	}
	if got := withID(arg, "spiffe://b"); got != "" {
		t.Errorf("withID present = %q, want empty", got)
	}
}

func TestAddEditor(t *testing.T) {
	in := []byte(`{"metadata":{"name":"default"},"bindings":[{"role":"editor","members":["user:a"]}]}`)
	out, changed, err := addEditor(in, "user:b")
	if err != nil || !changed {
		t.Fatalf("addEditor: changed=%v err=%v", changed, err)
	}
	var p struct {
		Metadata map[string]string `json:"metadata"`
		Bindings []struct {
			Role    string   `json:"role"`
			Members []string `json:"members"`
		} `json:"bindings"`
	}
	if err := json.Unmarshal(out, &p); err != nil || p.Metadata["name"] != "default" || len(p.Bindings) != 1 || strings.Join(p.Bindings[0].Members, ",") != "user:a,user:b" {
		t.Fatalf("addEditor kept %s", out)
	}
	if _, changed, _ := addEditor(out, "user:b"); changed {
		t.Error("addEditor changed a policy that has the member")
	}
	out, changed, _ = addEditor([]byte(`{"metadata":{"name":"default"}}`), "user:c")
	if !changed || !strings.Contains(string(out), `"role":"editor"`) {
		t.Errorf("addEditor without bindings = %s", out)
	}
}

// Every manifest's placeholders are the ones Deploy fills, so none is left.
func TestManifestsFill(t *testing.T) {
	for name, m := range map[string][]byte{
		"postgres":     fill(postgresYAML, "REPLACE_WITH_STORAGE_CLASS", "sc"),
		"prepare":      fill(prepareYAML, "REPLACE_WITH_STORAGE_CLASS", "sc", "REPLACE_WITH_CONTROLLER_IMAGE", ControllerImage),
		"relay":        fill(relayYAML, "REPLACE_WITH_CONTROLLER_IMAGE", ControllerImage, "REPLACE_WITH_API_SERVICE", apiService),
		"values":       fill(valuesYAML, "REPLACE_WITH_CONTROLLER_IMAGE", ControllerImage, "REPLACE_WITH_DATABASE_CIDRS", quoted([]string{"10.0.0.1"}), "REPLACE_WITH_CLUSTER_CIDRS", quoted([]string{"10.0.0.2"})),
		"installation": fill(installationYAML, "REPLACE_WITH_RUNTIME_IMAGE", "r@sha256:x", "REPLACE_WITH_STORAGE_CLASS", "sc", "REPLACE_WITH_CLUSTER", "c"),
		"tenant":       fill(tenantYAML, "REPLACE_WITH_TENANT", "oce-x", "REPLACE_WITH_KUBE_API_BLOCKS", ipBlocks([]string{"10.0.0.2"}, "        ")),
		"agent-setup":  fill(agentSetupYAML, "REPLACE_WITH_CONTROLLER_IMAGE", ControllerImage, "REPLACE_WITH_AGENT_NAME", "a", "REPLACE_WITH_ATESPACE", "s", "REPLACE_WITH_TEMPLATE", "t"),
	} {
		if strings.Contains(string(m), "REPLACE_WITH_") {
			t.Errorf("%s keeps a placeholder", name)
		}
	}
	if !strings.Contains(string(gatewayPolicyYAML), "patchType: JSONPatch") {
		t.Error("the gateway policy must be a JSONPatch: an ApplyConfiguration may not set its atomic fields")
	}
}

func TestRuntimeTagFollowsTheBridge(t *testing.T) {
	if tag := runtimeTag(); !strings.HasPrefix(tag, ChartRevision[:7]+"-") || len(tag) != 18 {
		t.Errorf("runtimeTag = %s", tag)
	}
}
