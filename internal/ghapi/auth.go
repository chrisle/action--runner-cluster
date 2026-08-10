package ghapi

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
	"net/http"
	"os"
	"sync"
	"time"
)

// Authenticator supplies the bearer token for API requests.
type Authenticator interface {
	// Token returns a valid token, refreshing it if necessary.
	Token(ctx context.Context) (string, error)
	// Describe names the auth method for logs and `arc doctor`.
	Describe() string
}

// StaticToken authenticates with a personal access token.
type StaticToken struct{ Value string }

func (s *StaticToken) Token(context.Context) (string, error) { return s.Value, nil }
func (s *StaticToken) Describe() string                      { return "personal access token" }

// AppAuth authenticates as a GitHub App installation. Installation tokens last
// an hour, so they are cached and refreshed shortly before expiry.
//
// The RS256 JWT is assembled with crypto/rsa rather than a JWT library: it is a
// fixed header, two claims and one signature, and it keeps this a two-dependency
// binary.
type AppAuth struct {
	AppID          int64
	InstallationID int64
	Key            *rsa.PrivateKey
	APIURL         string
	HTTP           *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewAppAuth builds an AppAuth from PEM bytes or a PEM file path.
func NewAppAuth(appID, installationID int64, pemData, pemPath, apiURL string, hc *http.Client) (*AppAuth, error) {
	raw := []byte(pemData)
	if pemPath != "" {
		b, err := os.ReadFile(pemPath)
		if err != nil {
			return nil, fmt.Errorf("read app private key: %w", err)
		}
		raw = b
	}
	key, err := parseRSAPrivateKey(raw)
	if err != nil {
		return nil, err
	}
	return &AppAuth{
		AppID:          appID,
		InstallationID: installationID,
		Key:            key,
		APIURL:         apiURL,
		HTTP:           hc,
	}, nil
}

func (a *AppAuth) Describe() string {
	return fmt.Sprintf("GitHub App %d installation %d", a.AppID, a.InstallationID)
}

func (a *AppAuth) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Refresh a minute early so an in-flight request can't expire mid-call.
	if a.token != "" && time.Now().Before(a.expires.Add(-time.Minute)) {
		return a.token, nil
	}

	jwt, err := a.appJWT()
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", a.APIURL, a.InstallationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(nil))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)

	resp, err := a.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("mint installation token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("mint installation token: %s", describeError(resp))
	}

	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode installation token: %w", err)
	}
	a.token, a.expires = out.Token, out.ExpiresAt
	return a.token, nil
}

// appJWT builds the short-lived RS256 JWT that authenticates as the App itself.
func (a *AppAuth) appJWT() (string, error) {
	now := time.Now()
	// iat is backdated 60s to tolerate clock skew against GitHub; GitHub
	// rejects a JWT whose iat is in the future.
	claims := map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(), // GitHub's ceiling is 10 minutes
		"iss": a.AppID,
	}
	header := map[string]string{"alg": "RS256", "typ": "JWT"}

	seg := func(v any) (string, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return base64.RawURLEncoding.EncodeToString(b), nil
	}
	h, err := seg(header)
	if err != nil {
		return "", err
	}
	c, err := seg(claims)
	if err != nil {
		return "", err
	}

	signingInput := h + "." + c
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign app jwt: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// parseRSAPrivateKey accepts both PKCS#1 ("RSA PRIVATE KEY", what GitHub hands
// out) and PKCS#8 ("PRIVATE KEY", what openssl often produces).
func parseRSAPrivateKey(raw []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("app private key is not valid PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse app private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("app private key is %T, want RSA", parsed)
	}
	return key, nil
}
