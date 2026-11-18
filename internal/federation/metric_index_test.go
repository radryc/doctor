package federation

import (
	"context"
	"io"
	"log"
	"testing"

	awsstore "github.com/radryc/doctor/internal/store/aws"
)

func TestManagerListMetricsPreservesNamespaceAndCaps(t *testing.T) {
	catalog, err := NewCatalog([]Target{{
		ID:       "aws-a",
		Account:  "111",
		Region:   "eu-west-1",
		Provider: "aws",
		Endpoint: "http://aws-a",
		Token:    "t",
		MetricDescriptors: []awsstore.MetricDescriptor{
			{Namespace: "AWS/EC2", MetricName: "CPUUtilization"},
			{Namespace: "Custom/App", MetricName: "CPUUtilization"},
			{Namespace: "AWS/EC2", MetricName: "NetworkIn"},
		},
	}}, "v1")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	manager := NewManager(catalog, Options{
		DiscoveryLimit: 2,
		Logger:         log.New(io.Discard, "", 0),
	})

	all, err := manager.ListMetrics(context.Background(), "", "")
	if err != nil {
		t.Fatalf("ListMetrics: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListMetrics length = %d, want cap 2", len(all))
	}

	ec2, err := manager.ListMetrics(context.Background(), "AWS/EC2", "")
	if err != nil {
		t.Fatalf("ListMetrics namespace: %v", err)
	}
	if len(ec2) != 2 {
		t.Fatalf("AWS/EC2 descriptors = %#v", ec2)
	}
	for _, descriptor := range ec2 {
		if descriptor.Namespace != "AWS/EC2" || descriptor.Account != "111" || descriptor.Region != "eu-west-1" {
			t.Fatalf("descriptor = %#v", descriptor)
		}
	}
}

func TestBuildMetricIndexPrefersDescriptors(t *testing.T) {
	catalog, err := NewCatalog([]Target{{
		ID:          "aws-a",
		Account:     "111",
		Region:      "eu-west-1",
		Provider:    "aws",
		Endpoint:    "http://aws-a",
		Token:       "t",
		MetricNames: []string{"LegacyName"},
		MetricDescriptors: []awsstore.MetricDescriptor{
			{Namespace: "Custom/App", MetricName: "CPUUtilization"},
		},
	}}, "v1")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	index := buildMetricIndex(catalog)
	if len(index) != 1 || index[0].Namespace != "Custom/App" || index[0].MetricName != "CPUUtilization" {
		t.Fatalf("index = %#v", index)
	}
}
