package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ToledoVitor/GoContext/internal/embedding"
	"github.com/ToledoVitor/GoContext/internal/embedding/openaicompat"
)

func TestEmbeddingConfigDefaultsOffWithoutSecretOrClientActivity(t *testing.T) {
	options, backend := parseEmbeddingFlags(t)
	keyRead := false
	clientCreations := 0

	config, err := resolveEmbeddingConfig(
		options,
		backend,
		commandRoleIndex,
		func(name string) (string, bool) {
			if name == embeddingAPIKeyEnv {
				keyRead = true
			}
			return "", false
		},
		func(openaicompat.Config) (embedding.Embedder, error) {
			clientCreations++
			return nil, nil
		},
	)
	if err != nil {
		t.Fatalf("resolveEmbeddingConfig() error = %v", err)
	}
	if config.mode != semanticModeOff {
		t.Fatalf("resolved mode = %q, want %q", config.mode, semanticModeOff)
	}
	if config.backend != indexBackendSnapshot {
		t.Fatalf("resolved backend = %q, want %q", config.backend, indexBackendSnapshot)
	}
	if config.client != nil {
		t.Fatal("resolved client is non-nil when semantics are off")
	}
	if keyRead {
		t.Fatal("API key was read when semantics are off")
	}
	if clientCreations != 0 {
		t.Fatalf("client creations = %d, want 0", clientCreations)
	}
}

func TestEmbeddingConfigEnabledValidatesBeforeReadingKeyAndCreatesClient(t *testing.T) {
	options, backend := parseEmbeddingFlags(
		t,
		"--semantic", "required",
		"--embedding-base-url", "https://api.example.test/v1",
		"--embedding-model", "example-model",
		"--index-backend", "sqlite",
	)
	events := make([]string, 0, 3)
	created := make([]openaicompat.Config, 0, 2)
	client := stubEmbedder{}

	config, err := resolveEmbeddingConfig(
		options,
		backend,
		commandRoleIndex,
		func(name string) (string, bool) {
			if name != embeddingAPIKeyEnv {
				return "", false
			}
			events = append(events, "key")
			return "KEY_CANARY_DO_NOT_PRINT", true
		},
		func(adapterConfig openaicompat.Config) (embedding.Embedder, error) {
			events = append(events, "client")
			created = append(created, adapterConfig)
			return client, nil
		},
	)
	if err != nil {
		t.Fatalf("resolveEmbeddingConfig() error = %v", err)
	}
	if got, want := strings.Join(events, ","), "client,key,client"; got != want {
		t.Fatalf("configuration events = %q, want %q", got, want)
	}
	if len(created) != 2 {
		t.Fatalf("client creations = %d, want 2", len(created))
	}
	if created[0].APIKey != "" {
		t.Fatal("validation client received API key before non-secret validation")
	}
	if created[1].APIKey == "" {
		t.Fatal("resolved client did not receive configured API key")
	}
	if created[1].BatchSize != 32 || created[1].MaxInFlight != 2 || created[1].Timeout != 30*time.Second {
		t.Fatalf(
			"adapter defaults = batch %d, in-flight %d, timeout %s; want 32, 2, 30s",
			created[1].BatchSize,
			created[1].MaxInFlight,
			created[1].Timeout,
		)
	}
	if config.client != client {
		t.Fatal("resolved client differs from client factory result")
	}
}

func TestEmbeddingConfigClassifiesValidatedEndpointWithoutNetwork(t *testing.T) {
	tests := []struct {
		name       string
		baseURL    string
		wantEgress dataEgressClass
	}{
		{name: "remote HTTPS", baseURL: "https://api.example.test/v1", wantEgress: dataEgressExternal},
		{name: "Ollama IPv4 loopback HTTP", baseURL: "http://127.0.0.1:11434/v1", wantEgress: dataEgressLocal},
		{name: "IPv6 loopback HTTPS", baseURL: "https://[::1]:8443/v1", wantEgress: dataEgressLocal},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, backend := parseEmbeddingFlags(
				t,
				"--semantic", "preferred",
				"--embedding-base-url", test.baseURL,
				"--embedding-model", "example-model",
				"--index-backend", "sqlite",
			)
			config, err := resolveEmbeddingConfig(
				options,
				backend,
				commandRoleIndex,
				mapLookup(nil),
				func(adapterConfig openaicompat.Config) (embedding.Embedder, error) {
					return openaicompat.New(adapterConfig)
				},
			)
			if err != nil {
				t.Fatalf("resolveEmbeddingConfig() error = %v", err)
			}
			if config.client == nil {
				t.Fatal("resolved client is nil")
			}
			if config.egress != test.wantEgress {
				t.Fatalf("egress class = %q, want %q", config.egress, test.wantEgress)
			}
		})
	}
}

func TestIndexBackendMatrix(t *testing.T) {
	tests := []struct {
		name      string
		role      commandRole
		backend   string
		want      indexBackend
		wantError bool
	}{
		{name: "index snapshot", role: commandRoleIndex, backend: "snapshot", want: indexBackendSnapshot},
		{name: "index sqlite", role: commandRoleIndex, backend: "sqlite", want: indexBackendSQLite},
		{name: "index rejects auto", role: commandRoleIndex, backend: "auto", wantError: true},
		{name: "search snapshot", role: commandRoleSearch, backend: "snapshot", want: indexBackendSnapshot},
		{name: "search sqlite", role: commandRoleSearch, backend: "sqlite", want: indexBackendSQLite},
		{name: "search auto", role: commandRoleSearch, backend: "auto", want: indexBackendAuto},
		{name: "search rejects unknown without echo", role: commandRoleSearch, backend: "bad\x1b[31mBACKEND_CANARY", wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, backend := parseEmbeddingFlags(t, "--index-backend", test.backend)
			config, err := resolveEmbeddingConfig(
				options,
				backend,
				test.role,
				mapLookup(nil),
				func(openaicompat.Config) (embedding.Embedder, error) {
					t.Fatal("client factory called with semantic mode off")
					return nil, nil
				},
			)
			if test.wantError {
				if err == nil {
					t.Fatal("resolveEmbeddingConfig() error = nil, want backend error")
				}
				if strings.Contains(err.Error(), test.backend) || strings.ContainsRune(err.Error(), '\x1b') {
					t.Fatalf("backend error exposes raw value: %q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveEmbeddingConfig() error = %v", err)
			}
			if config.backend != test.want {
				t.Fatalf("resolved backend = %q, want %q", config.backend, test.want)
			}
		})
	}
}

func TestIndexBackendRejectsSemanticSnapshotBeforeSecretLookup(t *testing.T) {
	tests := []struct {
		name     string
		role     commandRole
		wantText string
	}{
		{name: "index", role: commandRoleIndex, wantText: "--index-backend sqlite"},
		{name: "search", role: commandRoleSearch, wantText: "--index-backend sqlite or auto"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, backend := parseEmbeddingFlags(
				t,
				"--semantic", "preferred",
				"--embedding-base-url", "https://api.example.test/v1",
				"--embedding-model", "example-model",
			)
			keyRead := false
			clientCreations := 0
			_, err := resolveEmbeddingConfig(
				options,
				backend,
				test.role,
				func(name string) (string, bool) {
					if name == embeddingAPIKeyEnv {
						keyRead = true
						return "KEY_BACKEND_CANARY", true
					}
					return "", false
				},
				func(adapterConfig openaicompat.Config) (embedding.Embedder, error) {
					clientCreations++
					return openaicompat.New(adapterConfig)
				},
			)
			if err == nil || !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("resolveEmbeddingConfig() error = %v, want fixed text %q", err, test.wantText)
			}
			if keyRead {
				t.Fatal("API key read before backend compatibility validation")
			}
			if clientCreations != 1 {
				t.Fatalf("validation client creations = %d, want 1", clientCreations)
			}
		})
	}
}

func TestEmbeddingConfigDimensionsAreStrictAndFlagOverridesEnvironment(t *testing.T) {
	tests := []struct {
		name           string
		flagValue      *string
		environment    string
		wantDimensions int
		wantError      bool
	}{
		{name: "absent lets provider decide"},
		{name: "environment", environment: "1536", wantDimensions: 1536},
		{name: "flag overrides environment", flagValue: stringPointer("768"), environment: "1536", wantDimensions: 768},
		{name: "blank flag overrides environment", flagValue: stringPointer(""), environment: "1536", wantError: true},
		{name: "zero flag", flagValue: stringPointer("0"), wantError: true},
		{name: "negative environment", environment: "-1", wantError: true},
		{name: "signed environment", environment: "+1", wantError: true},
		{name: "whitespace environment", environment: " 1", wantError: true},
		{name: "junk environment", environment: "12DIMS_CANARY", wantError: true},
		{name: "overflow environment", environment: "999999999999999999999999999999", wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := []string{
				"--semantic", "preferred",
				"--embedding-base-url", "https://api.example.test/v1",
				"--embedding-model", "example-model",
				"--index-backend", "sqlite",
			}
			if test.flagValue != nil {
				args = append(args, "--embedding-dimensions="+*test.flagValue)
			}
			options, backend := parseEmbeddingFlags(t, args...)
			environment := map[string]string{}
			if test.environment != "" {
				environment[embeddingDimensionsEnv] = test.environment
			}
			factoryCalled := false
			_, err := resolveEmbeddingConfig(
				options,
				backend,
				commandRoleIndex,
				mapLookup(environment),
				func(adapterConfig openaicompat.Config) (embedding.Embedder, error) {
					factoryCalled = true
					if adapterConfig.Dimensions != test.wantDimensions {
						t.Fatalf("adapter dimensions = %d, want %d", adapterConfig.Dimensions, test.wantDimensions)
					}
					return stubEmbedder{}, nil
				},
			)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "invalid embedding dimensions") {
					t.Fatalf("resolveEmbeddingConfig() error = %v, want dimensions error", err)
				}
				if factoryCalled {
					t.Fatal("client factory called for invalid dimensions")
				}
				if (test.environment != "" && strings.Contains(err.Error(), test.environment)) || strings.Contains(err.Error(), "DIMS_CANARY") {
					t.Fatalf("dimensions error exposes raw value: %q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveEmbeddingConfig() error = %v", err)
			}
			if !factoryCalled {
				t.Fatal("client factory was not called for valid dimensions")
			}
		})
	}
}

func TestEmbeddingConfigBatchSizeBoundsAndStrictParsing(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		want      int
		wantError bool
	}{
		{name: "minimum", value: "1", want: 1},
		{name: "maximum", value: "128", want: 128},
		{name: "zero", value: "0", wantError: true},
		{name: "above maximum", value: "129", wantError: true},
		{name: "negative", value: "-1", wantError: true},
		{name: "signed", value: "+1", wantError: true},
		{name: "whitespace", value: "1 ", wantError: true},
		{name: "junk", value: "1BATCH_CANARY", wantError: true},
		{name: "overflow", value: "999999999999999999999999999999", wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, backend := parseEmbeddingFlags(
				t,
				"--semantic", "preferred",
				"--embedding-base-url", "https://api.example.test/v1",
				"--embedding-model", "example-model",
				"--index-backend", "sqlite",
			)
			keyRead := false
			factoryCalled := false
			_, err := resolveEmbeddingConfig(
				options,
				backend,
				commandRoleIndex,
				func(name string) (string, bool) {
					if name == embeddingAPIKeyEnv {
						keyRead = true
						return "", false
					}
					if name == embeddingBatchSizeEnv {
						return test.value, true
					}
					return "", false
				},
				func(adapterConfig openaicompat.Config) (embedding.Embedder, error) {
					factoryCalled = true
					if adapterConfig.BatchSize != test.want {
						t.Fatalf("adapter batch size = %d, want %d", adapterConfig.BatchSize, test.want)
					}
					return stubEmbedder{}, nil
				},
			)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "invalid embedding batch size") {
					t.Fatalf("resolveEmbeddingConfig() error = %v, want batch size error", err)
				}
				if factoryCalled || keyRead {
					t.Fatalf("invalid batch reached client/key: factory=%t key=%t", factoryCalled, keyRead)
				}
				if strings.Contains(err.Error(), "BATCH_CANARY") {
					t.Fatalf("batch error exposes raw value: %q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveEmbeddingConfig() error = %v", err)
			}
			if !factoryCalled || !keyRead {
				t.Fatalf("valid batch did not reach client/key: factory=%t key=%t", factoryCalled, keyRead)
			}
		})
	}
}

func TestEmbeddingConfigMaxInFlightBoundsAndStrictParsing(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		want      int
		wantError bool
	}{
		{name: "minimum", value: "1", want: 1},
		{name: "maximum", value: "8", want: 8},
		{name: "zero", value: "0", wantError: true},
		{name: "above maximum", value: "9", wantError: true},
		{name: "negative", value: "-1", wantError: true},
		{name: "signed", value: "+1", wantError: true},
		{name: "whitespace", value: "1 ", wantError: true},
		{name: "junk", value: "1FLIGHT_CANARY", wantError: true},
		{name: "overflow", value: "999999999999999999999999999999", wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, backend := parseEmbeddingFlags(
				t,
				"--semantic", "preferred",
				"--embedding-base-url", "https://api.example.test/v1",
				"--embedding-model", "example-model",
				"--index-backend", "sqlite",
			)
			keyRead := false
			factoryCalled := false
			_, err := resolveEmbeddingConfig(
				options,
				backend,
				commandRoleIndex,
				func(name string) (string, bool) {
					if name == embeddingAPIKeyEnv {
						keyRead = true
						return "", false
					}
					if name == embeddingMaxInFlightEnv {
						return test.value, true
					}
					return "", false
				},
				func(adapterConfig openaicompat.Config) (embedding.Embedder, error) {
					factoryCalled = true
					if adapterConfig.MaxInFlight != test.want {
						t.Fatalf("adapter max in-flight = %d, want %d", adapterConfig.MaxInFlight, test.want)
					}
					return stubEmbedder{}, nil
				},
			)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "invalid embedding max in-flight") {
					t.Fatalf("resolveEmbeddingConfig() error = %v, want max in-flight error", err)
				}
				if factoryCalled || keyRead {
					t.Fatalf("invalid max in-flight reached client/key: factory=%t key=%t", factoryCalled, keyRead)
				}
				if strings.Contains(err.Error(), "FLIGHT_CANARY") {
					t.Fatalf("max in-flight error exposes raw value: %q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveEmbeddingConfig() error = %v", err)
			}
			if !factoryCalled || !keyRead {
				t.Fatalf("valid max in-flight did not reach client/key: factory=%t key=%t", factoryCalled, keyRead)
			}
		})
	}
}

func TestEmbeddingConfigTimeoutIsStrictPositiveGoDuration(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		want      time.Duration
		wantError bool
	}{
		{name: "nanosecond", value: "1ns", want: time.Nanosecond},
		{name: "composite", value: "1m30s", want: 90 * time.Second},
		{name: "zero", value: "0s", wantError: true},
		{name: "negative", value: "-1s", wantError: true},
		{name: "signed", value: "+1s", wantError: true},
		{name: "whitespace", value: "1s ", wantError: true},
		{name: "unitless", value: "1", wantError: true},
		{name: "junk", value: "1sTIMEOUT_CANARY", wantError: true},
		{name: "overflow", value: "999999999999999999999999h", wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, backend := parseEmbeddingFlags(
				t,
				"--semantic", "preferred",
				"--embedding-base-url", "https://api.example.test/v1",
				"--embedding-model", "example-model",
				"--index-backend", "sqlite",
			)
			keyRead := false
			factoryCalled := false
			_, err := resolveEmbeddingConfig(
				options,
				backend,
				commandRoleIndex,
				func(name string) (string, bool) {
					if name == embeddingAPIKeyEnv {
						keyRead = true
						return "", false
					}
					if name == embeddingTimeoutEnv {
						return test.value, true
					}
					return "", false
				},
				func(adapterConfig openaicompat.Config) (embedding.Embedder, error) {
					factoryCalled = true
					if adapterConfig.Timeout != test.want {
						t.Fatalf("adapter timeout = %s, want %s", adapterConfig.Timeout, test.want)
					}
					return stubEmbedder{}, nil
				},
			)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "invalid embedding timeout") {
					t.Fatalf("resolveEmbeddingConfig() error = %v, want timeout error", err)
				}
				if factoryCalled || keyRead {
					t.Fatalf("invalid timeout reached client/key: factory=%t key=%t", factoryCalled, keyRead)
				}
				if strings.Contains(err.Error(), "TIMEOUT_CANARY") {
					t.Fatalf("timeout error exposes raw value: %q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveEmbeddingConfig() error = %v", err)
			}
			if !factoryCalled || !keyRead {
				t.Fatalf("valid timeout did not reach client/key: factory=%t key=%t", factoryCalled, keyRead)
			}
		})
	}
}

func TestEmbeddingConfigEnabledRequiresBaseURLAndModel(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		wantText string
	}{
		{
			name:     "base URL",
			wantText: "embedding base URL is required",
		},
		{
			name: "model",
			env: map[string]string{
				embeddingBaseURLEnv: "https://api.example.test/v1",
			},
			wantText: "embedding model is required",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, backend := parseEmbeddingFlags(t, "--semantic", "preferred")
			_, err := resolveEmbeddingConfig(
				options,
				backend,
				commandRoleIndex,
				mapLookup(test.env),
				func(openaicompat.Config) (embedding.Embedder, error) {
					t.Fatal("client factory called for incomplete configuration")
					return nil, nil
				},
			)
			if err == nil || !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("resolveEmbeddingConfig() error = %v, want fixed text %q", err, test.wantText)
			}
		})
	}
}

func TestEmbeddingConfigOffFlagOverridesEnabledEnvironmentAndIgnoresAdapterValues(t *testing.T) {
	options, backend := parseEmbeddingFlags(t, "--semantic", "off")
	environment := map[string]string{
		semanticModeEnv:         "required",
		embeddingBaseURLEnv:     "http://not-loopback.invalid/v1",
		embeddingModelEnv:       "MODEL_OFF_CANARY\x1b[31m",
		embeddingDimensionsEnv:  "not-a-number",
		embeddingBatchSizeEnv:   "999",
		embeddingMaxInFlightEnv: "999",
		embeddingTimeoutEnv:     "not-a-duration",
		embeddingAPIKeyEnv:      "KEY_OFF_CANARY",
	}
	lookups := make([]string, 0)

	config, err := resolveEmbeddingConfig(
		options,
		backend,
		commandRoleIndex,
		func(name string) (string, bool) {
			lookups = append(lookups, name)
			value, ok := environment[name]
			return value, ok
		},
		func(openaicompat.Config) (embedding.Embedder, error) {
			t.Fatal("client factory called when --semantic off")
			return nil, nil
		},
	)
	if err != nil {
		t.Fatalf("resolveEmbeddingConfig() error = %v", err)
	}
	if config.mode != semanticModeOff {
		t.Fatalf("resolved mode = %q, want off", config.mode)
	}
	if len(lookups) != 0 {
		t.Fatalf("environment lookups with explicit off = %v, want none", lookups)
	}
}

func TestEmbeddingConfigEndpointAndModelWithoutModeStayOff(t *testing.T) {
	tests := []struct {
		name        string
		arguments   []string
		environment map[string]string
	}{
		{
			name: "environment",
			environment: map[string]string{
				embeddingBaseURLEnv: "https://api.example.test/v1",
				embeddingModelEnv:   "example-model",
				embeddingAPIKeyEnv:  "KEY_IMPLICIT_MODE_CANARY",
			},
		},
		{
			name: "flags",
			arguments: []string{
				"--embedding-base-url", "https://api.example.test/v1",
				"--embedding-model", "example-model",
			},
			environment: map[string]string{embeddingAPIKeyEnv: "KEY_IMPLICIT_MODE_CANARY"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, backend := parseEmbeddingFlags(t, test.arguments...)
			keyRead := false
			config, err := resolveEmbeddingConfig(
				options,
				backend,
				commandRoleSearch,
				func(name string) (string, bool) {
					if name == embeddingAPIKeyEnv {
						keyRead = true
					}
					value, ok := test.environment[name]
					return value, ok
				},
				func(openaicompat.Config) (embedding.Embedder, error) {
					t.Fatal("client factory called without explicit semantic mode")
					return nil, nil
				},
			)
			if err != nil {
				t.Fatalf("resolveEmbeddingConfig() error = %v", err)
			}
			if config.mode != semanticModeOff || config.client != nil {
				t.Fatalf("resolved implicit configuration = %v, want off without client", config)
			}
			if keyRead {
				t.Fatal("API key read without explicit semantic mode")
			}
		})
	}
}

func TestEmbeddingConfigNonSecretFlagsOverrideEnvironment(t *testing.T) {
	options, backend := parseEmbeddingFlags(
		t,
		"--semantic", "preferred",
		"--embedding-base-url", "https://flags.example.test/v1",
		"--embedding-model", "flag-model",
		"--index-backend", "sqlite",
	)
	environment := map[string]string{
		semanticModeEnv:     "off",
		embeddingBaseURLEnv: "http://remote-env.invalid/v1",
		embeddingModelEnv:   "environment-model",
	}
	factoryCalled := false

	config, err := resolveEmbeddingConfig(
		options,
		backend,
		commandRoleIndex,
		mapLookup(environment),
		func(adapterConfig openaicompat.Config) (embedding.Embedder, error) {
			factoryCalled = true
			if adapterConfig.BaseURL != "https://flags.example.test/v1" || adapterConfig.Model != "flag-model" {
				t.Fatal("adapter did not receive flag values")
			}
			return stubEmbedder{}, nil
		},
	)
	if err != nil {
		t.Fatalf("resolveEmbeddingConfig() error = %v", err)
	}
	if config.mode != semanticModePreferred || !factoryCalled {
		t.Fatalf("resolved flag override = %v, factory called = %t", config, factoryCalled)
	}
}

func TestEmbeddingConfigRejectsUnsafeEndpointBeforeSecretLookup(t *testing.T) {
	tests := []string{
		"http://localhost:11434/v1",
		"http://192.0.2.1/v1",
		"https://user:URL_CANARY@example.test/v1",
		"https://example.test/v1?URL_CANARY=1",
		"https://example.test/unsafe/URL_CANARY",
	}
	for _, baseURL := range tests {
		t.Run(baseURL, func(t *testing.T) {
			options, backend := parseEmbeddingFlags(
				t,
				"--semantic", "required",
				"--embedding-base-url", baseURL,
				"--embedding-model", "example-model",
				"--index-backend", "sqlite",
			)
			keyRead := false
			_, err := resolveEmbeddingConfig(
				options,
				backend,
				commandRoleIndex,
				func(name string) (string, bool) {
					if name == embeddingAPIKeyEnv {
						keyRead = true
						return "KEY_URL_CANARY", true
					}
					return "", false
				},
				func(adapterConfig openaicompat.Config) (embedding.Embedder, error) {
					return openaicompat.New(adapterConfig)
				},
			)
			if err == nil || !strings.Contains(err.Error(), "invalid embedding base URL") {
				t.Fatalf("resolveEmbeddingConfig() error = %v, want base URL error", err)
			}
			if keyRead {
				t.Fatal("API key read before endpoint validation")
			}
			if strings.Contains(err.Error(), "URL_CANARY") || strings.Contains(err.Error(), baseURL) {
				t.Fatalf("endpoint error exposes raw value: %q", err.Error())
			}
		})
	}
}

func TestEmbeddingConfigExplicitBlankFlagsOverrideEnvironment(t *testing.T) {
	tests := []struct {
		name     string
		argument string
		wantText string
	}{
		{name: "base URL", argument: "--embedding-base-url=", wantText: "embedding base URL is required"},
		{name: "model", argument: "--embedding-model=", wantText: "embedding model is required"},
	}
	environment := map[string]string{
		embeddingBaseURLEnv: "https://api.example.test/v1",
		embeddingModelEnv:   "environment-model",
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, backend := parseEmbeddingFlags(
				t,
				"--semantic", "preferred",
				"--index-backend", "sqlite",
				test.argument,
			)
			_, err := resolveEmbeddingConfig(
				options,
				backend,
				commandRoleIndex,
				mapLookup(environment),
				func(openaicompat.Config) (embedding.Embedder, error) {
					t.Fatal("client factory called after explicit blank flag")
					return nil, nil
				},
			)
			if err == nil || !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("resolveEmbeddingConfig() error = %v, want fixed text %q", err, test.wantText)
			}
		})
	}
}

func TestEmbeddingConfigFormattingNeverExposesKeyOrUntrustedValues(t *testing.T) {
	options, backend := parseEmbeddingFlags(
		t,
		"--semantic", "required",
		"--embedding-base-url", "https://api.example.test/v1",
		"--embedding-model", "MODEL_FORMAT_CANARY\x1b[31m",
		"--index-backend", "sqlite",
	)
	config, err := resolveEmbeddingConfig(
		options,
		backend,
		commandRoleIndex,
		mapLookup(map[string]string{embeddingAPIKeyEnv: "KEY_FORMAT_CANARY"}),
		func(adapterConfig openaicompat.Config) (embedding.Embedder, error) {
			return openaicompat.New(adapterConfig)
		},
	)
	if err != nil {
		t.Fatalf("resolveEmbeddingConfig() error = %v", err)
	}
	for _, formatted := range []string{
		fmt.Sprint(config),
		fmt.Sprintf("%+v", config),
		fmt.Sprintf("%#v", config),
	} {
		if strings.Contains(formatted, "KEY_FORMAT_CANARY") || strings.Contains(formatted, "MODEL_FORMAT_CANARY") || strings.ContainsRune(formatted, '\x1b') {
			t.Fatalf("formatted configuration exposes sensitive/untrusted value: %q", formatted)
		}
	}
}

func TestEmbeddingOptionsFormattingNeverExposesRawFlagValues(t *testing.T) {
	options, _ := parseEmbeddingFlags(
		t,
		"--semantic=MODE_OPTIONS_CANARY\x1b[31m",
		"--embedding-base-url=https://URL_OPTIONS_CANARY.example/v1",
		"--embedding-model=MODEL_OPTIONS_CANARY",
		"--embedding-dimensions=DIMENSIONS_OPTIONS_CANARY",
	)

	for _, formatted := range []string{
		fmt.Sprint(options),
		fmt.Sprintf("%+v", options),
		fmt.Sprintf("%#v", options),
	} {
		if strings.Contains(formatted, "OPTIONS_CANARY") || strings.ContainsRune(formatted, '\x1b') {
			t.Fatalf("formatted options expose raw flag value: %q", formatted)
		}
	}
}

func TestEmbeddingFlagsDoNotExposeAPIKeyMechanism(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	var options embeddingOptions
	backend := indexBackendSnapshot
	addEmbeddingFlags(flags, &options, &backend)

	if flagDefinition := flags.Lookup("embedding-api-key"); flagDefinition != nil {
		t.Fatal("embedding-api-key flag exists")
	}
	var usage strings.Builder
	flags.SetOutput(&usage)
	flags.PrintDefaults()
	if strings.Contains(strings.ToLower(usage.String()), "api-key") {
		t.Fatalf("flag usage exposes API key mechanism: %q", usage.String())
	}
}

func TestIndexBackendFlagTracksExplicitSupplyAndRejectsBlank(t *testing.T) {
	implicitOptions, implicitBackend := parseEmbeddingFlags(t)
	implicit, err := resolveEmbeddingConfig(
		implicitOptions,
		implicitBackend,
		commandRoleSearch,
		mapLookup(nil),
		func(openaicompat.Config) (embedding.Embedder, error) { return stubEmbedder{}, nil },
	)
	if err != nil {
		t.Fatalf("resolveEmbeddingConfig(implicit) error = %v", err)
	}
	if implicit.backend != indexBackendSnapshot || implicit.backendExplicit {
		t.Fatalf("implicit backend = %q explicit=%t, want snapshot false", implicit.backend, implicit.backendExplicit)
	}

	explicitOptions, explicitBackend := parseEmbeddingFlags(t, "--index-backend=snapshot")
	explicit, err := resolveEmbeddingConfig(
		explicitOptions,
		explicitBackend,
		commandRoleSearch,
		mapLookup(nil),
		func(openaicompat.Config) (embedding.Embedder, error) { return stubEmbedder{}, nil },
	)
	if err != nil {
		t.Fatalf("resolveEmbeddingConfig(explicit) error = %v", err)
	}
	if explicit.backend != indexBackendSnapshot || !explicit.backendExplicit {
		t.Fatalf("explicit backend = %q explicit=%t, want snapshot true", explicit.backend, explicit.backendExplicit)
	}

	blankOptions, blankBackend := parseEmbeddingFlags(t, "--index-backend=")
	_, err = resolveEmbeddingConfig(
		blankOptions,
		blankBackend,
		commandRoleSearch,
		mapLookup(nil),
		func(openaicompat.Config) (embedding.Embedder, error) { return stubEmbedder{}, nil },
	)
	if !errors.Is(err, errInvalidIndexBackend) {
		t.Fatalf("resolveEmbeddingConfig(blank backend) error = %v, want fixed invalid backend", err)
	}
}

func parseEmbeddingFlags(t *testing.T, args ...string) (embeddingOptions, indexBackend) {
	t.Helper()
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	var options embeddingOptions
	backend := indexBackendSnapshot
	addEmbeddingFlags(flags, &options, &backend)
	if err := flags.Parse(args); err != nil {
		t.Fatalf("Parse(%v) error = %v", args, err)
	}
	return options, backend
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func stringPointer(value string) *string {
	return &value
}

func clearEmbeddingEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		semanticModeEnv,
		embeddingBaseURLEnv,
		embeddingModelEnv,
		embeddingDimensionsEnv,
		embeddingAPIKeyEnv,
		embeddingBatchSizeEnv,
		embeddingMaxInFlightEnv,
		embeddingTimeoutEnv,
	} {
		original, present := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("Unsetenv(%q) error = %v", name, err)
		}
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(name, original)
				return
			}
			_ = os.Unsetenv(name)
		})
	}
}

type stubEmbedder struct{}

func (stubEmbedder) Profile() embedding.Profile {
	return embedding.Profile{Fingerprint: "test", Model: "test"}
}

func (stubEmbedder) Embed(context.Context, embedding.Purpose, []string) (embedding.Batch, error) {
	return embedding.Batch{}, nil
}
