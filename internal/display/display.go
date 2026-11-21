// Package display loads the compiled display-profile bundle and resolves the
// caller's profile. It is presentation-only; values and query execution stay
// canonical.
package display

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	shared "github.com/radryc/monofs/pkg/display"
)

type Service struct {
	url      string
	file     string
	token    string
	interval time.Duration
	client   *http.Client

	mu        sync.Mutex
	bundle    *shared.Bundle
	version   string
	fetchedAt time.Time
}

// NewFromEnv builds a loader from DOCTOR_DISPLAY_* variables. It returns nil
// when neither a URL nor a file is configured.
func NewFromEnv() *Service {
	url := strings.TrimSpace(os.Getenv("DOCTOR_DISPLAY_BUNDLE_URL"))
	file := strings.TrimSpace(os.Getenv("DOCTOR_DISPLAY_BUNDLE_FILE"))
	if url == "" && file == "" {
		return nil
	}
	token := strings.TrimSpace(os.Getenv("DOCTOR_DISPLAY_BUNDLE_TOKEN"))
	if token == "" {
		token = strings.TrimSpace(os.Getenv("DOCTOR_ACCESS_POLICY_TOKEN"))
	}
	interval := 30 * time.Second
	if raw := strings.TrimSpace(os.Getenv("DOCTOR_DISPLAY_REFRESH_INTERVAL")); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			interval = parsed
		}
	}
	return &Service{
		url:      url,
		file:     file,
		token:    token,
		interval: interval,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// Resolve returns the compiled profile for a request. An empty requested id
// uses the caller's default.
func (s *Service) Resolve(ctx context.Context, subject, requested string) (*shared.CompiledProfile, string, error) {
	if s == nil {
		return nil, "", nil
	}
	bundle, version, err := s.bundleFor(ctx)
	if err != nil {
		return nil, version, err
	}
	if strings.TrimSpace(requested) == "" {
		requested = strings.TrimSpace(bundle.Defaults[subject])
	}
	profile, ok := bundle.Profile(subject, requested)
	if !ok {
		return nil, version, fmt.Errorf("display profile %q is not available", requested)
	}
	return profile, version, nil
}

// Profiles returns the compiled profiles for UI selection.
func (s *Service) Profiles(ctx context.Context) ([]shared.CompiledProfile, string, error) {
	if s == nil {
		return nil, "", nil
	}
	bundle, version, err := s.bundleFor(ctx)
	if err != nil {
		return nil, version, err
	}
	return bundle.Profiles, version, nil
}

// Version returns the current bundle version, refreshing if needed.
func (s *Service) Version(ctx context.Context) string {
	if s == nil {
		return ""
	}
	_, version, _ := s.bundleFor(ctx)
	return version
}

func (s *Service) bundleFor(ctx context.Context) (*shared.Bundle, string, error) {
	s.mu.Lock()
	if s.bundle != nil && time.Since(s.fetchedAt) < s.interval {
		bundle, version := s.bundle, s.version
		s.mu.Unlock()
		return bundle, version, nil
	}
	s.mu.Unlock()

	bundle, version, err := s.fetch(ctx)
	if err != nil {
		s.mu.Lock()
		if s.bundle != nil {
			bundle, version = s.bundle, s.version
			s.mu.Unlock()
			return bundle, version, nil
		}
		s.mu.Unlock()
		return nil, "", err
	}
	s.mu.Lock()
	s.bundle = bundle
	s.version = version
	s.fetchedAt = time.Now()
	s.mu.Unlock()
	return bundle, version, nil
}

func (s *Service) fetch(ctx context.Context) (*shared.Bundle, string, error) {
	var raw []byte
	var err error
	if s.url != "" {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
		if reqErr != nil {
			return nil, "", reqErr
		}
		if s.token != "" {
			req.Header.Set("Authorization", "Bearer "+s.token)
		}
		resp, doErr := s.client.Do(req)
		if doErr != nil {
			return nil, "", doErr
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, "", fmt.Errorf("display bundle: status %d", resp.StatusCode)
		}
		raw, err = io.ReadAll(resp.Body)
		if err != nil {
			return nil, "", err
		}
		if version := resp.Header.Get(shared.VersionHeader); version != "" {
			var bundle shared.Bundle
			if err := json.Unmarshal(raw, &bundle); err != nil {
				return nil, "", err
			}
			return &bundle, version, nil
		}
	} else {
		raw, err = os.ReadFile(s.file)
		if err != nil {
			return nil, "", err
		}
	}
	var bundle shared.Bundle
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return nil, "", err
	}
	version := bundle.Version
	if version == "" {
		version = time.Now().UTC().Format(time.RFC3339)
	}
	return &bundle, version, nil
}
