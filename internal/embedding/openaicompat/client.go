package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ToledoVitor/GoContext/internal/embedding"
)

const maxResponseBytes = 16 << 20

type embeddingRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	EncodingFormat string   `json:"encoding_format"`
	Dimensions     int      `json:"dimensions,omitempty"`
}

type embeddingResponse struct {
	Data  []embeddingResponseItem `json:"data"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

type embeddingResponseItem struct {
	Embedding embedding.Vector
	Index     int
}

func (item *embeddingResponseItem) UnmarshalJSON(data []byte) error {
	var wire struct {
		Embedding json.RawMessage `json:"embedding"`
		Index     *int            `json:"index"`
	}
	if err := json.Unmarshal(data, &wire); err != nil || wire.Index == nil || len(wire.Embedding) == 0 || bytes.Equal(bytes.TrimSpace(wire.Embedding), []byte("null")) {
		return errors.New("invalid embedding response item")
	}

	var components []json.RawMessage
	if err := json.Unmarshal(wire.Embedding, &components); err != nil {
		return errors.New("invalid embedding response item")
	}
	vector := make(embedding.Vector, len(components))
	for index, component := range components {
		if bytes.Equal(bytes.TrimSpace(component), []byte("null")) {
			return errors.New("invalid embedding response item")
		}
		if err := json.Unmarshal(component, &vector[index]); err != nil {
			return errors.New("invalid embedding response item")
		}
	}

	item.Index = *wire.Index
	item.Embedding = vector
	return nil
}

type encodedBatch struct {
	payload []byte
	count   int
}

type completedBatch struct {
	vectors     []embedding.Vector
	dimensions  int
	requests    int
	usageTokens int
}

// Embed produces one vector per input while preserving input order.
func (client *Client) Embed(ctx context.Context, _ embedding.Purpose, texts []string) (embedding.Batch, error) {
	if err := ctx.Err(); err != nil {
		return embedding.Batch{}, err
	}
	operationContext, cancel := context.WithTimeout(ctx, client.config.Timeout)
	defer cancel()

	batches, err := client.encodeBatches(texts)
	if err != nil {
		return embedding.Batch{}, err
	}

	vectors := make([]embedding.Vector, len(texts))
	completed, attemptedRequests, err := client.performBatches(operationContext, batches)
	if err != nil {
		if ctx.Err() != nil {
			return embedding.Batch{}, ctx.Err()
		}
		if errors.Is(operationContext.Err(), context.DeadlineExceeded) {
			return embedding.Batch{}, embedding.NewSemanticUnavailable(attemptedRequests)
		}
		if errors.Is(err, embedding.ErrSemanticUnavailable) {
			return embedding.Batch{}, embedding.NewSemanticUnavailable(attemptedRequests)
		}
		return embedding.Batch{}, err
	}

	dimensions := 0
	requests := 0
	usageTokens := 0
	offset := 0
	for index, result := range completed {
		if dimensions == 0 {
			dimensions = result.dimensions
		} else if result.dimensions != dimensions {
			return embedding.Batch{}, embedding.ErrInvalidBatch
		}
		copy(vectors[offset:], result.vectors)
		offset += batches[index].count
		requests += result.requests
		usageTokens += result.usageTokens
	}
	batch := embedding.Batch{
		Profile:     client.profile,
		Dimensions:  dimensions,
		Vectors:     vectors,
		Requests:    requests,
		UsageTokens: usageTokens,
	}
	if err := embedding.ValidateBatch(batch, len(texts)); err != nil {
		return embedding.Batch{}, err
	}
	return batch, nil
}

func (client *Client) performBatches(parent context.Context, batches []encodedBatch) ([]completedBatch, int, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	results := make([]completedBatch, len(batches))
	type batchJob struct {
		index   int
		encoded encodedBatch
	}
	jobs := make(chan batchJob, len(batches))
	for index, encoded := range batches {
		jobs <- batchJob{index: index, encoded: encoded}
	}
	close(jobs)

	var waitGroup sync.WaitGroup
	var firstError error
	var firstErrorOnce sync.Once
	recordError := func(err error) {
		firstErrorOnce.Do(func() {
			firstError = err
			cancel()
		})
	}
	workerCount := client.config.MaxInFlight
	if workerCount > len(batches) {
		workerCount = len(batches)
	}
	for range workerCount {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for {
				if err := ctx.Err(); err != nil {
					recordError(err)
					return
				}
				select {
				case <-ctx.Done():
					recordError(ctx.Err())
					return
				case job, ok := <-jobs:
					if !ok {
						return
					}
					wire, attempts, err := client.requestWithRetry(ctx, job.encoded.payload)
					results[job.index].requests = attempts
					if err != nil {
						recordError(err)
						return
					}
					vectors, dimensions, err := client.decodeVectors(wire, job.encoded.count)
					if err != nil {
						recordError(err)
						return
					}
					results[job.index] = completedBatch{
						vectors:     vectors,
						dimensions:  dimensions,
						requests:    attempts,
						usageTokens: wire.Usage.TotalTokens,
					}
				}
			}
		}()
	}

	waitGroup.Wait()
	attemptedRequests := 0
	for _, result := range results {
		attemptedRequests += result.requests
	}
	if firstError != nil {
		return nil, attemptedRequests, firstError
	}
	return results, attemptedRequests, nil
}

func (client *Client) encodeBatches(texts []string) ([]encodedBatch, error) {
	if len(texts) == 0 {
		return nil, embedding.ErrInvalidBatch
	}

	batchCapacity := len(texts) / client.config.BatchSize
	if len(texts)%client.config.BatchSize != 0 {
		batchCapacity++
	}
	batches := make([]encodedBatch, 0, batchCapacity)
	for start := 0; start < len(texts); {
		limit := len(texts)
		if remaining := len(texts) - start; client.config.BatchSize < remaining {
			limit = start + client.config.BatchSize
		}

		selectedEnd := start
		var selectedPayload []byte
		for end := start + 1; end <= limit; end++ {
			payload, err := json.Marshal(embeddingRequest{
				Model:          client.config.Model,
				Input:          texts[start:end],
				EncodingFormat: "float",
				Dimensions:     client.config.Dimensions,
			})
			if err != nil {
				return nil, errors.New("cannot encode embedding request")
			}
			if len(payload) > client.config.MaxBatchBytes {
				break
			}
			selectedEnd = end
			selectedPayload = payload
			if end == limit {
				break
			}
		}
		if selectedEnd == start {
			return nil, embedding.ErrInvalidBatch
		}
		batches = append(batches, encodedBatch{payload: selectedPayload, count: selectedEnd - start})
		start = selectedEnd
	}
	return batches, nil
}

func (client *Client) decodeVectors(wire embeddingResponse, expected int) ([]embedding.Vector, int, error) {
	if len(wire.Data) != expected {
		return nil, 0, embedding.ErrInvalidBatch
	}

	vectors := make([]embedding.Vector, expected)
	for _, item := range wire.Data {
		if item.Index < 0 || item.Index >= len(vectors) || vectors[item.Index] != nil {
			return nil, 0, embedding.ErrInvalidBatch
		}
		vectors[item.Index] = item.Embedding
	}

	dimensions := 0
	for _, vector := range vectors {
		if len(vector) == 0 || (client.config.Dimensions > 0 && len(vector) != client.config.Dimensions) {
			return nil, 0, embedding.ErrInvalidBatch
		}
		if dimensions == 0 {
			dimensions = len(vector)
		} else if len(vector) != dimensions {
			return nil, 0, embedding.ErrInvalidBatch
		}
		if err := normalizeVector(vector); err != nil {
			return nil, 0, err
		}
	}
	return vectors, dimensions, nil
}

func normalizeVector(vector embedding.Vector) error {
	maximum := 0.0
	for _, value := range vector {
		component := float64(value)
		if math.IsNaN(component) || math.IsInf(component, 0) {
			return embedding.ErrInvalidVector
		}
		if absolute := math.Abs(component); absolute > maximum {
			maximum = absolute
		}
	}
	if maximum == 0 {
		return embedding.ErrInvalidVector
	}

	scaledSquares := 0.0
	for _, value := range vector {
		scaled := float64(value) / maximum
		scaledSquares += scaled * scaled
	}
	norm := maximum * math.Sqrt(scaledSquares)
	for index, value := range vector {
		vector[index] = float32(float64(value) / norm)
	}
	return nil
}

func (client *Client) requestWithRetry(ctx context.Context, payload []byte) (embeddingResponse, int, error) {
	retries := client.config.MaxRetries
	if retries > 2 {
		retries = 2
	}
	limit := retries + 1

	for attempt := 1; attempt <= limit; attempt++ {
		wire, temporary, retryAfter, err := client.requestOnce(ctx, payload)
		if err == nil {
			return wire, attempt, nil
		}
		if ctx.Err() != nil {
			return embeddingResponse{}, attempt, ctx.Err()
		}
		if !temporary {
			return embeddingResponse{}, attempt, err
		}
		if attempt == limit {
			return embeddingResponse{}, attempt, embedding.ErrSemanticUnavailable
		}

		wait := retryDelay(attempt)
		if parsed, ok := client.parseRetryAfter(retryAfter); ok {
			wait = parsed
		}
		if deadline, ok := ctx.Deadline(); ok && client.now().Add(wait).After(deadline) {
			return embeddingResponse{}, attempt, embedding.ErrSemanticUnavailable
		}
		if err := client.sleep(ctx, wait); err != nil {
			if ctx.Err() != nil {
				return embeddingResponse{}, attempt, ctx.Err()
			}
			return embeddingResponse{}, attempt, embedding.ErrSemanticUnavailable
		}
	}
	return embeddingResponse{}, limit, embedding.ErrSemanticUnavailable
}

func (client *Client) requestOnce(ctx context.Context, payload []byte) (embeddingResponse, bool, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return embeddingResponse{}, false, "", errors.New("cannot create embedding request")
	}
	request.Header.Set("Content-Type", "application/json")
	if client.config.APIKey != "" {
		request.Header.Set("Authorization", "Bearer "+client.config.APIKey)
	}

	response, err := client.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return embeddingResponse{}, false, "", ctx.Err()
		}
		if isTemporaryConnectionError(err) {
			return embeddingResponse{}, true, "", errors.New("temporary embedding connection failure")
		}
		return embeddingResponse{}, false, "", errors.New("embedding request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		temporary := response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests || (response.StatusCode >= 500 && response.StatusCode <= 599)
		return embeddingResponse{}, temporary, response.Header.Get("Retry-After"), fmt.Errorf("embedding request failed with HTTP status %d", response.StatusCode)
	}

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return embeddingResponse{}, false, "", errors.New("cannot read embedding response")
	}
	if len(responseBody) > maxResponseBytes {
		return embeddingResponse{}, false, "", errors.New("embedding response exceeds size limit")
	}

	var wire embeddingResponse
	if err := json.Unmarshal(responseBody, &wire); err != nil {
		return embeddingResponse{}, false, "", fmt.Errorf("%w: cannot decode embedding response", embedding.ErrInvalidBatch)
	}
	return wire, false, "", nil
}

func isTemporaryConnectionError(err error) bool {
	var networkError net.Error
	return errors.As(err, &networkError) && (networkError.Timeout() || networkError.Temporary())
}

func retryDelay(attempt int) time.Duration {
	return time.Duration(1<<(attempt-1)) * 100 * time.Millisecond
}

func (client *Client) parseRetryAfter(value string) (time.Duration, bool) {
	if seconds, err := strconv.ParseUint(value, 10, 31); err == nil {
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	wait := when.Sub(client.now())
	if wait < 0 {
		wait = 0
	}
	return wait, true
}
