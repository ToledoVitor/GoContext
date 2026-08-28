package main

import (
	"errors"
	"flag"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ToledoVitor/GoContext/internal/embedding"
	"github.com/ToledoVitor/GoContext/internal/embedding/openaicompat"
)

const (
	semanticModeEnv         = "GOCONTEXT_SEMANTIC_MODE"
	embeddingBaseURLEnv     = "GOCONTEXT_EMBEDDING_BASE_URL"
	embeddingModelEnv       = "GOCONTEXT_EMBEDDING_MODEL"
	embeddingDimensionsEnv  = "GOCONTEXT_EMBEDDING_DIMENSIONS"
	embeddingAPIKeyEnv      = "GOCONTEXT_EMBEDDING_API_KEY"
	embeddingBatchSizeEnv   = "GOCONTEXT_EMBEDDING_BATCH_SIZE"
	embeddingMaxInFlightEnv = "GOCONTEXT_EMBEDDING_MAX_IN_FLIGHT"
	embeddingTimeoutEnv     = "GOCONTEXT_EMBEDDING_TIMEOUT"
)

const (
	defaultEmbeddingBatchSize   = 32
	defaultEmbeddingMaxInFlight = 2
	defaultEmbeddingTimeout     = 30 * time.Second
)

type semanticMode string

const (
	semanticModeOff       semanticMode = "off"
	semanticModePreferred semanticMode = "preferred"
	semanticModeRequired  semanticMode = "required"
)

var (
	errInvalidSemanticMode         = errors.New("semantic configuration: invalid semantic mode")
	errEmbeddingURLRequired        = errors.New("semantic configuration: embedding base URL is required")
	errEmbeddingModelRequired      = errors.New("semantic configuration: embedding model is required")
	errInvalidEmbeddingURL         = errors.New("semantic configuration: invalid embedding base URL")
	errInvalidEmbeddingDimensions  = errors.New("semantic configuration: invalid embedding dimensions")
	errInvalidEmbeddingBatchSize   = errors.New("semantic configuration: invalid embedding batch size")
	errInvalidEmbeddingMaxInFlight = errors.New("semantic configuration: invalid embedding max in-flight")
	errInvalidEmbeddingTimeout     = errors.New("semantic configuration: invalid embedding timeout")
	errInvalidIndexBackend         = errors.New("semantic configuration: invalid index backend")
	errIndexSemanticBackend        = errors.New("semantic configuration: semantic mode requires --index-backend sqlite")
	errSearchSemanticBackend       = errors.New(
		"semantic configuration: semantic mode requires --index-backend sqlite or auto",
	)
)

type commandRole string

const (
	commandRoleIndex  commandRole = "index"
	commandRoleSearch commandRole = "search"
)

type indexBackend string

const (
	indexBackendSnapshot indexBackend = "snapshot"
	indexBackendSQLite   indexBackend = "sqlite"
	indexBackendAuto     indexBackend = "auto"
)

type dataEgressClass string

const (
	dataEgressNone     dataEgressClass = "none"
	dataEgressLocal    dataEgressClass = "local"
	dataEgressExternal dataEgressClass = "external"
)

type trackedStringValue struct {
	value string
	set   bool
}

func (value *trackedStringValue) Set(raw string) error {
	value.value = raw
	value.set = true
	return nil
}

func (value *trackedStringValue) String() string {
	if value == nil || !value.set {
		return ""
	}
	return "<set>"
}

func (value *trackedStringValue) GoString() string {
	return value.String()
}

type embeddingOptions struct {
	mode       trackedStringValue
	baseURL    trackedStringValue
	model      trackedStringValue
	dimensions trackedStringValue
}

func (embeddingOptions) String() string {
	return "embedding options{values:redacted}"
}

func (embeddingOptions) GoString() string {
	return "embeddingOptions{values:redacted}"
}

func addEmbeddingFlags(flags *flag.FlagSet, options *embeddingOptions, backend *indexBackend) {
	flags.Var(&options.mode, "semantic", "modo semântico: off, preferred ou required")
	flags.Var(&options.baseURL, "embedding-base-url", "URL base OpenAI-compatible")
	flags.Var(&options.model, "embedding-model", "modelo de embeddings")
	flags.Var(&options.dimensions, "embedding-dimensions", "dimensões do embedding")
	flags.Func("index-backend", "backend do índice; valores dependem do comando (default snapshot)", func(raw string) error {
		*backend = indexBackend(raw)
		return nil
	})
}

type embeddingClientFactory func(openaicompat.Config) (embedding.Embedder, error)

type resolvedEmbeddingConfig struct {
	mode    semanticMode
	backend indexBackend
	client  embedding.Embedder
	egress  dataEgressClass
}

func (config resolvedEmbeddingConfig) String() string {
	return "embedding config{values:redacted}"
}

func (config resolvedEmbeddingConfig) GoString() string {
	return config.String()
}

func resolveEmbeddingConfig(
	options embeddingOptions,
	backend indexBackend,
	role commandRole,
	lookup func(string) (string, bool),
	newClient embeddingClientFactory,
) (resolvedEmbeddingConfig, error) {
	mode := semanticMode(resolveOption(options.mode, semanticModeEnv, string(semanticModeOff), lookup))
	resolved := resolvedEmbeddingConfig{mode: mode, backend: backend, egress: dataEgressNone}
	switch mode {
	case semanticModeOff, semanticModePreferred, semanticModeRequired:
	default:
		return resolvedEmbeddingConfig{}, errInvalidSemanticMode
	}
	if !backendAllowed(role, backend) {
		return resolvedEmbeddingConfig{}, errInvalidIndexBackend
	}
	if mode == semanticModeOff {
		return resolved, nil
	}

	baseURL := resolveOption(options.baseURL, embeddingBaseURLEnv, "", lookup)
	if strings.TrimSpace(baseURL) == "" {
		return resolvedEmbeddingConfig{}, errEmbeddingURLRequired
	}
	model := resolveOption(options.model, embeddingModelEnv, "", lookup)
	if strings.TrimSpace(model) == "" {
		return resolvedEmbeddingConfig{}, errEmbeddingModelRequired
	}
	dimensions := 0
	if rawDimensions, configured := resolveOptionalOption(options.dimensions, embeddingDimensionsEnv, lookup); configured {
		parsedDimensions, err := parseStrictPositiveInt(rawDimensions)
		if err != nil {
			return resolvedEmbeddingConfig{}, errInvalidEmbeddingDimensions
		}
		dimensions = parsedDimensions
	}
	batchSize := defaultEmbeddingBatchSize
	if rawBatchSize, configured := lookup(embeddingBatchSizeEnv); configured {
		parsedBatchSize, err := parseStrictBoundedInt(rawBatchSize, 128)
		if err != nil {
			return resolvedEmbeddingConfig{}, errInvalidEmbeddingBatchSize
		}
		batchSize = parsedBatchSize
	}
	maxInFlight := defaultEmbeddingMaxInFlight
	if rawMaxInFlight, configured := lookup(embeddingMaxInFlightEnv); configured {
		parsedMaxInFlight, err := parseStrictBoundedInt(rawMaxInFlight, 8)
		if err != nil {
			return resolvedEmbeddingConfig{}, errInvalidEmbeddingMaxInFlight
		}
		maxInFlight = parsedMaxInFlight
	}
	timeout := defaultEmbeddingTimeout
	if rawTimeout, configured := lookup(embeddingTimeoutEnv); configured {
		parsedTimeout, err := parseStrictPositiveDuration(rawTimeout)
		if err != nil {
			return resolvedEmbeddingConfig{}, errInvalidEmbeddingTimeout
		}
		timeout = parsedTimeout
	}

	adapterConfig := openaicompat.Config{
		BaseURL:     baseURL,
		Model:       model,
		Dimensions:  dimensions,
		BatchSize:   batchSize,
		MaxInFlight: maxInFlight,
		Timeout:     timeout,
	}
	validationClient, err := newClient(adapterConfig)
	if err != nil {
		return resolvedEmbeddingConfig{}, errInvalidEmbeddingURL
	}
	resolved.egress = classifyDataEgress(baseURL)
	if backend == indexBackendSnapshot {
		if role == commandRoleSearch {
			return resolvedEmbeddingConfig{}, errSearchSemanticBackend
		}
		return resolvedEmbeddingConfig{}, errIndexSemanticBackend
	}
	apiKey, _ := lookup(embeddingAPIKeyEnv)
	if apiKey == "" {
		resolved.client = validationClient
		return resolved, nil
	}

	adapterConfig.APIKey = apiKey
	configuredClient, err := newClient(adapterConfig)
	if err != nil {
		return resolvedEmbeddingConfig{}, errInvalidEmbeddingURL
	}
	resolved.client = configuredClient
	return resolved, nil
}

func resolveCLIEmbeddingConfig(options embeddingOptions, backend indexBackend, role commandRole) (resolvedEmbeddingConfig, error) {
	return resolveEmbeddingConfig(
		options,
		backend,
		role,
		os.LookupEnv,
		func(config openaicompat.Config) (embedding.Embedder, error) {
			return openaicompat.New(config)
		},
	)
}

func backendAllowed(role commandRole, backend indexBackend) bool {
	switch role {
	case commandRoleIndex:
		return backend == indexBackendSnapshot || backend == indexBackendSQLite
	case commandRoleSearch:
		return backend == indexBackendSnapshot || backend == indexBackendSQLite || backend == indexBackendAuto
	default:
		return false
	}
}

func classifyDataEgress(baseURL string) dataEgressClass {
	parsed, err := url.Parse(baseURL)
	if err == nil {
		if address := net.ParseIP(parsed.Hostname()); address != nil && address.IsLoopback() {
			return dataEgressLocal
		}
	}
	return dataEgressExternal
}

func resolveOption(option trackedStringValue, environmentName, fallback string, lookup func(string) (string, bool)) string {
	if option.set {
		return option.value
	}
	if value, ok := lookup(environmentName); ok {
		return value
	}
	return fallback
}

func resolveOptionalOption(option trackedStringValue, environmentName string, lookup func(string) (string, bool)) (string, bool) {
	if option.set {
		return option.value, true
	}
	return lookup(environmentName)
}

func parseStrictPositiveInt(raw string) (int, error) {
	if raw == "" {
		return 0, errors.New("empty integer")
	}
	for _, character := range raw {
		if character < '0' || character > '9' {
			return 0, errors.New("invalid integer")
		}
	}
	value, err := strconv.ParseUint(raw, 10, strconv.IntSize)
	if err != nil || value == 0 {
		return 0, errors.New("invalid positive integer")
	}
	return int(value), nil
}

func parseStrictBoundedInt(raw string, maximum int) (int, error) {
	value, err := parseStrictPositiveInt(raw)
	if err != nil || value > maximum {
		return 0, errors.New("integer outside allowed range")
	}
	return value, nil
}

func parseStrictPositiveDuration(raw string) (time.Duration, error) {
	if raw == "" || raw[0] == '+' || raw[0] == '-' {
		return 0, errors.New("invalid duration")
	}
	duration, err := time.ParseDuration(raw)
	if err != nil || duration <= 0 {
		return 0, errors.New("invalid positive duration")
	}
	return duration, nil
}
