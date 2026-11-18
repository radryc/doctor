// Package delivery implements Doctor's notification receiver plugins.
//
// A receiver is a transport adapter: webhook, Slack, Teams, Zoom, GoAlert,
// PagerDuty, Android, or a new type registered by an extension. Alerts and
// incidents share the same outbox and dispatcher, but each sender receives a
// kind-aware Message so incident information is never forced into alert labels.
package delivery

import (
	"fmt"
	"os"
	"strings"
)

// Event kinds.
const (
	KindAlert    = "alert"
	KindIncident = "incident"
)

// Receiver is the persisted notification channel configuration. URLEnv keeps
// webhook credentials out of config; Events optionally narrows the kinds a
// channel accepts.
type Receiver struct {
	Name        string `json:"name" yaml:"name"`
	Type        string `json:"type" yaml:"type"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	// Target is a transport-specific destination. For the fcb receiver it is
	// the FCM topic that receives the notification; devices join that topic
	// through the companion app or the FCB backend.
	Target    string            `json:"target,omitempty" yaml:"target,omitempty"`
	URL       string            `json:"url,omitempty" yaml:"url,omitempty"`
	URLEnv    string            `json:"urlEnv,omitempty" yaml:"urlEnv,omitempty"`
	SecretEnv string            `json:"secretEnv,omitempty" yaml:"secretEnv,omitempty"`
	Headers   map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	Timeout   string            `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	Events    []string          `json:"events,omitempty" yaml:"events,omitempty"`
	Format    map[string]string `json:"format,omitempty" yaml:"format,omitempty"`
	Templates map[string]string `json:"templates,omitempty" yaml:"templates,omitempty"`
}

// URLValue resolves the endpoint, preferring the environment variable when set.
func (r Receiver) URLValue() string {
	if env := strings.TrimSpace(r.URLEnv); env != "" {
		if value := strings.TrimSpace(os.Getenv(env)); value != "" {
			return value
		}
	}
	return strings.TrimSpace(r.URL)
}

// SecretValue resolves the HMAC/routing secret from the environment.
func (r Receiver) SecretValue() string {
	if env := strings.TrimSpace(r.SecretEnv); env != "" {
		return strings.TrimSpace(os.Getenv(env))
	}
	return ""
}

// Validate checks the receiver's transport-level requirements using the
// default registry. Unknown types are rejected so a typo does not silently
// drop notifications.
func (r Receiver) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("delivery: receiver has no name")
	}
	receiverType := strings.ToLower(strings.TrimSpace(r.Type))
	if receiverType == "" {
		receiverType = "webhook"
	}
	sender := DefaultRegistry().Get(receiverType)
	if sender == nil {
		return fmt.Errorf("delivery: unsupported receiver type %q", r.Type)
	}
	for _, event := range r.Events {
		switch strings.ToLower(strings.TrimSpace(event)) {
		case KindAlert, KindIncident:
		default:
			return fmt.Errorf("delivery: receiver %q has unknown event kind %q", r.Name, event)
		}
	}
	if err := sender.Validate(r); err != nil {
		return fmt.Errorf("delivery: receiver %q: %w", r.Name, err)
	}
	return nil
}

// Supports reports whether the receiver accepts a kind. An empty Events list
// means every kind its transport supports.
func (r Receiver) Supports(kind string) bool {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == "" {
		return true
	}
	if len(r.Events) > 0 {
		for _, allowed := range r.Events {
			if strings.EqualFold(strings.TrimSpace(allowed), kind) {
				// An explicit allowlist still cannot make a transport accept a
				// kind it has no renderer for.
				return senderSupports(r.Type, kind)
			}
		}
		return false
	}
	return senderSupports(r.Type, kind)
}

func senderSupports(receiverType, kind string) bool {
	receiverType = strings.ToLower(strings.TrimSpace(receiverType))
	if receiverType == "" {
		receiverType = "webhook"
	}
	sender := DefaultRegistry().Get(receiverType)
	if sender == nil {
		return false
	}
	for _, supported := range sender.Kinds() {
		if strings.EqualFold(supported, kind) {
			return true
		}
	}
	return false
}

// IncidentCapable reports whether a receiver type has an incident renderer.
func IncidentCapable(receiverType string) bool {
	return senderSupports(receiverType, KindIncident)
}

// ReceiverSupports reports whether a configured receiver accepts a kind.
func ReceiverSupports(receiver Receiver, kind string) bool {
	return receiver.Supports(kind)
}
