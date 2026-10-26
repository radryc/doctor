package query

import "testing"

func TestClassifyServiceKind(t *testing.T) {
	cases := []struct {
		name string
		info *svcInfo
		want string
	}{
		{"compute", &svcInfo{assetType: "Compute"}, serviceKindRuntime},
		{"loadbalancer", &svcInfo{assetType: "LoadBalancer"}, serviceKindRuntime},
		{"secret", &svcInfo{assetType: "Secret", hasAssetObservation: true}, serviceKindAsset},
		{"imagebuild", &svcInfo{assetType: "ImageBuild", hasAssetObservation: true}, serviceKindAsset},
		{"runtime-signal-without-type", &svcInfo{hasRuntimeMetrics: true}, serviceKindRuntime},
		{"observation-only-without-type", &svcInfo{hasAssetObservation: true}, serviceKindAsset},
		{"traces-without-type", &svcInfo{hasTraces: true}, serviceKindRuntime},
	}
	for _, tc := range cases {
		if got := classifyServiceKind(tc.info); got != tc.want {
			t.Errorf("%s: classifyServiceKind = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestTraceStatusIsError(t *testing.T) {
	for _, status := range []string{"STATUS_CODE_ERROR", "Error", "status_code_error"} {
		if !traceStatusIsError(status) {
			t.Errorf("traceStatusIsError(%q) = false", status)
		}
	}
	for _, status := range []string{"", "STATUS_CODE_UNSET", "STATUS_CODE_OK"} {
		if traceStatusIsError(status) {
			t.Errorf("traceStatusIsError(%q) = true", status)
		}
	}
}
