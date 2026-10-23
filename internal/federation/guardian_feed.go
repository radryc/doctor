package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GuardianFeed reads the machine-facing Guardian APIs used by the catalog
// controller. It is separate from the lazy token resolver: the controller needs
// complete snapshots, while query replicas resolve individual tokens on demand.
type GuardianFeed struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

// NewGuardianFeed builds a Guardian feed client.
func NewGuardianFeed(baseURL, token string, client *http.Client) *GuardianFeed {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &GuardianFeed{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Token:   strings.TrimSpace(token),
		Client:  client,
	}
}

type guardianPusherPage struct {
	Pushers    []guardianPusher `json:"pushers"`
	Total      int              `json:"total"`
	NextCursor string           `json:"nextCursor"`
	HasMore    bool             `json:"hasMore"`
}

type guardianPusher struct {
	Name           string    `json:"name"`
	Provider       string    `json:"provider"`
	Account        string    `json:"account"`
	Region         string    `json:"region"`
	Version        string    `json:"version"`
	Priority       int       `json:"priority"`
	Status         string    `json:"status"`
	LastHeartbeat  time.Time `json:"lastHeartbeat"`
	ObservedAPIURL string    `json:"observedApiUrl"`
	Token          string    `json:"token"`
	TokenEnv       string    `json:"tokenEnv"`
}

type guardianPartitionPage struct {
	Partitions []guardianPartition `json:"partitions"`
	Total      int                 `json:"total"`
	NextCursor string              `json:"nextCursor"`
	HasMore    bool                `json:"hasMore"`
}

type guardianPartition struct {
	Name         string            `json:"name"`
	Account      string            `json:"account"`
	Region       string            `json:"region"`
	Provider     string            `json:"provider"`
	Labels       map[string]string `json:"labels"`
	TargetPusher string            `json:"targetPusher"`
	Warnings     []string          `json:"warnings"`
}

// Pushers loads every registered pusher page and returns its token keyed by
// pusher name. Tokens stay out of the catalog snapshot.
func (g *GuardianFeed) Pushers(ctx context.Context) ([]PusherInfo, map[string]string, error) {
	if g == nil || g.BaseURL == "" {
		return nil, nil, fmt.Errorf("guardian feed URL is required")
	}
	pushers := make([]PusherInfo, 0)
	tokens := make(map[string]string)
	cursor := ""
	for pageNumber := 0; pageNumber < 10000; pageNumber++ {
		requestURL := g.BaseURL + "/api/m2m/pushers?limit=2000"
		if cursor != "" {
			requestURL += "&cursor=" + url.QueryEscape(cursor)
		}
		var page guardianPusherPage
		if err := g.getJSON(ctx, requestURL, &page); err != nil {
			return nil, nil, fmt.Errorf("list guardian pushers: %w", err)
		}
		for _, item := range page.Pushers {
			name := strings.TrimSpace(item.Name)
			if name == "" {
				continue
			}
			endpoint := strings.TrimRight(strings.TrimSpace(item.ObservedAPIURL), "/")
			health := Health{State: strings.TrimSpace(item.Status), LastHeartbeat: item.LastHeartbeat}
			pushers = append(pushers, PusherInfo{
				Name:     name,
				Account:  strings.TrimSpace(item.Account),
				Region:   strings.TrimSpace(item.Region),
				Endpoint: endpoint,
				TokenRef: "guardian-pusher://" + name,
				TokenEnv: strings.TrimSpace(item.TokenEnv),
				Version:  strings.TrimSpace(item.Version),
				Priority: item.Priority,
				Health:   health,
			})
			if token := strings.TrimSpace(item.Token); token != "" {
				tokens[name] = token
			} else if env := strings.TrimSpace(item.TokenEnv); env != "" {
				// The controller normally has no per-pusher env. Leave it to
				// the query-side EnvTokenResolver when present.
				_ = env
			}
		}
		if !page.HasMore || strings.TrimSpace(page.NextCursor) == "" {
			break
		}
		cursor = page.NextCursor
	}
	return pushers, tokens, nil
}

// Partitions loads the normalized, paginated partition feed.
func (g *GuardianFeed) Partitions(ctx context.Context) ([]PartitionInfo, error) {
	if g == nil || g.BaseURL == "" {
		return nil, fmt.Errorf("guardian feed URL is required")
	}
	partitions := make([]PartitionInfo, 0)
	cursor := ""
	for pageNumber := 0; pageNumber < 10000; pageNumber++ {
		requestURL := g.BaseURL + "/api/m2m/federation/partitions?limit=2000"
		if cursor != "" {
			requestURL += "&cursor=" + url.QueryEscape(cursor)
		}
		var page guardianPartitionPage
		if err := g.getJSON(ctx, requestURL, &page); err != nil {
			return nil, fmt.Errorf("list guardian federation partitions: %w", err)
		}
		for _, item := range page.Partitions {
			partitions = append(partitions, PartitionInfo{
				Name:     strings.TrimSpace(item.Name),
				Provider: strings.ToLower(strings.TrimSpace(item.Provider)),
				Account:  strings.TrimSpace(item.Account),
				Region:   strings.TrimSpace(item.Region),
				Labels:   cloneStringMap(item.Labels),
			})
		}
		if !page.HasMore || strings.TrimSpace(page.NextCursor) == "" {
			break
		}
		cursor = page.NextCursor
	}
	return partitions, nil
}

func (g *GuardianFeed) getJSON(ctx context.Context, requestURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if g.Token != "" {
		req.Header.Set("X-Guardian-Internal-Token", g.Token)
	}
	resp, err := g.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("guardian HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode guardian response: %w", err)
	}
	return nil
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
