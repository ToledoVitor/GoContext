package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ToledoVitor/GoContext/internal/embedding"
)

func TestClientEmbedSendsWireContractAndRestoresResponseOrder(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", request.Method)
		}
		if request.URL.Path != "/v1/embeddings" {
			t.Errorf("path = %q, want /v1/embeddings", request.URL.Path)
		}
		if contentType := request.Header.Get("Content-Type"); contentType != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", contentType)
		}
		if authorization := request.Header.Get("Authorization"); authorization != "Bearer credential-canary" {
			t.Errorf("Authorization = %q, want configured bearer", authorization)
		}

		var body struct {
			Model          string   `json:"model"`
			Input          []string `json:"input"`
			EncodingFormat string   `json:"encoding_format"`
			Dimensions     int      `json:"dimensions"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body.Model != "fixture-model" {
			t.Errorf("model = %q, want fixture-model", body.Model)
		}
		if !reflect.DeepEqual(body.Input, []string{"synthetic-alpha", "synthetic-beta"}) {
			t.Errorf("input = %#v, want ordered synthetic input", body.Input)
		}
		if body.EncodingFormat != "float" {
			t.Errorf("encoding_format = %q, want float", body.EncodingFormat)
		}
		if body.Dimensions != 2 {
			t.Errorf("dimensions = %d, want 2", body.Dimensions)
		}

		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"object":"list",
			"data":[
				{"object":"embedding","index":1,"embedding":[0,1]},
				{"object":"embedding","index":0,"embedding":[1,0]}
			],
			"model":"fixture-model",
			"usage":{"prompt_tokens":3,"total_tokens":5}
		}`))
	}))
	defer server.Close()

	client, err := New(Config{
		BaseURL:    server.URL + "/v1",
		Model:      "fixture-model",
		APIKey:     "credential-canary",
		Dimensions: 2,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	batch, err := client.Embed(context.Background(), embedding.PurposeDocument, []string{"synthetic-alpha", "synthetic-beta"})
	if err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	wantVectors := []embedding.Vector{{1, 0}, {0, 1}}
	if !reflect.DeepEqual(batch.Vectors, wantVectors) {
		t.Fatalf("Embed().Vectors = %#v, want %#v", batch.Vectors, wantVectors)
	}
	if batch.Profile != client.Profile() || batch.Dimensions != 2 || batch.Requests != 1 || batch.UsageTokens != 5 {
		t.Fatalf("Embed() metadata = %#v", batch)
	}
}

func TestClientRejectsOversizedResponseWithoutExposingRemoteBody(t *testing.T) {
	const remoteCanary = "remote-response-canary"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"padding":"`))
		_, _ = writer.Write([]byte(strings.Repeat("x", 16<<20)))
		_, _ = writer.Write([]byte(remoteCanary + `","data":[{"index":0,"embedding":[1]}]}`))
	}))
	defer server.Close()

	client := newFixtureClient(t, server.URL, 1)
	_, err := client.Embed(context.Background(), embedding.PurposeQuery, []string{"input-canary"})
	if err == nil {
		t.Fatal("Embed() error = nil, want oversized response rejection")
	}
	if strings.Contains(err.Error(), remoteCanary) || strings.Contains(err.Error(), "input-canary") {
		t.Fatalf("Embed() error exposed remote or input content: %v", err)
	}
}

func TestClientRejectsInvalidResponseBatchShape(t *testing.T) {
	tests := []struct {
		name       string
		dimensions int
		textCount  int
		body       string
	}{
		{name: "missing item", dimensions: 2, body: `{"data":[]}`},
		{name: "duplicate index", dimensions: 2, textCount: 2, body: `{"data":[{"index":0,"embedding":[1,0]},{"index":0,"embedding":[0,1]}]}`},
		{name: "out of range index", dimensions: 2, textCount: 2, body: `{"data":[{"index":2,"embedding":[1,0]},{"index":0,"embedding":[0,1]}]}`},
		{name: "missing index", dimensions: 1, body: `{"data":[{"embedding":[1]}]}`},
		{name: "null index", dimensions: 1, body: `{"data":[{"index":null,"embedding":[1]}]}`},
		{name: "requested dimension mismatch", dimensions: 2, body: `{"data":[{"index":0,"embedding":[1]}]}`},
		{name: "inconsistent inferred dimensions", dimensions: 0, textCount: 2, body: `{"data":[{"index":0,"embedding":[1,0]},{"index":1,"embedding":[1]}]}`},
		{name: "null vector component", dimensions: 2, body: `{"data":[{"index":0,"embedding":[1,null]}]}`},
		{name: "non finite component", dimensions: 1, body: `{"data":[{"index":0,"embedding":[1e1000]}]}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()

			client := newFixtureClient(t, server.URL, test.dimensions)
			textCount := test.textCount
			if textCount == 0 {
				textCount = 1
			}
			texts := make([]string, textCount)
			for index := range texts {
				texts[index] = fmt.Sprintf("synthetic-%d", index)
			}
			_, err := client.Embed(context.Background(), embedding.PurposeDocument, texts)
			if !errors.Is(err, embedding.ErrInvalidBatch) {
				t.Fatalf("Embed() error = %v, want %v", err, embedding.ErrInvalidBatch)
			}
		})
	}
}

func TestClientLoopbackDialValidationChecksConnectedPeerBeforeHTTP(t *testing.T) {
	tests := []struct {
		name    string
		peerIP  string
		wantErr bool
	}{
		{name: "IPv4 loopback", peerIP: "127.42.0.9"},
		{name: "IPv6 loopback", peerIP: "::1"},
		{name: "hostile non-loopback peer", peerIP: "192.0.2.10", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clientSide, serverSide := net.Pipe()
			defer serverSide.Close()
			wrapped := &remoteAddrConn{
				Conn: clientSide,
				addr: &net.TCPAddr{IP: net.ParseIP(test.peerIP), Port: 11434},
			}
			dial := requireLoopbackPeer(func(context.Context, string, string) (net.Conn, error) {
				return wrapped, nil
			})

			connection, err := dial(context.Background(), "tcp", "configured-endpoint:11434")
			if test.wantErr {
				if err == nil || connection != nil {
					t.Fatalf("dial() = (%v, %v), want rejected peer", connection, err)
				}
				writeErr := serverSide.SetWriteDeadline(time.Now())
				if writeErr == nil {
					_, writeErr = serverSide.Write([]byte("must-not-reach-http"))
				}
				if writeErr == nil {
					t.Fatal("hostile connection remained open for HTTP")
				}
				return
			}
			if err != nil || connection == nil {
				t.Fatalf("dial() = (%v, %v), want accepted loopback peer", connection, err)
			}
			_ = connection.Close()
		})
	}
}

func TestRetryTemporaryFailuresStopsAfterThreeAttempts(t *testing.T) {
	tests := []struct {
		name      string
		transport roundTripFunc
	}{
		{
			name: "temporary connection",
			transport: func(*http.Request) (*http.Response, error) {
				return nil, temporaryTransportError{}
			},
		},
		{name: "408", transport: statusTransport(http.StatusRequestTimeout, "remote-408-canary")},
		{name: "429", transport: statusTransport(http.StatusTooManyRequests, "remote-429-canary")},
		{name: "500", transport: statusTransport(http.StatusInternalServerError, "remote-500-canary")},
		{name: "503", transport: statusTransport(http.StatusServiceUnavailable, "remote-503-canary")},
		{name: "599", transport: statusTransport(599, "remote-599-canary")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newRetryClient(t)
			attempts := 0
			client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				attempts++
				return test.transport.RoundTrip(request)
			})
			var waits []time.Duration
			client.sleep = func(context.Context, time.Duration) error {
				waits = append(waits, time.Duration(len(waits)+1)*100*time.Millisecond)
				return nil
			}

			_, err := client.Embed(context.Background(), embedding.PurposeQuery, []string{"retry-input-canary"})
			if !errors.Is(err, embedding.ErrSemanticUnavailable) {
				t.Fatalf("Embed() error = %v, want %v", err, embedding.ErrSemanticUnavailable)
			}
			if attempts != 3 {
				t.Fatalf("attempts = %d, want 3", attempts)
			}
			if got := embedding.AttemptedRequests(err); got != 3 {
				t.Fatalf("AttemptedRequests(Embed error) = %d, want 3", got)
			}
			if len(waits) != 2 {
				t.Fatalf("waits = %d, want 2", len(waits))
			}
			for _, canary := range []string{"retry-input-canary", "remote-", "temporary-transport-canary", "credential-canary"} {
				if strings.Contains(err.Error(), canary) {
					t.Fatalf("Embed() error exposed %q: %v", canary, err)
				}
			}
		})
	}
}

func TestClientAggregatesAttemptedRequestsAcrossCanceledConcurrentBatches(t *testing.T) {
	client := newRetryClient(t)
	client.config.BatchSize = 1
	client.config.MaxInFlight = 2
	var calls atomic.Int64
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		call := calls.Add(1)
		if call <= 2 {
			started <- struct{}{}
			<-release
		}
		return responseWithStatus(http.StatusServiceUnavailable, "concurrent-body-canary"), nil
	})
	client.sleep = func(context.Context, time.Duration) error { return nil }

	done := make(chan error, 1)
	go func() {
		_, err := client.Embed(context.Background(), embedding.PurposeDocument, []string{"batch-one", "batch-two"})
		done <- err
	}()
	<-started
	<-started
	close(release)
	err := <-done
	if !errors.Is(err, embedding.ErrSemanticUnavailable) {
		t.Fatalf("Embed() error = %v, want ErrSemanticUnavailable", err)
	}
	requests := embedding.AttemptedRequests(err)
	if got := int(calls.Load()); requests != got || requests <= 3 {
		t.Fatalf("AttemptedRequests(error)/HTTP calls = %d/%d, want equal aggregate above one batch", requests, got)
	}
	for _, canary := range []string{"batch-one", "batch-two", "concurrent-body-canary"} {
		if strings.Contains(err.Error(), canary) {
			t.Fatalf("Embed() error exposes %q: %v", canary, err)
		}
	}
}

func TestClientInternalTimeoutPreservesAttemptedRequestCount(t *testing.T) {
	client := newRetryClient(t)
	client.config.Timeout = 20 * time.Millisecond
	var calls atomic.Int64
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})

	_, err := client.Embed(context.Background(), embedding.PurposeQuery, []string{"timeout-input-canary"})
	if !errors.Is(err, embedding.ErrSemanticUnavailable) {
		t.Fatalf("Embed() error = %v, want ErrSemanticUnavailable", err)
	}
	if got, want := embedding.AttemptedRequests(err), int(calls.Load()); got != want || got != 1 {
		t.Fatalf("AttemptedRequests(error)/HTTP calls = %d/%d, want 1/1", got, want)
	}
	if strings.Contains(err.Error(), "timeout-input-canary") {
		t.Fatalf("Embed() error exposes input: %v", err)
	}
}

func TestClientCanceledBeforeEmbedReportsZeroAttemptedRequests(t *testing.T) {
	client := newRetryClient(t)
	var calls atomic.Int64
	client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return embeddingSuccessResponse(), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.Embed(ctx, embedding.PurposeQuery, []string{"never-sent-canary"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Embed() error = %v, want context.Canceled", err)
	}
	if got := embedding.AttemptedRequests(err); got != 0 || calls.Load() != 0 {
		t.Fatalf("AttemptedRequests(error)/HTTP calls = %d/%d, want 0/0", got, calls.Load())
	}
}

func TestRetryDoesNotRetryOtherClientErrors(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 422, 600} {
		t.Run(fmt.Sprintf("status %d", status), func(t *testing.T) {
			client := newRetryClient(t)
			attempts := 0
			client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				attempts++
				return statusTransport(status, "remote-error-body-canary").RoundTrip(request)
			})

			_, err := client.Embed(context.Background(), embedding.PurposeDocument, []string{"client-error-input-canary"})
			if err == nil || errors.Is(err, embedding.ErrSemanticUnavailable) {
				t.Fatalf("Embed() error = %v, want non-temporary rejection", err)
			}
			if attempts != 1 {
				t.Fatalf("attempts = %d, want 1", attempts)
			}
			for _, canary := range []string{"remote-error-body-canary", "client-error-input-canary", "credential-canary"} {
				if strings.Contains(err.Error(), canary) {
					t.Fatalf("Embed() error exposed %q: %v", canary, err)
				}
			}
		})
	}
}

func TestRetryMaxIntCapsBeforeAddingAttempt(t *testing.T) {
	client, err := New(Config{
		BaseURL:    "https://api.example.com/v1",
		Model:      "fixture-model",
		Dimensions: 1,
		MaxRetries: math.MaxInt,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	attempts := 0
	client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		if attempts < 3 {
			return responseWithStatus(http.StatusServiceUnavailable, "retry-body-canary"), nil
		}
		return embeddingSuccessResponse(), nil
	})
	client.sleep = func(context.Context, time.Duration) error { return nil }

	batch, err := client.Embed(context.Background(), embedding.PurposeQuery, []string{"retry-max-input-canary"})
	if err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	if attempts != 3 || batch.Requests != 3 {
		t.Fatalf("attempts/Requests = %d/%d, want 3/3", attempts, batch.Requests)
	}
}

func TestRetryAfterIsHonoredOnlyWithinContextDeadline(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	tests := []struct {
		name       string
		retryAfter string
		deadline   time.Duration
		wantWait   time.Duration
		wantCalls  int
		wantErr    error
	}{
		{name: "delta seconds within deadline", retryAfter: "2", deadline: 3 * time.Second, wantWait: 2 * time.Second, wantCalls: 2},
		{name: "HTTP date within deadline", retryAfter: now.Add(2 * time.Second).Format(http.TimeFormat), deadline: 3 * time.Second, wantWait: 2 * time.Second, wantCalls: 2},
		{name: "beyond deadline", retryAfter: "2", deadline: time.Second, wantCalls: 1, wantErr: embedding.ErrSemanticUnavailable},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newRetryClient(t)
			client.now = func() time.Time { return now }
			calls := 0
			client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					response := responseWithStatus(http.StatusTooManyRequests, "retry-body-canary")
					response.Header.Set("Retry-After", test.retryAfter)
					return response, nil
				}
				return embeddingSuccessResponse(), nil
			})
			var waits []time.Duration
			client.sleep = func(_ context.Context, wait time.Duration) error {
				waits = append(waits, wait)
				return nil
			}
			ctx, cancel := context.WithDeadline(context.Background(), now.Add(test.deadline))
			defer cancel()

			batch, err := client.Embed(ctx, embedding.PurposeQuery, []string{"retry-after-input-canary"})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Embed() error = %v, want %v", err, test.wantErr)
			}
			if calls != test.wantCalls {
				t.Fatalf("calls = %d, want %d", calls, test.wantCalls)
			}
			if test.wantWait == 0 {
				if len(waits) != 0 {
					t.Fatalf("waits = %v, want none", waits)
				}
			} else if !reflect.DeepEqual(waits, []time.Duration{test.wantWait}) {
				t.Fatalf("waits = %v, want [%s]", waits, test.wantWait)
			}
			if test.wantErr == nil && batch.Requests != 2 {
				t.Fatalf("Embed().Requests = %d, want 2", batch.Requests)
			}
		})
	}
}

func TestRetryAfterWithoutParentDeadlineUsesClientTimeout(t *testing.T) {
	now := time.Now().UTC()
	client, err := New(Config{
		BaseURL:    "https://api.example.com/v1",
		Model:      "fixture-model",
		Dimensions: 1,
		Timeout:    time.Second,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	client.now = func() time.Time { return now }
	calls := 0
	client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			response := responseWithStatus(http.StatusTooManyRequests, "retry-body-canary")
			response.Header.Set("Retry-After", "2")
			return response, nil
		}
		return embeddingSuccessResponse(), nil
	})
	waits := 0
	client.sleep = func(context.Context, time.Duration) error {
		waits++
		return nil
	}

	_, err = client.Embed(context.Background(), embedding.PurposeQuery, []string{"timeout-input-canary"})
	if !errors.Is(err, embedding.ErrSemanticUnavailable) {
		t.Fatalf("Embed() error = %v, want %v", err, embedding.ErrSemanticUnavailable)
	}
	if calls != 1 || waits != 0 {
		t.Fatalf("calls/waits = %d/%d, want 1/0", calls, waits)
	}
}

func TestClientBatchesByCountAndEncodedBytesWhilePreservingGlobalOrder(t *testing.T) {
	tests := []struct {
		name          string
		texts         []string
		batchSize     int
		maxBatchBytes int
		wantRequests  int
	}{
		{
			name:          "count limit",
			texts:         []string{"synthetic-1", "synthetic-2", "synthetic-3", "synthetic-4", "synthetic-5"},
			batchSize:     2,
			maxBatchBytes: 1 << 20,
			wantRequests:  3,
		},
		{
			name: "encoded byte limit",
			texts: []string{
				strings.Repeat("a", 90) + "1",
				strings.Repeat("b", 90) + "2",
				strings.Repeat("c", 90) + "3",
				strings.Repeat("d", 90) + "4",
				strings.Repeat("e", 90) + "5",
			},
			batchSize:     10,
			maxBatchBytes: 200,
			wantRequests:  5,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var mutex sync.Mutex
			var requestSizes []int
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				raw, err := io.ReadAll(request.Body)
				if err != nil {
					t.Errorf("read request: %v", err)
					return
				}
				mutex.Lock()
				requestSizes = append(requestSizes, len(raw))
				mutex.Unlock()

				var body struct {
					Input []string `json:"input"`
				}
				if err := json.Unmarshal(raw, &body); err != nil {
					t.Errorf("decode request: %v", err)
					return
				}
				response := struct {
					Data []struct {
						Index     int       `json:"index"`
						Embedding []float32 `json:"embedding"`
					} `json:"data"`
					Usage struct {
						TotalTokens int `json:"total_tokens"`
					} `json:"usage"`
				}{Data: make([]struct {
					Index     int       `json:"index"`
					Embedding []float32 `json:"embedding"`
				}, len(body.Input))}
				for localIndex := len(body.Input) - 1; localIndex >= 0; localIndex-- {
					last := body.Input[localIndex][len(body.Input[localIndex])-1]
					response.Data[len(body.Input)-1-localIndex].Index = localIndex
					response.Data[len(body.Input)-1-localIndex].Embedding = fixtureUnitVector(last)
				}
				response.Usage.TotalTokens = len(body.Input)
				writer.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(writer).Encode(response)
			}))
			defer server.Close()

			client, err := New(Config{
				BaseURL:       server.URL + "/v1",
				Model:         "fixture-model",
				Dimensions:    2,
				BatchSize:     test.batchSize,
				MaxBatchBytes: test.maxBatchBytes,
			})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			batch, err := client.Embed(context.Background(), embedding.PurposeDocument, test.texts)
			if err != nil {
				t.Fatalf("Embed() error = %v", err)
			}

			if batch.Requests != test.wantRequests {
				t.Errorf("Embed().Requests = %d, want %d", batch.Requests, test.wantRequests)
			}
			if batch.UsageTokens != 5 {
				t.Errorf("Embed().UsageTokens = %d, want 5", batch.UsageTokens)
			}
			wantVectors := []embedding.Vector{{1, 0}, {0, 1}, {-1, 0}, {0, -1}, {0.6, 0.8}}
			if !reflect.DeepEqual(batch.Vectors, wantVectors) {
				t.Errorf("Embed().Vectors = %#v, want %#v", batch.Vectors, wantVectors)
			}
			mutex.Lock()
			defer mutex.Unlock()
			if len(requestSizes) != test.wantRequests {
				t.Fatalf("HTTP requests = %d, want %d", len(requestSizes), test.wantRequests)
			}
			for _, size := range requestSizes {
				if size > test.maxBatchBytes {
					t.Errorf("encoded request size = %d, exceeds %d", size, test.maxBatchBytes)
				}
			}
		})
	}
}

func TestClientRejectsSingletonAboveBatchByteLimitWithoutNetwork(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer server.Close()
	client, err := New(Config{
		BaseURL:       server.URL + "/v1",
		Model:         "fixture-model",
		Dimensions:    1,
		BatchSize:     10,
		MaxBatchBytes: 100,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = client.Embed(context.Background(), embedding.PurposeDocument, []string{strings.Repeat("input-canary", 20)})
	if !errors.Is(err, embedding.ErrInvalidBatch) {
		t.Fatalf("Embed() error = %v, want %v", err, embedding.ErrInvalidBatch)
	}
	if requests != 0 {
		t.Fatalf("HTTP requests = %d, want 0", requests)
	}
}

func TestClientBatchSizeMaxIntDoesNotOverflow(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		var body struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if !reflect.DeepEqual(body.Input, []string{"synthetic-alpha", "synthetic-beta"}) {
			t.Errorf("input = %#v, want both inputs in one request", body.Input)
		}
		_, _ = writer.Write([]byte(`{"data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}]}`))
	}))
	defer server.Close()

	client, err := New(Config{
		BaseURL:    server.URL + "/v1",
		Model:      "fixture-model",
		Dimensions: 2,
		BatchSize:  math.MaxInt,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	batch, err := client.Embed(context.Background(), embedding.PurposeDocument, []string{"synthetic-alpha", "synthetic-beta"})
	if err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	if requests != 1 {
		t.Fatalf("HTTP requests = %d, want 1", requests)
	}
	if !reflect.DeepEqual(batch.Vectors, []embedding.Vector{{1, 0}, {0, 1}}) {
		t.Fatalf("Embed().Vectors = %#v, want ordered vectors", batch.Vectors)
	}
}

func TestClientLimitsConcurrentBatchesAndPreservesOrder(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		if current == 2 {
			releaseOnce.Do(func() { close(release) })
		}
		select {
		case <-release:
		case <-time.After(100 * time.Millisecond):
		}

		var body struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		last := body.Input[0][len(body.Input[0])-1]
		_ = json.NewEncoder(writer).Encode(struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}{Data: []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}{{Index: 0, Embedding: fixtureUnitVector(last)}}})
	}))
	defer server.Close()

	client, err := New(Config{
		BaseURL:     server.URL + "/v1",
		Model:       "fixture-model",
		Dimensions:  2,
		BatchSize:   1,
		MaxInFlight: 2,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	batch, err := client.Embed(context.Background(), embedding.PurposeDocument, []string{"synthetic-1", "synthetic-2", "synthetic-3", "synthetic-4"})
	if err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	if maximum.Load() != 2 {
		t.Fatalf("maximum concurrent requests = %d, want 2", maximum.Load())
	}
	if !reflect.DeepEqual(batch.Vectors, []embedding.Vector{{1, 0}, {0, 1}, {-1, 0}, {0, -1}}) {
		t.Fatalf("Embed().Vectors = %#v, want global input order", batch.Vectors)
	}
}

func fixtureUnitVector(digit byte) []float32 {
	switch digit {
	case '1':
		return []float32{1, 0}
	case '2':
		return []float32{0, 1}
	case '3':
		return []float32{-1, 0}
	case '4':
		return []float32{0, -1}
	default:
		return []float32{0.6, 0.8}
	}
}

func TestClientCancelsInflightBatchAfterFirstFatalError(t *testing.T) {
	slowStarted := make(chan struct{})
	slowCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if body.Input[0] == "fatal-batch" {
			select {
			case <-slowStarted:
			case <-time.After(250 * time.Millisecond):
			}
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte("fatal-remote-body-canary"))
			return
		}
		close(slowStarted)
		<-request.Context().Done()
		close(slowCanceled)
	}))
	defer server.Close()

	client, err := New(Config{
		BaseURL:     server.URL + "/v1",
		Model:       "fixture-model",
		Dimensions:  1,
		BatchSize:   1,
		MaxInFlight: 2,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	_, err = client.Embed(context.Background(), embedding.PurposeDocument, []string{"fatal-batch", "slow-batch"})
	if err == nil || errors.Is(err, embedding.ErrSemanticUnavailable) {
		t.Fatalf("Embed() error = %v, want fatal provider rejection", err)
	}
	select {
	case <-slowCanceled:
	case <-time.After(time.Second):
		t.Fatal("in-flight sibling was not canceled")
	}
	if strings.Contains(err.Error(), "fatal-remote-body-canary") {
		t.Fatalf("Embed() error exposed remote body: %v", err)
	}
}

func TestClientPreservesParentContextCancellation(t *testing.T) {
	started := make(chan struct{})
	releaseHandler := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		select {
		case <-request.Context().Done():
		case <-releaseHandler:
		}
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	client := newFixtureClient(t, server.URL, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Embed(ctx, embedding.PurposeQuery, []string{"cancellation-input-canary"})
		done <- err
	}()
	<-started
	cancel()
	err := <-done
	close(releaseHandler)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Embed() error = %v, want %v", err, context.Canceled)
	}
}

func TestClientNormalizesFiniteVectorsAndRejectsZeroVector(t *testing.T) {
	tests := []struct {
		name    string
		vector  string
		want    embedding.Vector
		wantErr error
	}{
		{name: "normalizes finite vector", vector: `[3,4]`, want: embedding.Vector{0.6, 0.8}},
		{name: "rejects zero vector", vector: `[0,0]`, wantErr: embedding.ErrInvalidVector},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = writer.Write([]byte(`{"data":[{"index":0,"embedding":` + test.vector + `}]}`))
			}))
			defer server.Close()
			client := newFixtureClient(t, server.URL, 2)

			batch, err := client.Embed(context.Background(), embedding.PurposeDocument, []string{"normalization-input-canary"})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Embed() error = %v, want %v", err, test.wantErr)
			}
			if test.wantErr == nil && !reflect.DeepEqual(batch.Vectors[0], test.want) {
				t.Fatalf("Embed().Vectors[0] = %#v, want %#v", batch.Vectors[0], test.want)
			}
		})
	}
}

func TestClientOmitsAuthorizationAndUnrequestedDimensions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if authorization := request.Header.Get("Authorization"); authorization != "" {
			t.Errorf("Authorization = %q, want omitted", authorization)
		}
		var fields map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&fields); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if _, exists := fields["dimensions"]; exists {
			t.Error("dimensions field was sent when not requested")
		}
		_, _ = writer.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`))
	}))
	defer server.Close()

	client := newFixtureClient(t, server.URL, 0)
	batch, err := client.Embed(context.Background(), embedding.PurposeQuery, []string{"synthetic-query"})
	if err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	if batch.Dimensions != 2 {
		t.Fatalf("Embed().Dimensions = %d, want inferred 2", batch.Dimensions)
	}
}

func TestClientRejectsRedirectAndSanitizesRemoteFailures(t *testing.T) {
	const (
		apiKeyCanary = "credential-canary"
		inputCanary  = "input-content-canary"
		remoteCanary = "remote-error-body-canary"
	)
	tests := []struct {
		name    string
		handler http.Handler
	}{
		{
			name: "redirect",
			handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				http.Redirect(writer, request, "/redirect-target", http.StatusTemporaryRedirect)
			}),
		},
		{
			name: "remote client error body",
			handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(http.StatusBadRequest)
				_, _ = writer.Write([]byte(remoteCanary))
			}),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			redirectTargets := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/redirect-target" {
					redirectTargets++
					_, _ = writer.Write([]byte(remoteCanary))
					return
				}
				test.handler.ServeHTTP(writer, request)
			}))
			defer server.Close()
			client, err := New(Config{
				BaseURL:    server.URL + "/v1",
				Model:      "fixture-model",
				APIKey:     apiKeyCanary,
				Dimensions: 1,
			})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			_, err = client.Embed(context.Background(), embedding.PurposeDocument, []string{inputCanary})
			if err == nil {
				t.Fatal("Embed() error = nil, want rejected response")
			}
			if redirectTargets != 0 {
				t.Fatalf("redirect target requests = %d, want 0", redirectTargets)
			}
			for _, canary := range []string{apiKeyCanary, inputCanary, remoteCanary} {
				if strings.Contains(err.Error(), canary) {
					t.Fatalf("Embed() error exposed %q: %v", canary, err)
				}
			}
		})
	}
}

func TestClientOpenAIAndOllamaResponseSubsetConformance(t *testing.T) {
	fixtures := []struct {
		name       string
		response   string
		wantTokens int
	}{
		{
			name: "OpenAI subset",
			response: `{
				"object":"list",
				"data":[
					{"object":"embedding","index":1,"embedding":[0,2]},
					{"object":"embedding","index":0,"embedding":[1,0]}
				],
				"model":"fixture-model",
				"usage":{"prompt_tokens":7,"total_tokens":9}
			}`,
			wantTokens: 9,
		},
		{
			name: "Ollama OpenAI-compatible subset",
			response: `{
				"model":"fixture-model",
				"data":[
					{"embedding":[0,2],"index":1},
					{"embedding":[1,0],"index":0}
				]
			}`,
		},
	}

	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(fixture.response))
			}))
			defer server.Close()
			client := newFixtureClient(t, server.URL, 2)

			batch, err := client.Embed(context.Background(), embedding.PurposeDocument, []string{"synthetic-alpha", "synthetic-beta"})
			if err != nil {
				t.Fatalf("Embed() error = %v", err)
			}
			if !reflect.DeepEqual(batch.Vectors, []embedding.Vector{{1, 0}, {0, 1}}) {
				t.Fatalf("Embed().Vectors = %#v, want ordered normalized vectors", batch.Vectors)
			}
			if batch.UsageTokens != fixture.wantTokens {
				t.Fatalf("Embed().UsageTokens = %d, want %d", batch.UsageTokens, fixture.wantTokens)
			}
		})
	}
}

func TestClientRealOllamaOptIn(t *testing.T) {
	baseURL := os.Getenv("GOCONTEXT_TEST_OLLAMA_BASE_URL")
	model := os.Getenv("GOCONTEXT_TEST_OLLAMA_MODEL")
	if baseURL == "" && model == "" {
		t.Skip("set GOCONTEXT_TEST_OLLAMA_BASE_URL and GOCONTEXT_TEST_OLLAMA_MODEL to run")
	}
	if baseURL == "" || model == "" {
		t.Fatal("both Ollama opt-in environment variables are required")
	}
	client, err := New(Config{BaseURL: baseURL, Model: model, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if client.endpoint.Scheme != "http" {
		t.Fatal("real Ollama integration requires a cleartext loopback endpoint")
	}
	peerIP := net.ParseIP(client.endpoint.Hostname())
	if peerIP == nil || !peerIP.IsLoopback() {
		t.Fatal("real Ollama integration requires a literal loopback IP")
	}

	batch, err := client.Embed(context.Background(), embedding.PurposeDocument, []string{"synthetic alpha beta gamma"})
	if err != nil {
		t.Fatalf("Embed() error = %v", err)
	}
	if err := embedding.ValidateBatch(batch, 1); err != nil {
		t.Fatalf("ValidateBatch() error = %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

type temporaryTransportError struct{}

func (temporaryTransportError) Error() string   { return "temporary-transport-canary" }
func (temporaryTransportError) Timeout() bool   { return false }
func (temporaryTransportError) Temporary() bool { return true }

func statusTransport(status int, body string) roundTripFunc {
	return func(*http.Request) (*http.Response, error) {
		return responseWithStatus(status, body), nil
	}
}

func responseWithStatus(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func embeddingSuccessResponse() *http.Response {
	return responseWithStatus(http.StatusOK, `{"data":[{"index":0,"embedding":[1]}],"usage":{"total_tokens":7}}`)
}

func newRetryClient(t *testing.T) *Client {
	t.Helper()
	client, err := New(Config{
		BaseURL:    "https://api.example.com/v1",
		Model:      "fixture-model",
		APIKey:     "credential-canary",
		Dimensions: 1,
		MaxRetries: 99,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return client
}

type remoteAddrConn struct {
	net.Conn
	addr net.Addr
}

func (connection *remoteAddrConn) RemoteAddr() net.Addr {
	return connection.addr
}

func newFixtureClient(t *testing.T, serverURL string, dimensions int) *Client {
	t.Helper()
	client, err := New(Config{
		BaseURL:    serverURL + "/v1",
		Model:      "fixture-model",
		Dimensions: dimensions,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return client
}

var _ embedding.Embedder = (*Client)(nil)
