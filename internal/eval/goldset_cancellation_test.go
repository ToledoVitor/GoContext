package eval_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	evaluation "github.com/ToledoVitor/GoContext/internal/eval"
	"github.com/ToledoVitor/GoContext/internal/ingest"
)

type cancelOnErrCheckContext struct {
	calls    int
	cancelAt int
	done     chan struct{}
	canceled bool
}

func (*cancelOnErrCheckContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (ctx *cancelOnErrCheckContext) Done() <-chan struct{}   { return ctx.done }
func (*cancelOnErrCheckContext) Value(any) any               { return nil }

func (ctx *cancelOnErrCheckContext) Err() error {
	ctx.calls++
	if !ctx.canceled && ctx.calls >= ctx.cancelAt {
		ctx.canceled = true
		close(ctx.done)
	}
	if ctx.canceled {
		return context.Canceled
	}
	return nil
}

func TestParseGoldSetSamplesCancellationDuringUniqueKeyWalk(t *testing.T) {
	payload := []byte(`[` + strings.Repeat(`0,`, 256) + `0]`)
	ctx := newCancelOnErrCheckContext(3)
	goldSet, err := evaluation.ParseGoldSet(ctx, payload, "repo-ab", ingest.ScanPolicyVersion)
	if goldSet != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("gold set/error/checks = %#v/%v/%d", goldSet, err, ctx.calls)
	}
}

func TestParseGoldSetCancellationWinsAroundTypedDecodeAndSchemaFailure(t *testing.T) {
	for _, test := range []struct {
		name     string
		cancelAt int
	}{
		{name: "after decode", cancelAt: 7},
		{name: "before schema error", cancelAt: 9},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := newCancelOnErrCheckContext(test.cancelAt)
			goldSet, err := evaluation.ParseGoldSet(ctx, []byte(`{}`), "repo-ab", ingest.ScanPolicyVersion)
			if goldSet != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("gold set/error/checks = %#v/%v/%d", goldSet, err, ctx.calls)
			}
		})
	}
}

func newCancelOnErrCheckContext(cancelAt int) *cancelOnErrCheckContext {
	return &cancelOnErrCheckContext{cancelAt: cancelAt, done: make(chan struct{})}
}
