package alerting

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/radryc/doctor/internal/buildinfo"
	"sigs.k8s.io/yaml"
)

// alertmanagerStartedAt is reported through the Alertmanager-compatible status
// endpoint's uptime field.
var alertmanagerStartedAt = time.Now().UTC()

const alertmanagerDefaultReceiver = "doctor-sentinel"

// Alertmanager v2 wire types. The Grafana Alertmanager data source speaks this
// API for the external "prometheus" implementation.

type amVersionInfo struct {
	Version   string `json:"version"`
	Revision  string `json:"revision"`
	Branch    string `json:"branch"`
	BuildUser string `json:"buildUser"`
	BuildDate string `json:"buildDate"`
	GoVersion string `json:"goVersion"`
}

type amClusterStatus struct {
	Name   string `json:"name,omitempty"`
	Status string `json:"status"`
	Peers  []any  `json:"peers"`
}

type amConfig struct {
	Original string `json:"original"`
}

type amStatus struct {
	Cluster     amClusterStatus `json:"cluster"`
	VersionInfo amVersionInfo   `json:"versionInfo"`
	Config      amConfig        `json:"config"`
	Uptime      time.Time       `json:"uptime"`
}

type amAlertStatus struct {
	State       string   `json:"state"`
	SilencedBy  []string `json:"silencedBy"`
	InhibitedBy []string `json:"inhibitedBy"`
	MutedBy     []string `json:"mutedBy"`
}

type amReceiverReference struct {
	Name string `json:"name"`
}

type amAlert struct {
	Labels       map[string]string     `json:"labels"`
	Annotations  map[string]string     `json:"annotations"`
	StartsAt     time.Time             `json:"startsAt"`
	UpdatedAt    time.Time             `json:"updatedAt"`
	EndsAt       time.Time             `json:"endsAt"`
	GeneratorURL string                `json:"generatorURL,omitempty"`
	Fingerprint  string                `json:"fingerprint"`
	Receivers    []amReceiverReference `json:"receivers"`
	Status       amAlertStatus         `json:"status"`
}

type amAlertGroup struct {
	Labels      map[string]string   `json:"labels"`
	RouteLabels map[string]string   `json:"routeLabels"`
	Receiver    amReceiverReference `json:"receiver"`
	Alerts      []amAlert           `json:"alerts"`
}

type amMatcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual bool   `json:"isEqual"`
}

type amSilenceStatus struct {
	State string `json:"state"`
}

type amSilence struct {
	ID          string            `json:"id"`
	Matchers    []amMatcher       `json:"matchers"`
	StartsAt    time.Time         `json:"startsAt"`
	EndsAt      time.Time         `json:"endsAt"`
	UpdatedAt   time.Time         `json:"updatedAt"`
	CreatedBy   string            `json:"createdBy"`
	Comment     string            `json:"comment"`
	Annotations map[string]string `json:"annotations"`
	Status      amSilenceStatus   `json:"status"`
}

type alertmanagerRouteConfig struct {
	Receiver  string   `json:"receiver" yaml:"receiver"`
	GroupWait string   `json:"group_wait,omitempty" yaml:"group_wait,omitempty"`
	GroupBy   []string `json:"group_by,omitempty" yaml:"group_by,omitempty"`
}

type alertmanagerReceiverConfig struct {
	Name string `json:"name" yaml:"name"`
}

type alertmanagerConfigDocument struct {
	Route     alertmanagerRouteConfig      `json:"route" yaml:"route"`
	Receivers []alertmanagerReceiverConfig `json:"receivers" yaml:"receivers"`
}

func (a *API) handleAlertmanagerStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tenant := a.alertmanagerTenant(r)
	info := buildinfo.Current()
	writeJSON(w, http.StatusOK, amStatus{
		Cluster: amClusterStatus{Status: "disabled", Peers: []any{}},
		VersionInfo: amVersionInfo{
			Version:   firstNonEmpty(info.Version, "dev"),
			Revision:  firstNonEmpty(info.Revision, "unknown"),
			Branch:    info.Branch,
			BuildDate: info.BuildDate,
			GoVersion: info.GoVersion,
		},
		Config: amConfig{Original: a.alertmanagerConfigYAML(r.Context(), tenant)},
		Uptime: alertmanagerStartedAt,
	})
}

func (a *API) handleAlertmanagerAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tenant := a.alertmanagerTenant(r)
	identity := a.identity(r)
	alerts, err := a.Store.ListTenantAlerts(r.Context(), tenant)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	silences, err := a.Store.ListTenantSilences(r.Context(), tenant)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	matchers, err := parseAlertmanagerMatchers(r.URL.Query()["filter"])
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	receiverFilter, err := parseReceiverFilter(r.URL.Query().Get("receiver"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := a.now()
	stateFilter := alertmanagerAlertStateFilter{
		active:      queryBoolDefault(r.URL.Query().Get("active"), true),
		silenced:    queryBoolDefault(r.URL.Query().Get("silenced"), true),
		inhibited:   queryBoolDefault(r.URL.Query().Get("inhibited"), true),
		unprocessed: queryBoolDefault(r.URL.Query().Get("unprocessed"), true),
	}
	routingCache := map[string]*PartitionRouting{}
	out := make([]amAlert, 0, len(alerts))
	for _, alert := range alerts {
		if !identity.Can(CapAlertsRead, tenant, alert.Partition) {
			continue
		}
		converted, include := a.alertmanagerAlert(r.Context(), alert, silences, routingCache, matchers, receiverFilter, stateFilter, now)
		if include {
			out = append(out, converted)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Fingerprint < out[j].Fingerprint })
	writeJSON(w, http.StatusOK, out)
}

func (a *API) handleAlertmanagerAlertGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tenant := a.alertmanagerTenant(r)
	identity := a.identity(r)
	alerts, err := a.Store.ListTenantAlerts(r.Context(), tenant)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	silences, err := a.Store.ListTenantSilences(r.Context(), tenant)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	matchers, err := parseAlertmanagerMatchers(r.URL.Query()["filter"])
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	receiverFilter, err := parseReceiverFilter(r.URL.Query().Get("receiver"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := a.now()
	stateFilter := alertmanagerAlertStateFilter{
		active:      queryBoolDefault(r.URL.Query().Get("active"), true),
		silenced:    queryBoolDefault(r.URL.Query().Get("silenced"), true),
		inhibited:   queryBoolDefault(r.URL.Query().Get("inhibited"), true),
		unprocessed: queryBoolDefault(r.URL.Query().Get("unprocessed"), true),
	}
	routingCache := map[string]*PartitionRouting{}
	byPartition := map[string][]amAlert{}
	for _, alert := range alerts {
		if !identity.Can(CapAlertsRead, tenant, alert.Partition) {
			continue
		}
		converted, include := a.alertmanagerAlert(r.Context(), alert, silences, routingCache, matchers, receiverFilter, stateFilter, now)
		if !include {
			continue
		}
		byPartition[alert.Partition] = append(byPartition[alert.Partition], converted)
	}
	partitions := make([]string, 0, len(byPartition))
	for partition := range byPartition {
		partitions = append(partitions, partition)
	}
	sort.Strings(partitions)
	out := make([]amAlertGroup, 0, len(partitions))
	for _, partition := range partitions {
		groupAlerts := byPartition[partition]
		receiver := alertmanagerDefaultReceiver
		if len(groupAlerts) > 0 && len(groupAlerts[0].Receivers) > 0 {
			receiver = groupAlerts[0].Receivers[0].Name
		}
		labels := map[string]string{"partition": partition}
		out = append(out, amAlertGroup{
			Labels:      labels,
			RouteLabels: labels,
			Receiver:    amReceiverReference{Name: receiver},
			Alerts:      groupAlerts,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) handleAlertmanagerSilences(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tenant := a.alertmanagerTenant(r)
	identity := a.identity(r)
	silences, err := a.Store.ListTenantSilences(r.Context(), tenant)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	matchers, err := parseAlertmanagerMatchers(r.URL.Query()["filter"])
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	active := queryBoolDefault(r.URL.Query().Get("active"), true)
	expired := queryBoolDefault(r.URL.Query().Get("expired"), true)
	pending := queryBoolDefault(r.URL.Query().Get("pending"), true)
	now := a.now()
	out := make([]amSilence, 0, len(silences))
	for _, silence := range silences {
		if !identity.Can(CapAlertsRead, tenant, silence.Partition) {
			continue
		}
		converted := alertmanagerSilence(silence, now)
		switch converted.Status.State {
		case "active":
			if !active {
				continue
			}
		case "expired":
			if !expired {
				continue
			}
		case "pending":
			if !pending {
				continue
			}
		}
		if !silenceMatchesFilter(silence, matchers) {
			continue
		}
		out = append(out, converted)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Status.State != out[j].Status.State {
			return silenceStateRank(out[i].Status.State) < silenceStateRank(out[j].Status.State)
		}
		return out[i].EndsAt.Before(out[j].EndsAt)
	})
	writeJSON(w, http.StatusOK, out)
}

func (a *API) handleAlertmanagerSilence(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tenant := a.alertmanagerTenant(r)
	identity := a.identity(r)
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v2/silence/"), "/")
	if id == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	silences, err := a.Store.ListTenantSilences(r.Context(), tenant)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, silence := range silences {
		if silence.ID != id {
			continue
		}
		if !identity.Can(CapAlertsRead, tenant, silence.Partition) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeJSON(w, http.StatusOK, alertmanagerSilence(silence, a.now()))
		return
	}
	writeError(w, http.StatusNotFound, "not found")
}

type alertmanagerAlertStateFilter struct {
	active      bool
	silenced    bool
	inhibited   bool
	unprocessed bool
}

func (a *API) alertmanagerAlert(ctx context.Context, alert Alert, silences []Silence, routingCache map[string]*PartitionRouting, matchers []Matcher, receiverFilter *regexp.Regexp, stateFilter alertmanagerAlertStateFilter, now time.Time) (amAlert, bool) {
	if alert.Status == StatusResolved {
		return amAlert{}, false
	}
	if alert.EndsAt != nil && alert.EndsAt.Before(now) && alert.Status == StatusFiring {
		return amAlert{}, false
	}
	if !matchAlertmanagerMatchers(matchers, alert.Labels) {
		return amAlert{}, false
	}
	routing, ok := routingCache[alert.Partition]
	if !ok {
		cfg, err := a.Store.GetPartitionRouting(ctx, alert.Tenant, alert.Partition)
		if err != nil {
			cfg = nil
		}
		routing = cfg
		routingCache[alert.Partition] = cfg
	}
	receiverNames := matchingReceivers(routing, alert)
	receivers := make([]amReceiverReference, 0, len(receiverNames))
	for _, name := range receiverNames {
		receivers = append(receivers, amReceiverReference{Name: name})
	}
	if receiverFilter != nil {
		matched := false
		for _, name := range receiverNames {
			if receiverFilter.MatchString(name) {
				matched = true
				break
			}
		}
		if !matched {
			return amAlert{}, false
		}
	}
	silencedBy := make([]string, 0)
	for _, silence := range silences {
		if silence.Active(now) && silence.Matches(alert) {
			silencedBy = append(silencedBy, silence.ID)
		}
	}
	sort.Strings(silencedBy)
	state := "active"
	switch {
	case alert.Status == StatusPending:
		state = "unprocessed"
		if !stateFilter.unprocessed {
			return amAlert{}, false
		}
	case alert.Status == StatusSilenced:
		state = "suppressed"
		if len(silencedBy) == 0 {
			silencedBy = append(silencedBy, alert.SilenceIDs...)
			sort.Strings(silencedBy)
		}
		if !stateFilter.silenced {
			return amAlert{}, false
		}
	case len(silencedBy) > 0:
		state = "suppressed"
		if !stateFilter.silenced {
			return amAlert{}, false
		}
	default:
		if !stateFilter.active {
			return amAlert{}, false
		}
	}
	labels := make(map[string]string, len(alert.Labels)+3)
	for key, value := range alert.Labels {
		labels[key] = value
	}
	setIfMissing(labels, "alertname", alert.Labels["alertname"])
	setIfMissing(labels, "partition", alert.Partition)
	setIfMissing(labels, "tenant", alert.Tenant)
	setIfMissing(labels, "source", alert.Source)
	setIfMissing(labels, "severity", alert.Severity)
	annotations := make(map[string]string, len(alert.Annotations))
	for key, value := range alert.Annotations {
		annotations[key] = value
	}
	endsAt := time.Time{}
	if alert.EndsAt != nil {
		endsAt = *alert.EndsAt
	}
	generatorURL := ""
	if strings.HasPrefix(annotations["guardian.url"], "http") {
		generatorURL = annotations["guardian.url"]
	}
	return amAlert{
		Labels:       labels,
		Annotations:  annotations,
		StartsAt:     alert.StartsAt,
		UpdatedAt:    alert.UpdatedAt,
		EndsAt:       endsAt,
		GeneratorURL: generatorURL,
		Fingerprint:  firstNonEmpty(alert.Fingerprint, alert.ID),
		Receivers:    receivers,
		Status: amAlertStatus{
			State:       state,
			SilencedBy:  silencedBy,
			InhibitedBy: []string{},
			MutedBy:     []string{},
		},
	}, true
}

func (a *API) alertmanagerConfigYAML(ctx context.Context, tenant string) string {
	names := map[string]struct{}{}
	if routings, err := a.Store.ListPartitionRouting(ctx, tenant); err == nil {
		for _, routing := range routings {
			for _, receiver := range routing.Receivers {
				names[receiver.Name] = struct{}{}
			}
		}
	}
	if len(names) == 0 {
		if cfg, err := a.Store.GetReceiverConfig(ctx); err == nil {
			for _, receiver := range cfg.Receivers {
				names[receiver.Name] = struct{}{}
			}
		}
	}
	receiverNames := make([]string, 0, len(names))
	for name := range names {
		receiverNames = append(receiverNames, name)
	}
	sort.Strings(receiverNames)
	if len(receiverNames) == 0 {
		receiverNames = []string{alertmanagerDefaultReceiver}
	}
	receivers := make([]alertmanagerReceiverConfig, 0, len(receiverNames))
	for _, name := range receiverNames {
		receivers = append(receivers, alertmanagerReceiverConfig{Name: name})
	}
	doc := alertmanagerConfigDocument{
		Route: alertmanagerRouteConfig{
			Receiver: receiverNames[0],
			GroupBy:  []string{"partition", "alertname"},
		},
		Receivers: receivers,
	}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return "route:\n  receiver: " + alertmanagerDefaultReceiver + "\nreceivers:\n  - name: " + alertmanagerDefaultReceiver + "\n"
	}
	return string(data)
}

func (a *API) alertmanagerTenant(r *http.Request) string {
	tenant := strings.TrimSpace(r.Header.Get("X-Doctor-Tenant"))
	if tenant == "" {
		tenant = strings.TrimSpace(r.URL.Query().Get("tenant"))
	}
	if tenant == "" && a.Service != nil {
		tenant = strings.TrimSpace(a.Service.DefaultTenant)
	}
	if tenant == "" {
		tenant = "default"
	}
	return tenant
}

func alertmanagerSilence(silence Silence, now time.Time) amSilence {
	state := "active"
	switch {
	case now.Before(silence.StartsAt):
		state = "pending"
	case !now.Before(silence.EndsAt):
		state = "expired"
	}
	return amSilence{
		ID:          silence.ID,
		Matchers:    alertmanagerMatchers(silence.Matchers),
		StartsAt:    silence.StartsAt,
		EndsAt:      silence.EndsAt,
		UpdatedAt:   silence.UpdatedAt,
		CreatedBy:   silence.CreatedBy,
		Comment:     silence.Comment,
		Annotations: map[string]string{},
		Status:      amSilenceStatus{State: state},
	}
}

func alertmanagerMatchers(matchers []Matcher) []amMatcher {
	out := make([]amMatcher, 0, len(matchers))
	for _, matcher := range matchers {
		operator := strings.TrimSpace(matcher.Operator)
		if operator == "" {
			operator = "="
		}
		out = append(out, amMatcher{
			Name:    matcher.Name,
			Value:   matcher.Value,
			IsRegex: operator == "=~" || operator == "!~",
			IsEqual: operator == "=" || operator == "==" || operator == "=~",
		})
	}
	return out
}

func silenceStateRank(state string) int {
	switch state {
	case "active":
		return 1
	case "pending":
		return 2
	default:
		return 3
	}
}

var alertmanagerMatcherPattern = regexp.MustCompile(`^\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*(=~|!~|!=|=)\s*(?:"((?:\\.|[^"])*)"|'([^']*)'|(\S+))\s*$`)

func parseAlertmanagerMatchers(filters []string) ([]Matcher, error) {
	out := make([]Matcher, 0, len(filters))
	for _, filter := range filters {
		filter = strings.TrimSpace(filter)
		if filter == "" {
			continue
		}
		matches := alertmanagerMatcherPattern.FindStringSubmatch(filter)
		if matches == nil {
			return nil, fmt.Errorf("invalid matcher %q", filter)
		}
		value := matches[3]
		if matches[4] != "" {
			value = matches[4]
		} else if matches[5] != "" {
			value = matches[5]
		} else {
			unquoted, err := strconv.Unquote(`"` + value + `"`)
			if err == nil {
				value = unquoted
			}
		}
		matcher := Matcher{Name: matches[1], Operator: matches[2], Value: value}
		if matcher.Operator == "=~" || matcher.Operator == "!~" {
			if _, err := regexp.Compile(matcher.Value); err != nil {
				return nil, fmt.Errorf("invalid matcher %q: %w", filter, err)
			}
		}
		out = append(out, matcher)
	}
	return out, nil
}

func parseReceiverFilter(value string) (*regexp.Regexp, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	compiled, err := regexp.Compile("^(?:" + value + ")$")
	if err != nil {
		return nil, fmt.Errorf("invalid receiver filter: %w", err)
	}
	return compiled, nil
}

func matchAlertmanagerMatchers(matchers []Matcher, labels map[string]string) bool {
	for _, matcher := range matchers {
		if !matcher.Matches(labels) {
			return false
		}
	}
	return true
}

func silenceMatchesFilter(silence Silence, filters []Matcher) bool {
	if len(filters) == 0 {
		return true
	}
	for _, filter := range filters {
		found := false
		filterOperator := strings.TrimSpace(filter.Operator)
		if filterOperator == "" {
			filterOperator = "="
		}
		for _, matcher := range silence.Matchers {
			operator := strings.TrimSpace(matcher.Operator)
			if operator == "" {
				operator = "="
			}
			if matcher.Name == filter.Name && operator == filterOperator && matcher.Value == filter.Value {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func queryBoolDefault(value string, fallback bool) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func setIfMissing(labels map[string]string, key, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	if _, ok := labels[key]; !ok {
		labels[key] = value
	}
}
