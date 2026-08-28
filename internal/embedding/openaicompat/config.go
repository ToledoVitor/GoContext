// Package openaicompat implements the OpenAI-compatible embeddings protocol.
package openaicompat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ToledoVitor/GoContext/internal/embedding"
)

const (
	protocolVersion     = "openai-compatible-embeddings-v1"
	wireEncodingVersion = "float32-v1"
	vectorNormalization = "cosine-unit-f32-v1"

	defaultBatchSize     = 32
	defaultMaxBatchBytes = 256 * 1024
	defaultMaxInFlight   = 2
	defaultTimeout       = 30 * time.Second
	defaultMaxRetries    = 2
)

var (
	errInvalidBaseURL       = errors.New("invalid embedding base URL")
	errModelRequired        = errors.New("embedding model is required")
	errInvalidDimensions    = errors.New("embedding dimensions cannot be negative")
	errInvalidBatchSize     = errors.New("embedding batch size cannot be negative")
	errInvalidMaxBatchBytes = errors.New("embedding max batch bytes cannot be negative")
	errInvalidMaxInFlight   = errors.New("embedding max in-flight cannot be negative")
	errInvalidTimeout       = errors.New("embedding timeout cannot be negative")
	errInvalidMaxRetries    = errors.New("embedding max retries cannot be negative")
	errNonLoopbackPeer      = errors.New("embedding HTTP peer is not loopback")
)

// Config configures an OpenAI-compatible embedding client.
type Config struct {
	BaseURL       string
	Model         string
	APIKey        string
	Dimensions    int
	BatchSize     int
	MaxBatchBytes int
	MaxInFlight   int
	Timeout       time.Duration
	MaxRetries    int
}

// Client is an OpenAI-compatible embedding client.
type Client struct {
	config     Config
	endpoint   *url.URL
	profile    embedding.Profile
	httpClient *http.Client
	now        func() time.Time
	sleep      func(context.Context, time.Duration) error
}

// New validates config and creates an OpenAI-compatible embedding client.
func New(config Config) (*Client, error) {
	baseURL, err := normalizeBaseURL(config.BaseURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(config.Model) == "" {
		return nil, errModelRequired
	}
	if err := validateLimits(config); err != nil {
		return nil, err
	}

	applyDefaults(&config)
	config.BaseURL = baseURL.String()
	endpoint := *baseURL
	endpoint.Path += "/embeddings"

	profile, err := newProfile(endpoint.String(), config.Model, config.Dimensions)
	if err != nil {
		return nil, err
	}

	dialContext := (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	if baseURL.Scheme == "http" {
		dialContext = requireLoopbackPeer(dialContext)
	}
	transport := &http.Transport{
		DialContext:           dialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}

	return &Client{
		config:   config,
		endpoint: &endpoint,
		profile:  profile,
		now:      time.Now,
		sleep:    sleepContext,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   config.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

func requireLoopbackPeer(dial dialContextFunc) dialContextFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		connection, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}

		remote, ok := connection.RemoteAddr().(*net.TCPAddr)
		if !ok || remote.IP == nil || !remote.IP.IsLoopback() {
			_ = connection.Close()
			return nil, errNonLoopbackPeer
		}
		return connection, nil
	}
}

// Profile returns the compatibility profile produced by this client.
func (client *Client) Profile() embedding.Profile {
	return client.profile
}

func normalizeBaseURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.Hostname() == "" {
		return nil, errInvalidBaseURL
	}

	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && scheme != "http" {
		return nil, errInvalidBaseURL
	}
	if parsed.User != nil || parsed.ForceQuery || parsed.RawQuery != "" || parsed.Fragment != "" || strings.Contains(raw, "#") {
		return nil, errInvalidBaseURL
	}
	if parsed.RawPath != "" || (parsed.Path != "/v1" && parsed.Path != "/v1/") {
		return nil, errInvalidBaseURL
	}
	if strings.HasSuffix(parsed.Host, ":") {
		return nil, errInvalidBaseURL
	}

	hostname := parsed.Hostname()
	ip := net.ParseIP(hostname)
	if scheme == "http" && (ip == nil || !ip.IsLoopback()) {
		return nil, errInvalidBaseURL
	}
	if strings.Contains(hostname, "%") {
		return nil, errInvalidBaseURL
	}
	if ip != nil {
		hostname = ip.String()
	} else {
		hostname = strings.ToLower(hostname)
	}

	normalizedPort := ""
	if rawPort := parsed.Port(); rawPort != "" {
		port, err := strconv.ParseUint(rawPort, 10, 16)
		if err != nil || port == 0 {
			return nil, errInvalidBaseURL
		}
		if !((scheme == "https" && port == 443) || (scheme == "http" && port == 80)) {
			normalizedPort = strconv.FormatUint(port, 10)
		}
	}

	host := hostname
	if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	if normalizedPort != "" {
		host += ":" + normalizedPort
	}

	return &url.URL{
		Scheme: scheme,
		Host:   host,
		Path:   "/v1",
	}, nil
}

func validateLimits(config Config) error {
	switch {
	case config.Dimensions < 0:
		return errInvalidDimensions
	case config.BatchSize < 0:
		return errInvalidBatchSize
	case config.MaxBatchBytes < 0:
		return errInvalidMaxBatchBytes
	case config.MaxInFlight < 0:
		return errInvalidMaxInFlight
	case config.Timeout < 0:
		return errInvalidTimeout
	case config.MaxRetries < 0:
		return errInvalidMaxRetries
	default:
		return nil
	}
}

func applyDefaults(config *Config) {
	if config.BatchSize == 0 {
		config.BatchSize = defaultBatchSize
	}
	if config.MaxBatchBytes == 0 {
		config.MaxBatchBytes = defaultMaxBatchBytes
	}
	if config.MaxInFlight == 0 {
		config.MaxInFlight = defaultMaxInFlight
	}
	if config.Timeout == 0 {
		config.Timeout = defaultTimeout
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = defaultMaxRetries
	}
}

func newProfile(endpoint, model string, dimensions int) (embedding.Profile, error) {
	descriptor := struct {
		ProtocolVersion     string `json:"protocol_version"`
		Endpoint            string `json:"endpoint"`
		Model               string `json:"model"`
		Dimensions          int    `json:"dimensions"`
		WireEncoding        string `json:"wire_encoding"`
		VectorNormalization string `json:"vector_normalization"`
	}{
		ProtocolVersion:     protocolVersion,
		Endpoint:            endpoint,
		Model:               model,
		Dimensions:          dimensions,
		WireEncoding:        wireEncodingVersion,
		VectorNormalization: vectorNormalization,
	}

	canonical, err := json.Marshal(descriptor)
	if err != nil {
		return embedding.Profile{}, errors.New("cannot create embedding profile")
	}
	digest := sha256.Sum256(canonical)
	return embedding.Profile{
		Fingerprint: hex.EncodeToString(digest[:]),
		Model:       model,
	}, nil
}
