package k8s

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func loadManifest(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("doctor.yaml")
	if err != nil {
		t.Fatalf("read doctor.yaml: %v", err)
	}
	return string(data)
}

func TestManifestImagesArePinned(t *testing.T) {
	manifest := loadManifest(t)
	if strings.Contains(manifest, ":latest") {
		t.Fatal("deploy/k8s/doctor.yaml must not use the mutable :latest tag")
	}
	for _, image := range []string{"doctor-ingest:", "doctor-query:", "doctor-rules:", "doctor-sentinel:"} {
		if !strings.Contains(manifest, "image: "+image) {
			t.Fatalf("missing pinned image %q", image)
		}
	}
}

type networkPolicy struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Ingress []struct {
			From []struct {
				PodSelector       map[string]any `json:"podSelector"`
				NamespaceSelector map[string]any `json:"namespaceSelector"`
			} `json:"from"`
		} `json:"ingress"`
	} `json:"spec"`
}

func TestUIIngressAllowsOnlyIngressControllerPods(t *testing.T) {
	manifest := loadManifest(t)
	var policy *networkPolicy
	for _, document := range strings.Split(manifest, "\n---") {
		var decoded networkPolicy
		if err := yaml.Unmarshal([]byte(document), &decoded); err != nil {
			t.Fatalf("parse manifest document: %v", err)
		}
		if decoded.Metadata.Name == "doctor-allow-ui-ingress" {
			policy = &decoded
			break
		}
	}
	if policy == nil {
		t.Fatal("doctor-allow-ui-ingress NetworkPolicy not found")
	}
	if len(policy.Spec.Ingress) != 1 || len(policy.Spec.Ingress[0].From) != 1 {
		t.Fatalf("UI ingress must have exactly one scoped source, got %+v", policy.Spec.Ingress)
	}
	source := policy.Spec.Ingress[0].From[0]
	nsLabels, _ := source.NamespaceSelector["matchLabels"].(map[string]any)
	if nsLabels["kubernetes.io/metadata.name"] != "ingress-nginx" {
		t.Fatalf("namespace selector = %+v, want ingress-nginx", source.NamespaceSelector)
	}
	podLabels, _ := source.PodSelector["matchLabels"].(map[string]any)
	if len(podLabels) == 0 {
		t.Fatal("UI ingress must select ingress-controller pods, not every pod in the namespace")
	}
	if podLabels["app.kubernetes.io/name"] != "ingress-nginx" {
		t.Fatalf("pod selector = %+v, want ingress-nginx controller", podLabels)
	}
}
