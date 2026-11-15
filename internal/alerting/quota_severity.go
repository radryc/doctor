package alerting

import "strings"

// quotaAlertMarkers identify alerts caused by a partition breaching its own
// quota. Quota breaches are capacity signals, not availability incidents, so
// they must never be escalated to critical for the partition that triggered
// them. The alert is still delivered, just at warning severity.
var quotaAlertMarkers = []string{
	"quota",
	"resourceexhausted",
	"resource exhausted",
	"rate limit",
	"rate-limit",
	"too many requests",
}

// capQuotaBreachSeverity downgrades a critical quota-breach alert to warning.
// It is applied on every alert transition in Service.Apply, so no producer
// (Guardian partition alerts, Sentinel intake, rules, subspace events, cloud
// sources) can create a critical alert for a quota breach.
func capQuotaBreachSeverity(alert Alert) Alert {
	if !strings.EqualFold(strings.TrimSpace(alert.Severity), "critical") {
		return alert
	}
	if !isQuotaBreachAlert(alert) {
		return alert
	}
	alert.Severity = "warning"
	alert.Labels = cloneStringMap(alert.Labels)
	if alert.Labels == nil {
		alert.Labels = map[string]string{}
	}
	alert.Labels["severity"] = "warning"
	alert.Labels["quota_breach"] = "true"
	alert.Annotations = cloneStringMap(alert.Annotations)
	if alert.Annotations == nil {
		alert.Annotations = map[string]string{}
	}
	if _, ok := alert.Annotations["quota_breach"]; !ok {
		alert.Annotations["quota_breach"] = "quota breaches are capped at warning severity"
	}
	return alert
}

// isQuotaBreachAlert looks only at fields that describe the alert's cause. It
// deliberately avoids scanning every label value so unrelated strings (IDs,
// durations, HTTP paths) cannot accidentally suppress a real critical alert.
func isQuotaBreachAlert(alert Alert) bool {
	for _, value := range []string{
		alert.Labels["alertname"],
		alert.Labels["name"],
		alert.Labels["reason"],
		alert.Labels["resource"],
		alert.Labels["quota"],
		alert.Labels["quota_scope"],
		alert.Annotations["summary"],
		alert.Annotations["description"],
		alert.Value,
	} {
		if containsQuotaMarker(value) {
			return true
		}
	}
	return false
}

func containsQuotaMarker(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return false
	}
	for _, marker := range quotaAlertMarkers {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
