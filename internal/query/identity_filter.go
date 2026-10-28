package query

import (
	"net/http"
	"strings"
)

// signalIdentity is the canonical partition/intent/asset tuple shared by logs,
// traces, and metrics. Empty fields are wildcards.
type signalIdentity struct {
	Partition string
	Intent    string
	Asset     string
}

func identityFromRequest(r *http.Request) signalIdentity {
	return signalIdentity{
		Partition: partitionFromRequest(r),
		Intent:    strings.TrimSpace(requestParam(r, "intent")),
		Asset:     strings.TrimSpace(requestParam(r, "asset")),
	}
}

func (f signalIdentity) active() bool {
	return f.Partition != "" || f.Intent != "" || f.Asset != ""
}

func (f signalIdentity) matches(partition, intent, asset string) bool {
	if f.Partition != "" && partition != f.Partition {
		return false
	}
	if f.Intent != "" && intent != f.Intent {
		return false
	}
	if f.Asset != "" && asset != f.Asset {
		return false
	}
	return true
}

// logSelector renders the identity as Loki/LogEngine stream-labelled matchers.
// Underscore names work for both Doctor's Loki projection and MonoFS, which
// normalizes guardian_partition to guardian.partition when matching labels.
func (f signalIdentity) logSelector(service string) string {
	matchers := make([]string, 0, 4)
	if service != "" {
		matchers = append(matchers, `service="`+escapeLogQL(service)+`"`)
	}
	if f.Partition != "" {
		matchers = append(matchers, `guardian_partition="`+escapeLogQL(f.Partition)+`"`)
	}
	if f.Intent != "" {
		matchers = append(matchers, `guardian_intent="`+escapeLogQL(f.Intent)+`"`)
	}
	if f.Asset != "" {
		matchers = append(matchers, `guardian_asset="`+escapeLogQL(f.Asset)+`"`)
	}
	if len(matchers) == 0 {
		return `{service=~".+"}`
	}
	return "{" + strings.Join(matchers, ",") + "}"
}
