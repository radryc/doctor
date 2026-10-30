package alerting

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CloudWatchAlarm mirrors guardian/internal/awsread.Alarm.
type CloudWatchAlarm struct {
	Name                    string            `json:"name"`
	ARN                     string            `json:"arn,omitempty"`
	Account                 string            `json:"account,omitempty"`
	Region                  string            `json:"region,omitempty"`
	Type                    string            `json:"type,omitempty"`
	State                   string            `json:"state"`
	StateReason             string            `json:"stateReason,omitempty"`
	StateUpdatedAt          time.Time         `json:"stateUpdatedAt,omitempty"`
	Namespace               string            `json:"namespace,omitempty"`
	MetricName              string            `json:"metricName,omitempty"`
	Threshold               float64           `json:"threshold,omitempty"`
	ComparisonOperator      string            `json:"comparisonOperator,omitempty"`
	Tags                    map[string]string `json:"tags,omitempty"`
	ActionsEnabled          bool              `json:"actionsEnabled"`
	AlarmActions            []string          `json:"alarmActions,omitempty"`
	OKActions               []string          `json:"okActions,omitempty"`
	InsufficientDataActions []string          `json:"insufficientDataActions,omitempty"`
}

type alarmEnvelope struct {
	Alarms []CloudWatchAlarm `json:"alarms"`
	Count  int               `json:"count"`
}

// AWSSource polls the guardian-pusher-aws read API and imports selected
// CloudWatch alarms.
type AWSSource struct {
	BaseURL     string
	Token       string
	Regions     []string
	Tenant      string
	Policy      *AWSPolicy
	Service     *Service
	Store       *Store
	HTTP        *http.Client
	Logger      *log.Logger
	Interval    time.Duration
	DefaultMode AWSPolicyMode
	Now         func() time.Time
}

// SourceSummary reports one poll result.
type SourceSummary struct {
	Discovered int `json:"discovered"`
	Included   int `json:"included"`
	Excluded   int `json:"excluded"`
	Imported   int `json:"imported"`
	Unscoped   int `json:"unscoped"`
}

// Run polls until ctx is cancelled.
func (s *AWSSource) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if summary, err := s.PollOnce(ctx); err != nil {
		s.logf("poll: %v", err)
	} else {
		s.logf("poll: discovered=%d included=%d excluded=%d imported=%d unscoped=%d", summary.Discovered, summary.Included, summary.Excluded, summary.Imported, summary.Unscoped)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if summary, err := s.PollOnce(ctx); err != nil {
				s.logf("poll: %v", err)
			} else {
				s.logf("poll: discovered=%d included=%d excluded=%d imported=%d unscoped=%d", summary.Discovered, summary.Included, summary.Excluded, summary.Imported, summary.Unscoped)
			}
		}
	}
}

// PollOnce fetches and imports selected alarms.
func (s *AWSSource) PollOnce(ctx context.Context) (SourceSummary, error) {
	var summary SourceSummary
	if strings.TrimSpace(s.BaseURL) == "" {
		return summary, fmt.Errorf("alerting: aws read URL is not configured")
	}
	policy := s.Policy
	if policy == nil && s.Store != nil {
		loaded, err := s.Store.GetAWSPolicy(ctx)
		if err != nil {
			return summary, err
		}
		policy = loaded
	}
	if policy == nil {
		policy = DefaultAWSPolicy()
	}
	if s.DefaultMode != "" {
		policy.Default = s.DefaultMode
	}
	alarms, err := s.fetchAlarms(ctx)
	if err != nil {
		return summary, err
	}
	summary.Discovered = len(alarms)
	for _, alarm := range alarms {
		meta := AlarmMeta{
			Name:      alarm.Name,
			ARN:       alarm.ARN,
			Account:   alarm.Account,
			Region:    alarm.Region,
			Type:      alarm.Type,
			Namespace: alarm.Namespace,
			Metric:    alarm.MetricName,
			Tags:      alarm.Tags,
		}
		selection := policy.Select(meta)
		if !selection.Included {
			summary.Excluded++
			continue
		}
		alert, ok := s.normalize(alarm)
		if !ok {
			summary.Unscoped++
			continue
		}
		summary.Included++
		if s.Service == nil {
			continue
		}
		if _, err := s.Service.Apply(ctx, alert); err != nil {
			return summary, err
		}
		summary.Imported++
	}
	return summary, nil
}

func (s *AWSSource) fetchAlarms(ctx context.Context) ([]CloudWatchAlarm, error) {
	endpoint := strings.TrimRight(s.BaseURL, "/") + "/v1/aws/alarms"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(s.Token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(s.Token))
	}
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("aws read API HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var envelope alarmEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode alarms: %w", err)
	}
	if len(s.Regions) == 0 {
		return envelope.Alarms, nil
	}
	allowed := make(map[string]bool, len(s.Regions))
	for _, region := range s.Regions {
		allowed[strings.TrimSpace(region)] = true
	}
	out := make([]CloudWatchAlarm, 0, len(envelope.Alarms))
	for _, alarm := range envelope.Alarms {
		if allowed[alarm.Region] {
			out = append(out, alarm)
		}
	}
	return out, nil
}

func (s *AWSSource) normalize(alarm CloudWatchAlarm) (Alert, bool) {
	partition := firstTag(alarm.Tags, "sentinel.io/partition", "guardian.io/partition", "guardian_partition", "partition")
	if partition == "" {
		return Alert{}, false
	}
	status := StatusNoData
	switch strings.ToUpper(alarm.State) {
	case "ALARM":
		status = StatusFiring
	case "OK":
		status = StatusResolved
	case "INSUFFICIENT_DATA":
		status = StatusNoData
	}
	severity := firstTag(alarm.Tags, "sentinel.io/severity", "severity")
	if severity == "" {
		if status == StatusFiring {
			severity = "warning"
		} else {
			severity = "info"
		}
	}
	delivery := firstTag(alarm.Tags, "sentinel.io/delivery", "delivery")
	if delivery == "" {
		delivery = DeliverySentinel
	}
	labels := map[string]string{
		"alertname": alarm.Name,
		"source":    "aws",
		"account":   alarm.Account,
		"region":    alarm.Region,
		"arn":       alarm.ARN,
	}
	if partition != "" {
		labels["partition"] = partition
	}
	for _, key := range []string{"service", "intent", "asset", "cluster"} {
		if value := firstTag(alarm.Tags, "guardian.io/"+key, key); value != "" {
			labels[key] = value
		}
	}
	annotations := map[string]string{
		"summary": alarm.StateReason,
	}
	if runbook := firstTag(alarm.Tags, "sentinel.io/runbook", "runbook_url", "runbook"); runbook != "" {
		annotations["runbook_url"] = runbook
	}
	if alarm.ARN != "" {
		annotations["console_url"] = awsConsoleURL(alarm.Region, alarm.ARN)
	}
	tenant := s.Tenant
	if tenant == "" && s.Service != nil {
		tenant = s.Service.DefaultTenant
	}
	if tenant == "" {
		tenant = "default"
	}
	startsAt := alarm.StateUpdatedAt
	if startsAt.IsZero() {
		startsAt = s.now()
	}
	alert := Alert{
		Fingerprint: Fingerprint("aws", tenant, partition, labels),
		Source:      "aws",
		Tenant:      tenant,
		Partition:   partition,
		Status:      status,
		Severity:    severity,
		Labels:      labels,
		Annotations: annotations,
		StartsAt:    startsAt,
		Delivery:    delivery,
	}
	alert.ID = alert.Fingerprint
	if status == StatusResolved {
		now := s.now()
		alert.EndsAt = &now
	}
	return alert, true
}

func firstTag(tags map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(tags[key]); value != "" {
			return value
		}
	}
	return ""
}

func awsConsoleURL(region, arn string) string {
	if region == "" {
		region = "us-east-1"
	}
	return "https://console.aws.amazon.com/cloudwatch/home?region=" + url.QueryEscape(region) + "#alarmsV2:alarm/" + url.PathEscape(lastPath(arn))
}

func lastPath(value string) string {
	value = strings.TrimSpace(value)
	if idx := strings.LastIndex(value, ":"); idx >= 0 {
		return value[idx+1:]
	}
	return value
}

func (s *AWSSource) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *AWSSource) logf(format string, args ...any) {
	if s.Logger != nil {
		s.Logger.Printf("sentinel aws: "+format, args...)
	}
}
