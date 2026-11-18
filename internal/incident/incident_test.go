package incident

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/alerting"
)

type fixture struct {
	files   *alerting.MemoryStore
	alerts  *alerting.Store
	store   *Store
	service *Service
	now     time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	files := alerting.NewMemoryStore()
	alertStore := alerting.NewStore(files, "", "/doctor/v1/alerts")
	store := NewStore(files, alertStore, "", "/doctor/v1/alerts")
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	service := &Service{
		Store:     store,
		Alerts:    alertStore,
		Engines:   NewEngineRegistry(),
		Analyzers: map[string]Analyzer{},
		Now:       func() time.Time { return now },
		Delivery:  false,
	}
	return &fixture{files: files, alerts: alertStore, store: store, service: service, now: now}
}

func (f *fixture) seedSet(t *testing.T, partition, name string) AlertSet {
	t.Helper()
	set := AlertSet{
		Tenant:        "default",
		Partition:     partition,
		Name:          name,
		SeverityFloor: "critical",
		Match:         []alerting.Matcher{{Name: "severity", Operator: "=", Value: "critical"}},
		GroupBy:       []string{"partition", "service"},
		Postmortem: PostmortemSpec{
			Engine:           EngineBuiltin,
			AnalysisProvider: AnalysisNone,
		},
	}
	if err := set.Validate(); err != nil {
		t.Fatalf("validate set: %v", err)
	}
	if err := f.store.PutAlertSet(context.Background(), set, "absent"); err != nil {
		t.Fatalf("put set: %v", err)
	}
	return set
}

func (f *fixture) seedAlert(t *testing.T, fingerprint, name, service, status string) {
	t.Helper()
	alert := alerting.Alert{
		ID:          fingerprint,
		Fingerprint: fingerprint,
		Source:      "test",
		Tenant:      "default",
		Partition:   "doctor",
		Status:      status,
		Severity:    "critical",
		Labels: map[string]string{
			"alertname": name,
			"severity":  "critical",
			"service":   service,
			"partition": "doctor",
		},
		StartsAt:  f.now,
		CreatedAt: f.now,
		UpdatedAt: f.now,
	}
	if err := f.alerts.SaveAlert(context.Background(), alert, nil); err != nil {
		t.Fatalf("save alert: %v", err)
	}
}

func TestPolicyDefaultsToThirtyMinutes(t *testing.T) {
	f := newFixture(t)
	policy, err := f.store.GetPolicy(context.Background(), "default", "doctor")
	if err != nil {
		t.Fatal(err)
	}
	if got := duration(policy.MitigateDelay); got != 30*time.Minute {
		t.Fatalf("mitigateDelay = %s, want 30m", got)
	}
	if !policy.AutoMitigateEnabled() {
		t.Fatal("autoMitigate should default true")
	}
}

func TestPolicyIsPerPartition(t *testing.T) {
	f := newFixture(t)
	short := Policy{MitigateDelay: alerting.Duration(5 * time.Minute)}
	if err := f.store.PutPolicy(context.Background(), "default", "payments", short); err != nil {
		t.Fatal(err)
	}
	payments, _ := f.store.GetPolicy(context.Background(), "default", "payments")
	doctor, _ := f.store.GetPolicy(context.Background(), "default", "doctor")
	if duration(payments.MitigateDelay) != 5*time.Minute {
		t.Fatalf("payments = %s", duration(payments.MitigateDelay))
	}
	if duration(doctor.MitigateDelay) != 30*time.Minute {
		t.Fatalf("doctor = %s", duration(doctor.MitigateDelay))
	}
}

func TestAckByIDFindsIncidentAcrossPartitions(t *testing.T) {
	f := newFixture(t)
	f.seedSet(t, "doctor", "platform-critical")
	f.seedAlert(t, "fp1", "CheckoutDown", "checkout", alerting.StatusFiring)
	ctx := context.Background()
	if err := f.service.ReconcileTenant(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	incidents, err := f.store.ListTenantIncidents(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 1 {
		t.Fatalf("incidents = %d, want 1", len(incidents))
	}
	id := incidents[0].ID

	found, err := f.service.AckByID(ctx, "default", id, "alice", true)
	if err != nil || !found {
		t.Fatalf("AckByID ack = found %t err %v", found, err)
	}
	incident, _, err := f.store.GetIncident(ctx, "default", "doctor", id)
	if err != nil {
		t.Fatal(err)
	}
	if incident.State != StateAcknowledged || incident.Owner != "alice" {
		t.Fatalf("incident after ack = %+v", incident)
	}

	found, err = f.service.AckByID(ctx, "default", id, "alice", false)
	if err != nil || !found {
		t.Fatalf("AckByID nack = found %t err %v", found, err)
	}
	incident, _, err = f.store.GetIncident(ctx, "default", "doctor", id)
	if err != nil {
		t.Fatal(err)
	}
	if incident.State != StateOpen || incident.Owner != "" {
		t.Fatalf("incident after nack = %+v", incident)
	}

	if found, err := f.service.AckByID(ctx, "default", "missing", "alice", true); err != nil || found {
		t.Fatalf("AckByID missing = found %t err %v", found, err)
	}
}

func TestReconcileOpensThenAutoMitigatesAfterThirtyMinutes(t *testing.T) {
	f := newFixture(t)
	f.seedSet(t, "doctor", "platform-critical")
	f.seedAlert(t, "fp1", "CheckoutDown", "checkout", alerting.StatusFiring)
	ctx := context.Background()

	if err := f.service.ReconcileTenant(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	incidents, _ := f.store.ListIncidents(ctx, "default", "doctor")
	if len(incidents) != 1 {
		t.Fatalf("incidents = %d, want 1", len(incidents))
	}
	incident := incidents[0]
	if incident.State != StateOpen {
		t.Fatalf("state = %s, want open", incident.State)
	}
	if incident.GroupKey != "partition=doctor,service=checkout" {
		t.Fatalf("group key = %q", incident.GroupKey)
	}

	// Resolve the alert; the 30 minute countdown starts at the next reconcile.
	f.seedAlert(t, "fp1", "CheckoutDown", "checkout", alerting.StatusResolved)
	if err := f.service.ReconcileTenant(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	incident = mustIncident(t, f, incident.ID)
	if incident.State != StateOpen || incident.MitigateDueAt == nil {
		t.Fatalf("expected open with mitigate due, got state=%s due=%v", incident.State, incident.MitigateDueAt)
	}

	f.now = f.now.Add(29 * time.Minute)
	f.service.Now = func() time.Time { return f.now }
	if err := f.service.ReconcileTenant(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	incident = mustIncident(t, f, incident.ID)
	if incident.State != StateOpen {
		t.Fatalf("state = %s, want open before 30m", incident.State)
	}

	f.now = f.now.Add(2 * time.Minute)
	f.service.Now = func() time.Time { return f.now }
	if err := f.service.ReconcileTenant(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	incident = mustIncident(t, f, incident.ID)
	if incident.State != StateMitigated {
		t.Fatalf("state = %s, want mitigated", incident.State)
	}
	if !incident.AutoMitigated {
		t.Fatal("expected automatic mitigation")
	}
}

func TestRefireBeforeMitigationCancelsCountdown(t *testing.T) {
	f := newFixture(t)
	f.seedSet(t, "doctor", "platform-critical")
	f.seedAlert(t, "fp1", "CheckoutDown", "checkout", alerting.StatusFiring)
	ctx := context.Background()
	_ = f.service.ReconcileTenant(ctx, "default")
	incidents, _ := f.store.ListIncidents(ctx, "default", "doctor")
	incident := incidents[0]

	f.seedAlert(t, "fp1", "CheckoutDown", "checkout", alerting.StatusResolved)
	f.now = f.now.Add(10 * time.Minute)
	f.service.Now = func() time.Time { return f.now }
	_ = f.service.ReconcileTenant(ctx, "default")

	f.seedAlert(t, "fp1", "CheckoutDown", "checkout", alerting.StatusFiring)
	f.now = f.now.Add(5 * time.Minute)
	f.service.Now = func() time.Time { return f.now }
	_ = f.service.ReconcileTenant(ctx, "default")

	updated := mustIncident(t, f, incident.ID)
	if updated.MembersClearAt != nil || updated.MitigateDueAt != nil {
		t.Fatalf("expected cleared countdown, got clear=%v due=%v", updated.MembersClearAt, updated.MitigateDueAt)
	}
	if updated.State != StateOpen {
		t.Fatalf("state = %s, want open", updated.State)
	}
}

func TestResolveRequiresMitigatedAndCreatesPostmortem(t *testing.T) {
	f := newFixture(t)
	f.seedSet(t, "doctor", "platform-critical")
	f.seedAlert(t, "fp1", "CheckoutDown", "checkout", alerting.StatusFiring)
	ctx := context.Background()
	_ = f.service.ReconcileTenant(ctx, "default")
	incidents, _ := f.store.ListIncidents(ctx, "default", "doctor")
	incident := incidents[0]

	if _, err := f.service.Resolve(ctx, "default", "doctor", incident.ID, "ada", "done"); !errors.Is(err, ErrResolveFromOpen) {
		t.Fatalf("resolve from open err = %v, want ErrResolveFromOpen", err)
	}

	f.seedAlert(t, "fp1", "CheckoutDown", "checkout", alerting.StatusResolved)
	_ = f.service.ReconcileTenant(ctx, "default")
	f.now = f.now.Add(31 * time.Minute)
	f.service.Now = func() time.Time { return f.now }
	_ = f.service.ReconcileTenant(ctx, "default")
	incident = mustIncident(t, f, incident.ID)
	if incident.State != StateMitigated {
		t.Fatalf("pre-resolve state = %s", incident.State)
	}

	resolved, err := f.service.Resolve(ctx, "default", "doctor", incident.ID, "ada", "rolled back deploy")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != StateResolved {
		t.Fatalf("state = %s, want resolved", resolved.State)
	}
	doc, err := f.service.Postmortem(ctx, "default", "doctor", incident.ID)
	if err != nil {
		t.Fatal(err)
	}
	if doc == nil || !strings.Contains(doc.Markdown, "CheckoutDown") {
		t.Fatalf("postmortem draft missing incident title: %#v", doc)
	}
	if doc.Status != PostmortemDraft {
		t.Fatalf("postmortem status = %s, want draft (analysis none)", doc.Status)
	}
}

func TestManualMitigateRejectsFiringAlerts(t *testing.T) {
	f := newFixture(t)
	f.seedSet(t, "doctor", "platform-critical")
	f.seedAlert(t, "fp1", "CheckoutDown", "checkout", alerting.StatusFiring)
	ctx := context.Background()
	_ = f.service.ReconcileTenant(ctx, "default")
	incidents, _ := f.store.ListIncidents(ctx, "default", "doctor")
	incident := incidents[0]

	if _, err := f.service.Mitigate(ctx, "default", "doctor", incident.ID, "ada", "manual", false); !errors.Is(err, ErrAlertsFiring) {
		t.Fatalf("err = %v, want ErrAlertsFiring", err)
	}
	if _, err := f.service.Mitigate(ctx, "default", "doctor", incident.ID, "ada", "manual override", true); err != nil {
		t.Fatalf("override mitigate: %v", err)
	}
	// The still-firing member must not bounce the incident back open.
	if err := f.service.ReconcileTenant(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	updated := mustIncident(t, f, incident.ID)
	if updated.State != StateMitigated {
		t.Fatalf("state after override reconcile = %s, want mitigated", updated.State)
	}
}

func TestGroupByCreatesOneIncidentPerGroup(t *testing.T) {
	f := newFixture(t)
	f.seedSet(t, "doctor", "platform-critical")
	f.seedAlert(t, "fp1", "CheckoutDown", "checkout", alerting.StatusFiring)
	f.seedAlert(t, "fp2", "CartDown", "cart", alerting.StatusFiring)
	ctx := context.Background()
	_ = f.service.ReconcileTenant(ctx, "default")
	incidents, _ := f.store.ListIncidents(ctx, "default", "doctor")
	if len(incidents) != 2 {
		t.Fatalf("incidents = %d, want 2", len(incidents))
	}
}

func TestReopenWithinWindow(t *testing.T) {
	f := newFixture(t)
	f.seedSet(t, "doctor", "platform-critical")
	f.seedAlert(t, "fp1", "CheckoutDown", "checkout", alerting.StatusFiring)
	ctx := context.Background()
	_ = f.service.ReconcileTenant(ctx, "default")
	incidents, _ := f.store.ListIncidents(ctx, "default", "doctor")
	incident := incidents[0]
	f.seedAlert(t, "fp1", "CheckoutDown", "checkout", alerting.StatusResolved)
	_ = f.service.ReconcileTenant(ctx, "default")
	f.now = f.now.Add(31 * time.Minute)
	f.service.Now = func() time.Time { return f.now }
	_ = f.service.ReconcileTenant(ctx, "default")
	if _, err := f.service.Resolve(ctx, "default", "doctor", incident.ID, "ada", "fixed"); err != nil {
		t.Fatal(err)
	}

	reopened, err := f.service.Reopen(ctx, "default", "doctor", incident.ID, "ada")
	if err != nil {
		t.Fatal(err)
	}
	if reopened.State != StateOpen && reopened.State != StateAcknowledged {
		t.Fatalf("state = %s", reopened.State)
	}
}

func TestAgentAnalyzerMapsInvestigation(t *testing.T) {
	server := newAgentStub(t)
	defer server.Close()
	analyzer := AgentAnalyzer{URL: server.URL, Token: "test"}
	analysis, err := analyzer.Analyze(context.Background(), AnalysisInput{
		Incident: Incident{ID: "inc-1", Partition: "doctor", Title: "Checkout", Severity: "critical", State: StateResolved, OpenedAt: time.Now()},
		Alerts:   []MemberAlert{{Fingerprint: "fp1", Alertname: "CheckoutDown", Status: "resolved", Severity: "critical"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if analysis.RootCause == "" || analysis.Summary == "" {
		t.Fatalf("analysis missing content: %#v", analysis)
	}
	if len(analysis.ActionItems) == 0 {
		t.Fatalf("expected action items from improvements")
	}
	if analysis.Provider != AnalysisAgent {
		t.Fatalf("provider = %s", analysis.Provider)
	}
}

func mustIncident(t *testing.T, f *fixture, id string) Incident {
	t.Helper()
	incident, _, err := f.store.GetIncident(context.Background(), "default", "doctor", id)
	if err != nil || incident == nil {
		t.Fatalf("get incident %s: %v", id, err)
	}
	return *incident
}
