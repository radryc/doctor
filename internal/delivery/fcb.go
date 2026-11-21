package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	// maxFCBTopicLength bounds the topic name accepted by FCM.
	maxFCBTopicLength = 900
)

// fcmTopicPattern is the character set FCM accepts for topic names.
var fcmTopicPattern = regexp.MustCompile(`^[A-Za-z0-9_.~%-]+$`)

// FCBConfig configures the optional fcm-bridge backend for fcb receivers.
// The zero value disables the global fallback; a receiver may still set its
// own url/urlEnv.
type FCBConfig struct {
	Enabled       bool
	BackendURL    string
	WebhookSecret string
	DefaultTopic  string
	Timeout       time.Duration
}

// FCBSender delivers doctor.android/v1 payloads to an fcm-bridge (FCB).
// A receiver's target is the FCM topic; devices join that topic through the
// companion app or the FCB backend.
type FCBSender struct{}

// Type implements Sender.
func (FCBSender) Type() string { return "fcb" }

// Kinds implements Sender.
func (FCBSender) Kinds() []string { return []string{KindAlert, KindIncident} }

// Validate implements Sender.
func (FCBSender) Validate(receiver Receiver) error {
	topic := fcbTopic(receiver, "")
	if topic == "" {
		// No target means "use the backend default": the on-call rotation for
		// notfify/fcm-bridge, or its configured default topic. A direct device
		// token in X-FCM-Token is also valid.
		return nil
	}
	return validateFCMTopic(topic)
}

// Send implements Sender.
func (FCBSender) Send(ctx context.Context, runtime Runtime, receiver Receiver, message Message) error {
	endpoint := receiver.URLValue()
	if endpoint == "" {
		if !runtime.FCB.Enabled || strings.TrimSpace(runtime.FCB.BackendURL) == "" {
			return fmt.Errorf("delivery: receiver %q: fcb backend not configured (set DOCTOR_SENTINEL_FCB_BACKEND_URL or receiver url/urlEnv)", receiver.Name)
		}
		endpoint = strings.TrimSpace(runtime.FCB.BackendURL)
	}

	// An explicit receiver secretEnv is an override: when it is set but the
	// env var is missing, send unsigned rather than silently using the global
	// secret.
	secret := receiver.SecretValue()
	if strings.TrimSpace(receiver.SecretEnv) == "" {
		secret = runtime.FCB.WebhookSecret
	}

	topic := fcbTopic(receiver, "")
	if topic == "" && headerValue(receiver.Headers, "X-FCM-Token") == "" {
		topic = strings.TrimSpace(runtime.FCB.DefaultTopic)
	}
	if topic != "" {
		if err := validateFCMTopic(topic); err != nil {
			return fmt.Errorf("delivery: receiver %q: %w", receiver.Name, err)
		}
	}

	payload, err := json.Marshal(androidNotification(message))
	if err != nil {
		return err
	}

	headers := make(map[string]string, len(receiver.Headers)+1)
	for key, value := range receiver.Headers {
		headers[key] = value
	}
	if topic != "" {
		// X-FCM-Topic drives the simple topic relay. X-FCB-Target lets the
		// on-call backend resolve a named device group instead of the rotation.
		// The first-class target wins over a header copied from an older config.
		headers["X-FCM-Topic"] = topic
		headers["X-FCB-Target"] = topic
	}

	if runtime.FCB.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, runtime.FCB.Timeout)
		defer cancel()
	}
	return runtime.postSignedTo(ctx, receiver.Name, endpoint, secret, payload, headers)
}

// fcbTopic resolves a receiver's topic, preferring the first-class target.
func fcbTopic(receiver Receiver, fallback string) string {
	if topic := strings.TrimSpace(receiver.Target); topic != "" {
		return topic
	}
	if topic := headerValue(receiver.Headers, "X-FCM-Topic"); topic != "" {
		return topic
	}
	return strings.TrimSpace(fallback)
}

func headerValue(headers map[string]string, name string) string {
	for key, value := range headers {
		if strings.EqualFold(strings.TrimSpace(key), name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func validateFCMTopic(topic string) error {
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return fmt.Errorf("fcb topic is empty")
	}
	if len(topic) > maxFCBTopicLength {
		return fmt.Errorf("fcb topic is longer than %d characters", maxFCBTopicLength)
	}
	if !fcmTopicPattern.MatchString(topic) {
		return fmt.Errorf("fcb topic %q contains invalid characters (allowed: letters, numbers, -_.~%%)", topic)
	}
	return nil
}
