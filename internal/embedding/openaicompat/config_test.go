package openaicompat

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestConfigAcceptsSafeEndpointsAndNormalizesEmbeddingsURL(t *testing.T) {
	tests := []struct {
		name         string
		baseURL      string
		wantBaseURL  string
		wantEndpoint string
	}{
		{
			name:         "remote HTTPS",
			baseURL:      "https://API.Example.com/v1",
			wantBaseURL:  "https://api.example.com/v1",
			wantEndpoint: "https://api.example.com/v1/embeddings",
		},
		{
			name:         "remote HTTPS trailing slash",
			baseURL:      "https://api.example.com/v1/",
			wantBaseURL:  "https://api.example.com/v1",
			wantEndpoint: "https://api.example.com/v1/embeddings",
		},
		{
			name:         "HTTPS localhost",
			baseURL:      "https://localhost/v1",
			wantBaseURL:  "https://localhost/v1",
			wantEndpoint: "https://localhost/v1/embeddings",
		},
		{
			name:         "IPv4 loopback HTTP",
			baseURL:      "http://127.42.0.9:11434/v1",
			wantBaseURL:  "http://127.42.0.9:11434/v1",
			wantEndpoint: "http://127.42.0.9:11434/v1/embeddings",
		},
		{
			name:         "IPv6 loopback HTTP",
			baseURL:      "http://[0:0:0:0:0:0:0:1]:11434/v1",
			wantBaseURL:  "http://[::1]:11434/v1",
			wantEndpoint: "http://[::1]:11434/v1/embeddings",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validConfig()
			config.BaseURL = test.baseURL

			client, err := New(config)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if client.config.BaseURL != test.wantBaseURL {
				t.Fatalf("normalized BaseURL = %q, want %q", client.config.BaseURL, test.wantBaseURL)
			}
			if got := client.endpoint.String(); got != test.wantEndpoint {
				t.Fatalf("endpoint = %q, want %q", got, test.wantEndpoint)
			}
		})
	}
}

func TestConfigRejectsUnsafeOrInvalidValuesWithoutLeakingCredential(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Config)
	}{
		{name: "empty base URL", change: func(config *Config) { config.BaseURL = "" }},
		{name: "unsupported scheme", change: func(config *Config) { config.BaseURL = "ftp://api.example.com/v1" }},
		{name: "missing host", change: func(config *Config) { config.BaseURL = "https:///v1" }},
		{name: "HTTP localhost", change: func(config *Config) { config.BaseURL = "http://localhost:11434/v1" }},
		{name: "HTTP remote DNS", change: func(config *Config) { config.BaseURL = "http://api.example.com/v1" }},
		{name: "HTTP remote IPv4", change: func(config *Config) { config.BaseURL = "http://192.0.2.1/v1" }},
		{name: "HTTP remote IPv6", change: func(config *Config) { config.BaseURL = "http://[2001:db8::1]/v1" }},
		{name: "userinfo", change: func(config *Config) { config.BaseURL = "https://credential-canary@api.example.com/v1" }},
		{name: "query", change: func(config *Config) { config.BaseURL = "https://api.example.com/v1?key=credential-canary" }},
		{name: "fragment", change: func(config *Config) { config.BaseURL = "https://api.example.com/v1#credential-canary" }},
		{name: "empty path", change: func(config *Config) { config.BaseURL = "https://api.example.com" }},
		{name: "path outside v1", change: func(config *Config) { config.BaseURL = "https://api.example.com/v2" }},
		{name: "embeddings path supplied", change: func(config *Config) { config.BaseURL = "https://api.example.com/v1/embeddings" }},
		{name: "dot segment path", change: func(config *Config) { config.BaseURL = "https://api.example.com/v1/../v1" }},
		{name: "encoded path separator", change: func(config *Config) { config.BaseURL = "https://api.example.com/v1%2fembeddings" }},
		{name: "empty model", change: func(config *Config) { config.Model = "" }},
		{name: "whitespace model", change: func(config *Config) { config.Model = " \t" }},
		{name: "negative dimensions", change: func(config *Config) { config.Dimensions = -1 }},
		{name: "negative batch size", change: func(config *Config) { config.BatchSize = -1 }},
		{name: "negative max batch bytes", change: func(config *Config) { config.MaxBatchBytes = -1 }},
		{name: "negative max in flight", change: func(config *Config) { config.MaxInFlight = -1 }},
		{name: "negative timeout", change: func(config *Config) { config.Timeout = -time.Second }},
		{name: "negative max retries", change: func(config *Config) { config.MaxRetries = -1 }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validConfig()
			config.APIKey = "credential-canary"
			test.change(&config)

			client, err := New(config)
			if err == nil {
				t.Fatalf("New() client = %#v, want error", client)
			}
			if strings.Contains(err.Error(), "credential-canary") {
				t.Fatalf("New() error leaked credential: %v", err)
			}
		})
	}
}

func TestConfigAppliesExactDefaults(t *testing.T) {
	client, err := New(validConfig())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if client.config.BatchSize != 32 {
		t.Errorf("BatchSize = %d, want 32", client.config.BatchSize)
	}
	if client.config.MaxBatchBytes != 256*1024 {
		t.Errorf("MaxBatchBytes = %d, want %d", client.config.MaxBatchBytes, 256*1024)
	}
	if client.config.MaxInFlight != 2 {
		t.Errorf("MaxInFlight = %d, want 2", client.config.MaxInFlight)
	}
	if client.config.Timeout != 30*time.Second {
		t.Errorf("Timeout = %s, want 30s", client.config.Timeout)
	}
	if client.config.MaxRetries != 2 {
		t.Errorf("MaxRetries = %d, want 2", client.config.MaxRetries)
	}
}

func TestConfigCreatesPrivateTransportWithoutProxyOrRedirects(t *testing.T) {
	client, err := New(validConfig())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	transport, ok := client.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type = %T, want *http.Transport", client.httpClient.Transport)
	}
	if transport == http.DefaultTransport {
		t.Fatal("Transport reuses http.DefaultTransport")
	}
	if transport.Proxy != nil {
		t.Fatal("Transport has proxy policy enabled")
	}
	if client.httpClient.CheckRedirect == nil {
		t.Fatal("CheckRedirect is nil")
	}
	if err := client.httpClient.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("CheckRedirect() error = %v, want %v", err, http.ErrUseLastResponse)
	}
}

func TestProfileFingerprintExcludesCredentialAndUsesCanonicalConfiguration(t *testing.T) {
	first := validConfig()
	first.APIKey = "secret-one"
	second := first
	second.APIKey = "secret-two"
	second.BaseURL += "/"

	firstClient, err := New(first)
	if err != nil {
		t.Fatalf("New(first) error = %v", err)
	}
	secondClient, err := New(second)
	if err != nil {
		t.Fatalf("New(second) error = %v", err)
	}

	firstProfile := firstClient.Profile()
	if firstProfile != secondClient.Profile() {
		t.Fatal("credential or trailing slash changed embedding profile")
	}
	if firstProfile.Model != first.Model {
		t.Fatalf("Profile().Model = %q, want %q", firstProfile.Model, first.Model)
	}
	if strings.Contains(fmt.Sprintf("%+v", firstProfile), "secret") {
		t.Fatal("profile contains credential")
	}
	const wantFingerprint = "68f128dbe6b7f1d2a870dffa21aad2059a3af4f6cddeb44fa03cd3d4233048e5"
	if firstProfile.Fingerprint != wantFingerprint {
		t.Fatalf("Profile().Fingerprint = %q, want %q", firstProfile.Fingerprint, wantFingerprint)
	}
}

func TestProfileFingerprintChangesWithEmbeddingCompatibility(t *testing.T) {
	base := validConfig()
	baseClient, err := New(base)
	if err != nil {
		t.Fatalf("New(base) error = %v", err)
	}

	tests := []struct {
		name   string
		change func(*Config)
	}{
		{name: "endpoint", change: func(config *Config) { config.BaseURL = "https://other.example.com/v1" }},
		{name: "model", change: func(config *Config) { config.Model = "other-model" }},
		{name: "requested dimensions", change: func(config *Config) { config.Dimensions = 768 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := base
			test.change(&config)
			client, err := New(config)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if client.Profile().Fingerprint == baseClient.Profile().Fingerprint {
				t.Fatal("compatibility change did not change fingerprint")
			}
		})
	}
}

func validConfig() Config {
	return Config{
		BaseURL:    "https://api.example.com/v1",
		Model:      "text-embedding-3-small",
		Dimensions: 1536,
	}
}
