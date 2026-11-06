package dashboards

import (
	"reflect"
	"testing"
)

func TestEffectivePartitionsUnionsDocumentAndWidgets(t *testing.T) {
	doc := DashboardDocument{
		Partitions: []string{" agent ", "monofs", "agent", ""},
		Layout: []DashboardWidget{
			{Partitions: []string{"agent", "k8s-top"}},
			{Partitions: nil},
		},
	}
	got := doc.EffectivePartitions()
	want := []string{"agent", "k8s-top", "monofs"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("EffectivePartitions() = %v, want %v", got, want)
	}
}

func TestEffectivePartitionsEmptyForLegacyDocument(t *testing.T) {
	doc := DashboardDocument{}
	if got := doc.EffectivePartitions(); len(got) != 0 {
		t.Fatalf("EffectivePartitions() = %v, want empty", got)
	}
}
