package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/source"
)

type failingCorpusReader struct {
	loadErr  error
	closeErr error
}

func (r failingCorpusReader) Load(context.Context, string) ([]source.Chunk, error) {
	if r.loadErr != nil {
		return nil, r.loadErr
	}
	return []source.Chunk{{ID: "chunk"}}, nil
}

func (r failingCorpusReader) Close() error {
	return r.closeErr
}

func TestLoadAndClosePropagatesReaderCloseError(t *testing.T) {
	closeErr := errors.New("close failure")
	chunks, err := loadAndClose(context.Background(), "repository", failingCorpusReader{closeErr: closeErr})
	if chunks != nil {
		t.Fatalf("loadAndClose() chunks = %#v, want nil after close failure", chunks)
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("loadAndClose() error = %v, want close failure", err)
	}
}

func TestLoadAndCloseJoinsLoadAndReaderCloseErrors(t *testing.T) {
	loadErr := errors.New("load failure")
	closeErr := errors.New("close failure")
	chunks, err := loadAndClose(context.Background(), "repository", failingCorpusReader{loadErr: loadErr, closeErr: closeErr})
	if chunks != nil {
		t.Fatalf("loadAndClose() chunks = %#v, want nil", chunks)
	}
	if !errors.Is(err, loadErr) || !errors.Is(err, closeErr) {
		t.Fatalf("loadAndClose() error = %v, want joined load and close failures", err)
	}
}
