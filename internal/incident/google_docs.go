package incident

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// GoogleDocsEngine publishes a postmortem as a Google Doc using a service
// account. It intentionally uses the public REST APIs so no Google SDK is
// pulled into the Sentinel binary.
type GoogleDocsEngine struct {
	Client *http.Client
}

// Name implements Engine.
func (GoogleDocsEngine) Name() string { return EngineGoogleDocs }

// Metadata implements Engine.
func (GoogleDocsEngine) Metadata() EngineMetadata {
	return EngineMetadata{
		Name:        EngineGoogleDocs,
		Label:       "Google Docs",
		Description: "Create a Google Doc from the postmortem with a service account.",
		Fields: []OptionDef{
			{Key: "credentialsEnv", Label: "Service account JSON env var", Type: "string", Secret: true, Placeholder: "GOOGLE_APPLICATION_CREDENTIALS"},
			{Key: "folderId", Label: "Drive folder ID", Type: "string"},
			{Key: "shareWith", Label: "Share with (comma-separated emails)", Type: "string"},
			{Key: "subject", Label: "Impersonate Workspace user", Type: "string"},
		},
	}
}

type googleServiceAccount struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

// Publish implements Engine.
func (e GoogleDocsEngine) Publish(ctx context.Context, doc PostmortemDocument) (PostmortemRef, error) {
	options := doc.Postmortem.EngineOptions
	credentialsEnv := strings.TrimSpace(options["credentialsEnv"])
	if credentialsEnv == "" {
		credentialsEnv = "GOOGLE_APPLICATION_CREDENTIALS"
	}
	credentialsPath := strings.TrimSpace(os.Getenv(credentialsEnv))
	if credentialsPath == "" {
		return PostmortemRef{}, fmt.Errorf("incident: google-docs engine needs %s", credentialsEnv)
	}
	raw, err := os.ReadFile(credentialsPath)
	if err != nil {
		return PostmortemRef{}, fmt.Errorf("incident: read google credentials: %w", err)
	}
	var account googleServiceAccount
	if err := json.Unmarshal(raw, &account); err != nil {
		return PostmortemRef{}, fmt.Errorf("incident: decode google credentials: %w", err)
	}
	if account.ClientEmail == "" || account.PrivateKey == "" {
		return PostmortemRef{}, fmt.Errorf("incident: google service account is missing client_email or private_key")
	}
	client := e.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	token, err := googleAccessToken(ctx, client, account, strings.TrimSpace(options["subject"]))
	if err != nil {
		return PostmortemRef{}, err
	}
	title := strings.TrimSpace(options["documentTitle"])
	if title == "" {
		title = "Postmortem — " + doc.Incident.TitleText()
	}
	fileID, err := googleCreateDocument(ctx, client, token, title, strings.TrimSpace(options["folderId"]))
	if err != nil {
		return PostmortemRef{}, err
	}
	if err := googleInsertText(ctx, client, token, fileID, doc.Markdown); err != nil {
		return PostmortemRef{}, err
	}
	if share := strings.TrimSpace(options["shareWith"]); share != "" {
		for _, email := range strings.Split(share, ",") {
			email = strings.TrimSpace(email)
			if email == "" {
				continue
			}
			if err := googleShare(ctx, client, token, fileID, email); err != nil {
				return PostmortemRef{}, err
			}
		}
	}
	return PostmortemRef{
		URL:        "https://docs.google.com/document/d/" + fileID + "/edit",
		ExternalID: fileID,
	}, nil
}

func googleAccessToken(ctx context.Context, client *http.Client, account googleServiceAccount, subject string) (string, error) {
	tokenURI := strings.TrimSpace(account.TokenURI)
	if tokenURI == "" {
		tokenURI = "https://oauth2.googleapis.com/token"
	}
	now := time.Now().Unix()
	claims := map[string]any{
		"iss":   account.ClientEmail,
		"scope": "https://www.googleapis.com/auth/drive.file https://www.googleapis.com/auth/documents",
		"aud":   tokenURI,
		"iat":   now,
		"exp":   now + 3600,
	}
	if subject != "" {
		claims["sub"] = subject
	}
	assertion, err := signJWT(account.PrivateKey, claims)
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("incident: google token HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	if parsed.AccessToken == "" {
		return "", fmt.Errorf("incident: google token response had no access_token")
	}
	return parsed.AccessToken, nil
}

func signJWT(privateKeyPEM string, claims map[string]any) (string, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return "", fmt.Errorf("incident: google private key is not PEM")
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return "", err
		}
		key = parsed
	default:
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return "", err
		}
		rsaKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return "", fmt.Errorf("incident: google private key is not RSA")
		}
		key = rsaKey
	}
	header := map[string]any{"alg": "RS256", "typ": "JWT"}
	headerJSON, _ := json.Marshal(header)
	claimsJSON, _ := json.Marshal(claims)
	encoding := base64.RawURLEncoding
	signingInput := encoding.EncodeToString(headerJSON) + "." + encoding.EncodeToString(claimsJSON)
	sum := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + encoding.EncodeToString(signature), nil
}

func googleCreateDocument(ctx context.Context, client *http.Client, token, title, folderID string) (string, error) {
	payload := map[string]any{
		"name":     title,
		"mimeType": "application/vnd.google-apps.document",
	}
	if folderID != "" {
		payload["parents"] = []string{folderID}
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://www.googleapis.com/drive/v3/files", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	response, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("incident: google create doc HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(response)))
	}
	var parsed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response, &parsed); err != nil {
		return "", err
	}
	if parsed.ID == "" {
		return "", fmt.Errorf("incident: google create doc returned no id")
	}
	return parsed.ID, nil
}

func googleInsertText(ctx context.Context, client *http.Client, token, fileID, text string) error {
	payload := map[string]any{
		"requests": []map[string]any{{
			"insertText": map[string]any{
				"location": map[string]any{"index": 1},
				"text":     text,
			},
		}},
	}
	body, _ := json.Marshal(payload)
	endpoint := "https://docs.googleapis.com/v1/documents/" + url.PathEscape(fileID) + ":batchUpdate"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	response, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("incident: google insert text HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(response)))
	}
	return nil
}

func googleShare(ctx context.Context, client *http.Client, token, fileID, email string) error {
	payload := map[string]any{
		"type":         "user",
		"role":         "writer",
		"emailAddress": email,
	}
	body, _ := json.Marshal(payload)
	endpoint := "https://www.googleapis.com/drive/v3/files/" + url.PathEscape(fileID) + "/permissions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	response, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("incident: google share HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(response)))
	}
	return nil
}
