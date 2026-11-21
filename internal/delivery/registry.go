package delivery

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/radryc/doctor/internal/netguard"
)

// Runtime carries shared HTTP settings into a sender.
type Runtime struct {
	Client    *http.Client
	URLPolicy *netguard.Policy
	// FCB configures the optional fcm-bridge backend used by fcb receivers.
	FCB FCBConfig
}

// Sender is the extension point. Implement it in a new file under
// internal/delivery (or an external package) and register it.
type Sender interface {
	// Type is the persisted receiver type ("webhook", "android", ...).
	Type() string
	// Kinds lists the event kinds this transport can render.
	Kinds() []string
	// Validate checks the receiver configuration before it is saved.
	Validate(receiver Receiver) error
	// Send delivers one message.
	Send(ctx context.Context, runtime Runtime, receiver Receiver, message Message) error
}

// Registry maps receiver types to senders.
type Registry struct {
	mu      sync.RWMutex
	senders map[string]Sender
}

// NewRegistry builds a registry with the supplied senders.
func NewRegistry(senders ...Sender) *Registry {
	registry := &Registry{senders: map[string]Sender{}}
	for _, sender := range senders {
		registry.Register(sender)
	}
	return registry
}

// Register adds or replaces one sender. It is safe to call from init().
func (r *Registry) Register(sender Sender) {
	if sender == nil {
		return
	}
	name := strings.ToLower(strings.TrimSpace(sender.Type()))
	if name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.senders == nil {
		r.senders = map[string]Sender{}
	}
	r.senders[name] = sender
}

// Get returns the sender for a receiver type.
func (r *Registry) Get(receiverType string) Sender {
	receiverType = strings.ToLower(strings.TrimSpace(receiverType))
	if receiverType == "" {
		receiverType = "webhook"
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.senders[receiverType]
}

// Types lists registered receiver types.
func (r *Registry) Types() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.senders))
	for name := range r.senders {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Supports reports whether a receiver accepts a kind in this registry.
func (r *Registry) Supports(receiver Receiver, kind string) bool {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == "" {
		return true
	}
	if len(receiver.Events) > 0 {
		allowed := false
		for _, event := range receiver.Events {
			if strings.EqualFold(strings.TrimSpace(event), kind) {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	sender := r.Get(receiver.Type)
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

// Send dispatches a message through the matching sender.
func (r *Registry) Send(ctx context.Context, runtime Runtime, receiver Receiver, message Message) error {
	sender := r.Get(receiver.Type)
	if sender == nil {
		return fmt.Errorf("delivery: unsupported receiver type %q", receiver.Type)
	}
	return sender.Send(ctx, runtime, receiver, message)
}

var (
	defaultRegistry     *Registry
	defaultRegistryOnce sync.Once
)

// DefaultRegistry returns the process-wide registry with all built-in
// receivers. Extensions may call Register before serving traffic.
func DefaultRegistry() *Registry {
	defaultRegistryOnce.Do(func() {
		defaultRegistry = NewRegistry(
			WebhookSender{},
			SlackSender{},
			TeamsSender{},
			ZoomSender{},
			GoAlertSender{},
			PagerDutySender{},
			AndroidSender{},
			FCBSender{},
		)
	})
	return defaultRegistry
}

// Register adds a sender to the default registry. Call it from an init()
// function in a new receiver file.
func Register(sender Sender) {
	DefaultRegistry().Register(sender)
}
