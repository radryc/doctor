package delivery

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultTimeout   = 10 * time.Second
	maxResponseBytes = 1 << 20
)

func (rt Runtime) client() *http.Client {
	if rt.Client != nil {
		return rt.Client
	}
	return &http.Client{Timeout: defaultTimeout}
}

func (rt Runtime) checkURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("delivery: receiver has no url")
	}
	if rt.URLPolicy == nil {
		return nil
	}
	return rt.URLPolicy.Validate(raw)
}

func (rt Runtime) do(req *http.Request) error {
	resp, err := rt.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (rt Runtime) postJSON(ctx context.Context, receiver Receiver, payload []byte) error {
	endpoint := receiver.URLValue()
	if err := rt.checkURL(endpoint); err != nil {
		return fmt.Errorf("delivery: receiver %q: %w", receiver.Name, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "doctor-sentinel")
	for key, value := range receiver.Headers {
		req.Header.Set(key, value)
	}
	return rt.do(req)
}

func (rt Runtime) postWithHMAC(ctx context.Context, receiver Receiver, payload []byte) error {
	return rt.postSigned(ctx, receiver, payload, nil)
}

func (rt Runtime) postSigned(ctx context.Context, receiver Receiver, payload []byte, headers map[string]string) error {
	merged := make(map[string]string, len(receiver.Headers)+len(headers))
	for key, value := range headers {
		merged[key] = value
	}
	// Static receiver headers win over per-message headers, matching the
	// historical transport behaviour.
	for key, value := range receiver.Headers {
		merged[key] = value
	}
	return rt.postSignedTo(ctx, receiver.Name, receiver.URLValue(), receiver.SecretValue(), payload, merged)
}

// postSignedTo is the transport shared by senders that resolve the endpoint
// and secret from global runtime defaults instead of the receiver document.
func (rt Runtime) postSignedTo(ctx context.Context, name, endpoint, secret string, payload []byte, headers map[string]string) error {
	if err := rt.checkURL(endpoint); err != nil {
		return fmt.Errorf("delivery: receiver %q: %w", name, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "doctor-sentinel")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(payload)
		signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		req.Header.Set("X-Sentinel-Signature", signature)
		req.Header.Set("X-Doctor-Signature", signature)
	}
	return rt.do(req)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
