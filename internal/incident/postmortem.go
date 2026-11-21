package incident

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	doctorstore "github.com/radryc/doctor/internal/store"
)

// Engine publishes a postmortem document to a destination.
type Engine interface {
	Name() string
	Metadata() EngineMetadata
	Publish(ctx context.Context, doc PostmortemDocument) (PostmortemRef, error)
}

// PostmortemDocument is the complete input to an engine.
type PostmortemDocument struct {
	Incident   Incident   `json:"incident"`
	Postmortem Postmortem `json:"postmortem"`
	Markdown   string     `json:"markdown"`
}

// PostmortemRef identifies the published document.
type PostmortemRef struct {
	URL        string `json:"url,omitempty"`
	ExternalID string `json:"externalId,omitempty"`
}

// EngineMetadata describes an engine and its options to the UI.
type EngineMetadata struct {
	Name        string      `json:"name"`
	Label       string      `json:"label"`
	Description string      `json:"description,omitempty"`
	Fields      []OptionDef `json:"fields,omitempty"`
}

// OptionDef is one engine/analyzer option.
type OptionDef struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Type        string `json:"type"`
	Required    bool   `json:"required,omitempty"`
	Secret      bool   `json:"secret,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
}

// EngineRegistry holds every registered postmortem publisher.
type EngineRegistry struct {
	engines map[string]Engine
	order   []string
}

// NewEngineRegistry builds the built-in registry.
func NewEngineRegistry() *EngineRegistry {
	registry := &EngineRegistry{engines: map[string]Engine{}}
	registry.Register(BuiltinEngine{})
	registry.Register(WebhookEngine{})
	registry.Register(GoogleDocsEngine{})
	return registry
}

// Register adds an engine.
func (r *EngineRegistry) Register(engine Engine) {
	if engine == nil {
		return
	}
	name := strings.ToLower(strings.TrimSpace(engine.Name()))
	if name == "" {
		return
	}
	if _, exists := r.engines[name]; !exists {
		r.order = append(r.order, name)
	}
	r.engines[name] = engine
}

// Get returns an engine by name.
func (r *EngineRegistry) Get(name string) Engine {
	if r == nil {
		return nil
	}
	return r.engines[strings.ToLower(strings.TrimSpace(name))]
}

// List returns metadata in registration order.
func (r *EngineRegistry) List() []EngineMetadata {
	if r == nil {
		return nil
	}
	out := make([]EngineMetadata, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.engines[name].Metadata())
	}
	return out
}

// BuiltinEngine keeps the markdown draft in Doctor.
type BuiltinEngine struct{}

// Name implements Engine.
func (BuiltinEngine) Name() string { return EngineBuiltin }

// Metadata implements Engine.
func (BuiltinEngine) Metadata() EngineMetadata {
	return EngineMetadata{
		Name:        EngineBuiltin,
		Label:       "Doctor draft",
		Description: "Store the postmortem in Doctor and link to it from the incident.",
	}
}

// Publish implements Engine.
func (BuiltinEngine) Publish(_ context.Context, doc PostmortemDocument) (PostmortemRef, error) {
	return PostmortemRef{URL: "/#/incidents/" + doc.Incident.ID + "?tab=postmortem"}, nil
}

// WebhookEngine publishes to an external tool through a generic JSON webhook.
type WebhookEngine struct {
	Client *http.Client
}

// Name implements Engine.
func (WebhookEngine) Name() string { return EngineWebhook }

// Metadata implements Engine.
func (WebhookEngine) Metadata() EngineMetadata {
	return EngineMetadata{
		Name:        EngineWebhook,
		Label:       "Generic webhook",
		Description: "POST the incident, timeline, analysis, and markdown to an external tool.",
		Fields: []OptionDef{
			{Key: "url", Label: "Endpoint URL", Type: "string", Placeholder: "https://..."},
			{Key: "urlEnv", Label: "URL environment variable", Type: "string", Secret: true},
			{Key: "secretEnv", Label: "HMAC secret environment variable", Type: "string", Secret: true},
		},
	}
}

// Publish implements Engine.
func (e WebhookEngine) Publish(ctx context.Context, doc PostmortemDocument) (PostmortemRef, error) {
	endpoint := strings.TrimSpace(doc.Postmortem.EngineOptions["url"])
	if env := strings.TrimSpace(doc.Postmortem.EngineOptions["urlEnv"]); env != "" {
		endpoint = strings.TrimSpace(os.Getenv(env))
	}
	if endpoint == "" {
		return PostmortemRef{}, fmt.Errorf("incident: webhook postmortem engine needs url or urlEnv")
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		return PostmortemRef{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return PostmortemRef{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "doctor-sentinel")
	if env := strings.TrimSpace(doc.Postmortem.EngineOptions["secretEnv"]); env != "" {
		secret := os.Getenv(env)
		if secret != "" {
			mac := hmac.New(sha256.New, []byte(secret))
			_, _ = mac.Write(payload)
			req.Header.Set("X-Doctor-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		}
	}
	client := e.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return PostmortemRef{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return PostmortemRef{}, fmt.Errorf("incident: postmortem webhook HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var ref PostmortemRef
	if len(bytes.TrimSpace(body)) > 0 {
		_ = json.Unmarshal(body, &ref)
	}
	if ref.URL == "" {
		ref.URL = endpoint
	}
	return ref, nil
}

// createPostmortem builds the draft and optionally starts analysis.
func (s *Service) createPostmortem(ctx context.Context, incident Incident, set *AlertSet, policy Policy) {
	now := s.now()
	engine := policy.PostmortemEngine
	options := map[string]string(nil)
	autoPublish := policy.AutoPublish
	provider := policy.AnalysisProvider
	timeout := duration(policy.AnalysisTimeout)
	if set != nil {
		if strings.TrimSpace(set.Postmortem.Engine) != "" {
			engine = set.Postmortem.Engine
		}
		options = set.Postmortem.Options
		autoPublish = autoPublish || set.Postmortem.AutoPublish
		if strings.TrimSpace(set.Postmortem.AnalysisProvider) != "" {
			provider = set.Postmortem.AnalysisProvider
		}
		if set.Postmortem.AnalysisTimeout > 0 {
			timeout = duration(set.Postmortem.AnalysisTimeout)
		}
	}
	if engine == "" {
		engine = EngineBuiltin
	}
	previous, version, err := s.Store.GetPostmortem(ctx, incident.Tenant, incident.Partition, incident.ID)
	if err != nil {
		s.logf("incident: load postmortem %s: %v", incident.ID, err)
		return
	}
	revision := 1
	expected := "absent"
	if previous != nil {
		revision = previous.Revision + 1
		expected = version
	}
	status := PostmortemDraft
	if strings.EqualFold(provider, AnalysisAgent) && s.Analyzers[AnalysisAgent] != nil {
		status = PostmortemAnalyzing
	}
	doc := Postmortem{
		IncidentID:       incident.ID,
		Tenant:           incident.Tenant,
		Partition:        incident.Partition,
		Revision:         revision,
		Status:           status,
		Engine:           engine,
		EngineOptions:    options,
		AnalysisProvider: provider,
		Markdown:         renderPostmortem(incident, nil),
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := s.Store.SavePostmortem(ctx, doc, expected); err != nil {
		if errors.Is(err, doctorstore.ErrConflict) {
			return
		}
		s.logf("incident: save postmortem %s: %v", incident.ID, err)
		return
	}
	if status == PostmortemAnalyzing {
		incident.Append(now, EventAnalysisStarted, "sentinel", "agent analysis started")
		incident.NextUpdateAt = nil
		if version := s.latestIncidentVersion(ctx, incident); version != "absent" {
			_ = s.Store.SaveIncident(ctx, incident, version, nil, nil)
		}
		if s.AsyncAnalysis {
			go func(tenant, partition, id string) {
				analyzeCtx, cancel := context.WithTimeout(context.Background(), timeout)
				defer cancel()
				if err := s.AnalyzeNow(analyzeCtx, tenant, partition, id); err != nil {
					s.logf("incident: analyze %s: %v", id, err)
				}
			}(incident.Tenant, incident.Partition, incident.ID)
		}
	} else if autoPublish {
		_, _ = s.Publish(ctx, incident.Tenant, incident.Partition, incident.ID, "sentinel")
	}
}

// AnalyzeNow runs the configured analyzer and updates the postmortem.
func (s *Service) AnalyzeNow(ctx context.Context, tenant, partition, id string) error {
	incident, _, err := s.Store.GetIncident(ctx, tenant, partition, id)
	if err != nil || incident == nil {
		return ErrNotFound
	}
	doc, version, err := s.Store.GetPostmortem(ctx, tenant, partition, id)
	if err != nil || doc == nil {
		return ErrNotFound
	}
	provider := strings.ToLower(strings.TrimSpace(doc.AnalysisProvider))
	if provider == "" || provider == AnalysisNone {
		return nil
	}
	analyzer := s.Analyzers[provider]
	if analyzer == nil {
		doc.Status = PostmortemFailed
		doc.Error = "analysis provider not configured: " + provider
		return s.Store.SavePostmortem(ctx, *doc, version)
	}
	set := s.loadSet(ctx, tenant, partition, incident.Set)
	policy := s.loadPolicy(ctx, tenant, partition)
	timeout := duration(policy.AnalysisTimeout)
	if set != nil && set.Postmortem.AnalysisTimeout > 0 {
		timeout = duration(set.Postmortem.AnalysisTimeout)
	}
	analysisCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	analysis, err := analyzer.Analyze(analysisCtx, AnalysisInput{
		Incident: *incident,
		Set:      derefSet(set),
		Alerts:   incident.MemberAlerts,
	})
	if err != nil {
		doc.Status = PostmortemFailed
		doc.Error = err.Error()
		_ = s.Store.SavePostmortem(ctx, *doc, version)
		return err
	}
	doc.Analysis = &analysis
	doc.Markdown = renderPostmortem(*incident, &analysis)
	doc.Status = PostmortemAnalyzed
	doc.Error = ""
	if err := s.Store.SavePostmortem(ctx, *doc, version); err != nil {
		return err
	}
	now := s.now()
	incident.Append(now, EventAnalysisCompleted, "agent", "postmortem analysis completed")
	if v := s.latestIncidentVersion(ctx, *incident); v != "absent" {
		_ = s.Store.SaveIncident(ctx, *incident, v, nil, nil)
	}
	autoPublish := policy.AutoPublish
	if set != nil {
		autoPublish = autoPublish || set.Postmortem.AutoPublish
	}
	if autoPublish {
		_, _ = s.Publish(ctx, tenant, partition, id, "sentinel")
	}
	return nil
}

// Publish sends the postmortem to its configured engine.
func (s *Service) Publish(ctx context.Context, tenant, partition, id, actor string) (*Postmortem, error) {
	incident, _, err := s.Store.GetIncident(ctx, tenant, partition, id)
	if err != nil || incident == nil {
		return nil, ErrNotFound
	}
	doc, version, err := s.Store.GetPostmortem(ctx, tenant, partition, id)
	if err != nil || doc == nil {
		return nil, ErrNotFound
	}
	engine := s.Engines.Get(doc.Engine)
	if engine == nil {
		return nil, fmt.Errorf("incident: unknown postmortem engine %q", doc.Engine)
	}
	doc.Attempts++
	ref, err := engine.Publish(ctx, PostmortemDocument{Incident: *incident, Postmortem: *doc, Markdown: doc.Markdown})
	if err != nil {
		doc.Status = PostmortemFailed
		doc.Error = err.Error()
		_ = s.Store.SavePostmortem(ctx, *doc, version)
		return nil, err
	}
	doc.URL = ref.URL
	doc.ExternalID = ref.ExternalID
	doc.Status = PostmortemPublished
	doc.Error = ""
	if err := s.Store.SavePostmortem(ctx, *doc, version); err != nil {
		return nil, err
	}
	now := s.now()
	incident.Append(now, EventPostmortemPublished, actor, "postmortem published to "+doc.Engine)
	set := s.loadSet(ctx, tenant, partition, incident.Set)
	outbox := s.notifications(ctx, derefSet(set), *incident, "postmortem.published", actor, "postmortem published")
	if v := s.latestIncidentVersion(ctx, *incident); v != "absent" {
		_ = s.Store.SaveIncident(ctx, *incident, v, nil, outbox)
	}
	return doc, nil
}

// Postmortem loads a postmortem.
func (s *Service) Postmortem(ctx context.Context, tenant, partition, id string) (*Postmortem, error) {
	doc, _, err := s.Store.GetPostmortem(ctx, tenant, partition, id)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, ErrNotFound
	}
	return doc, nil
}

// UpdatePostmortemDraft lets an operator edit the markdown before publishing.
func (s *Service) UpdatePostmortemDraft(ctx context.Context, tenant, partition, id, markdown string) (*Postmortem, error) {
	doc, version, err := s.Store.GetPostmortem(ctx, tenant, partition, id)
	if err != nil || doc == nil {
		return nil, ErrNotFound
	}
	if strings.TrimSpace(markdown) != "" {
		doc.Markdown = markdown
	}
	if doc.Status == PostmortemPublished {
		doc.Status = PostmortemAnalyzed
	}
	if err := s.Store.SavePostmortem(ctx, *doc, version); err != nil {
		return nil, err
	}
	return doc, nil
}

func renderPostmortem(incident Incident, analysis *Analysis) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Incident %s\n\n", incident.TitleText())
	fmt.Fprintf(&b, "**ID:** %s  \n", incident.ID)
	fmt.Fprintf(&b, "**Severity:** %s  \n", incident.Severity)
	fmt.Fprintf(&b, "**Partition:** %s  \n", incident.Partition)
	fmt.Fprintf(&b, "**Alert set:** %s  \n", incident.Set)
	fmt.Fprintf(&b, "**State:** %s  \n", incident.State)
	fmt.Fprintf(&b, "**Opened:** %s  \n", incident.OpenedAt.Format(time.RFC3339))
	if incident.AcknowledgedAt != nil {
		fmt.Fprintf(&b, "**Acknowledged:** %s  \n", incident.AcknowledgedAt.Format(time.RFC3339))
	}
	if incident.MitigatedAt != nil {
		fmt.Fprintf(&b, "**Mitigated:** %s  \n", incident.MitigatedAt.Format(time.RFC3339))
	}
	if incident.ResolvedAt != nil {
		fmt.Fprintf(&b, "**Resolved:** %s  \n", incident.ResolvedAt.Format(time.RFC3339))
	}
	b.WriteString("\n## Summary\n\n")
	if analysis != nil && strings.TrimSpace(analysis.Summary) != "" {
		b.WriteString(analysis.Summary + "\n")
	} else if strings.TrimSpace(incident.Resolution) != "" {
		b.WriteString(incident.Resolution + "\n")
	} else {
		b.WriteString("Analysis pending.\n")
	}
	if analysis != nil && strings.TrimSpace(analysis.Impact) != "" {
		b.WriteString("\n## Impact\n\n" + analysis.Impact + "\n")
	}
	if analysis != nil && strings.TrimSpace(analysis.RootCause) != "" {
		b.WriteString("\n## Root cause\n\n" + analysis.RootCause + "\n")
	}
	if analysis != nil && len(analysis.ContributingFactors) > 0 {
		b.WriteString("\n## Contributing factors\n\n")
		for _, factor := range analysis.ContributingFactors {
			b.WriteString("- " + factor + "\n")
		}
	}
	if analysis != nil && len(analysis.ActionItems) > 0 {
		b.WriteString("\n## Action items\n\n")
		for _, item := range analysis.ActionItems {
			fmt.Fprintf(&b, "- [ ] %s", item.Title)
			if item.Target != "" {
				fmt.Fprintf(&b, " (%s)", item.Target)
			}
			b.WriteString("\n")
			if item.Rationale != "" {
				b.WriteString("      " + item.Rationale + "\n")
			}
		}
	}
	b.WriteString("\n## Timeline\n\n")
	for _, entry := range incident.Timeline {
		fmt.Fprintf(&b, "- %s **%s** %s\n", entry.At.Format(time.RFC3339), entry.Kind, entry.Message)
	}
	if len(incident.MemberAlerts) > 0 {
		b.WriteString("\n## Member alerts\n\n")
		for _, alert := range incident.MemberAlerts {
			fmt.Fprintf(&b, "- %s (%s, %s)\n", alert.Alertname, alert.Severity, alert.Status)
		}
	}
	if analysis != nil && len(analysis.Evidence) > 0 {
		b.WriteString("\n## Evidence\n\n")
		for _, evidence := range analysis.Evidence {
			fmt.Fprintf(&b, "- %s: %s\n", evidence.Kind, firstNonEmpty(evidence.Summary, evidence.Query, evidence.Source))
		}
	}
	if analysis != nil {
		fmt.Fprintf(&b, "\n---\n\n_Generated by %s", analysis.Provider)
		if analysis.Model != "" {
			fmt.Fprintf(&b, " (%s)", analysis.Model)
		}
		if analysis.Confidence > 0 {
			fmt.Fprintf(&b, ", confidence %.0f%%", analysis.Confidence*100)
		}
		b.WriteString("._\n")
	}
	return b.String()
}

// AgentAnalyzer calls the Strata SRE agent's on-demand investigation endpoint.
type AgentAnalyzer struct {
	URL    string
	Token  string
	Client *http.Client
}

// Name implements Analyzer.
func (a AgentAnalyzer) Name() string { return AnalysisAgent }

type agentInvestigationRequest struct {
	ExternalRef     string        `json:"externalRef"`
	Incident        agentIncident `json:"incident"`
	Alerts          []agentAlert  `json:"alerts"`
	DeadlineSeconds int           `json:"deadlineSeconds"`
}

type agentIncident struct {
	ID        string          `json:"id"`
	Partition string          `json:"partition"`
	Title     string          `json:"title"`
	Severity  string          `json:"severity"`
	State     string          `json:"state"`
	OpenedAt  time.Time       `json:"openedAt"`
	Timeline  []TimelineEntry `json:"timeline,omitempty"`
}

type agentAlert struct {
	Fingerprint string            `json:"fingerprint"`
	Alertname   string            `json:"alertname"`
	Status      string            `json:"status"`
	Severity    string            `json:"severity"`
	Labels      map[string]string `json:"labels,omitempty"`
	StartsAt    time.Time         `json:"starts_at,omitempty"`
}

type agentInvestigationResponse struct {
	Summary              string  `json:"summary"`
	RootCausePartition   string  `json:"root_cause_partition"`
	RootCauseService     string  `json:"root_cause_service"`
	Confidence           float64 `json:"confidence"`
	RecommendedAction    string  `json:"recommended_action"`
	RecommendationTarget string  `json:"recommendation_target"`
	Rationale            string  `json:"rationale"`
	Model                string  `json:"model"`
	Improvements         []struct {
		Title          string  `json:"title"`
		Category       string  `json:"category"`
		Rationale      string  `json:"rationale"`
		ProposedChange string  `json:"proposed_change"`
		Target         string  `json:"target"`
		Confidence     float64 `json:"confidence"`
	} `json:"improvements"`
	Evidence []struct {
		Kind          string `json:"kind"`
		Query         string `json:"query"`
		ResultSummary string `json:"result_summary"`
		Source        string `json:"source"`
	} `json:"evidence"`
}

// Analyze implements Analyzer.
func (a AgentAnalyzer) Analyze(ctx context.Context, in AnalysisInput) (Analysis, error) {
	if strings.TrimSpace(a.URL) == "" {
		return Analysis{}, fmt.Errorf("incident: agent analyzer URL is not configured")
	}
	timeout := 120 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 && remaining < timeout {
			timeout = remaining
		}
	}
	request := agentInvestigationRequest{
		ExternalRef: "doctor:" + in.Incident.ID,
		Incident: agentIncident{
			ID:        in.Incident.ID,
			Partition: in.Incident.Partition,
			Title:     in.Incident.TitleText(),
			Severity:  in.Incident.Severity,
			State:     in.Incident.State,
			OpenedAt:  in.Incident.OpenedAt,
			Timeline:  in.Incident.Timeline,
		},
		DeadlineSeconds: int(timeout / time.Second),
	}
	for _, alert := range in.Alerts {
		request.Alerts = append(request.Alerts, agentAlert{
			Fingerprint: alert.Fingerprint,
			Alertname:   alert.Alertname,
			Status:      alert.Status,
			Severity:    alert.Severity,
			Labels:      alert.Labels,
			StartsAt:    alert.StartsAt,
		})
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return Analysis{}, err
	}
	endpoint := strings.TrimRight(strings.TrimSpace(a.URL), "/") + "/v1/investigations"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return Analysis{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "doctor-sentinel")
	if strings.TrimSpace(a.Token) != "" {
		req.Header.Set("Authorization", "Bearer "+a.Token)
		req.Header.Set("X-Nightwatch-Token", a.Token)
	}
	req.Header.Set("X-Nightwatch-Partition-Access", in.Incident.Partition)
	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Analysis{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Analysis{}, fmt.Errorf("incident: agent analysis HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed agentInvestigationResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Analysis{}, err
	}
	root := strings.TrimSpace(strings.Join(nonEmpty(parsed.RootCausePartition, parsed.RootCauseService), "/"))
	analysis := Analysis{
		Summary:     parsed.Summary,
		RootCause:   strings.TrimSpace(strings.TrimSpace(root) + " " + parsed.Rationale),
		Confidence:  parsed.Confidence,
		Provider:    AnalysisAgent,
		Model:       parsed.Model,
		GeneratedAt: time.Now().UTC(),
	}
	analysis.Impact = fmt.Sprintf("%d member alert(s) across partition %s", len(in.Alerts), in.Incident.Partition)
	if parsed.RecommendedAction != "" {
		analysis.ActionItems = append(analysis.ActionItems, ActionItem{
			Title:      parsed.RecommendedAction,
			Category:   "recommendation",
			Target:     parsed.RecommendationTarget,
			Confidence: parsed.Confidence,
		})
	}
	for _, improvement := range parsed.Improvements {
		analysis.ActionItems = append(analysis.ActionItems, ActionItem{
			Title:      improvement.Title,
			Category:   improvement.Category,
			Rationale:  firstNonEmpty(improvement.Rationale, improvement.ProposedChange),
			Target:     improvement.Target,
			Confidence: improvement.Confidence,
		})
	}
	for _, evidence := range parsed.Evidence {
		analysis.Evidence = append(analysis.Evidence, EvidenceRef{
			Kind:    evidence.Kind,
			Query:   evidence.Query,
			Summary: evidence.ResultSummary,
			Source:  evidence.Source,
		})
	}
	if parsed.Rationale != "" {
		analysis.ContributingFactors = append(analysis.ContributingFactors, parsed.Rationale)
	}
	return analysis, nil
}

func nonEmpty(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	return out
}

// Engine names.
const (
	EngineBuiltin    = "builtin"
	EngineWebhook    = "webhook"
	EngineGoogleDocs = "google-docs"
)
