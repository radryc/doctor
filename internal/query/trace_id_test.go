package query

import "testing"

func TestIsXRayTraceID(t *testing.T) {
	for _, id := range []string{
		"1-6adb5c86-43638b80008cec551d0c73c5",
		"1-00000000-000000000000000000000000",
	} {
		if !isXRayTraceID(id) {
			t.Fatalf("isXRayTraceID(%q) = false, want true", id)
		}
	}
	for _, id := range []string{
		"2a6bcebffa8d5800a720320a30a81bf6",
		"1-xyz-43638b80008cec551d0c73c5",
		"1-6adb5c86",
		"",
	} {
		if isXRayTraceID(id) {
			t.Fatalf("isXRayTraceID(%q) = true, want false", id)
		}
	}
}
