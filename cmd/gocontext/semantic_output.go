package main

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/ToledoVitor/GoContext/internal/search/hybrid"
)

const exactSearchWarningThreshold = 20_000

const (
	semanticDegradedWarning     = "aviso: semântica indisponível; usando busca lexical\n"
	externalIndexEgressWarning  = "aviso: indexação semântica externa pode enviar fonte permitida para fora da máquina\n"
	externalSearchEgressWarning = "aviso: busca semântica externa pode enviar a consulta para fora da máquina\n"
	exactSearchScaleWarning     = "aviso: busca exata SQLite acima de 20000 chunks pode ser lenta\n"
)

type cliHybridObserver struct {
	writer io.Writer
	once   sync.Once
}

func (observer *cliHybridObserver) Observe(_ context.Context, event hybrid.Event) {
	if observer == nil || observer.writer == nil || event.Kind != "fallback" {
		return
	}
	observer.once.Do(func() {
		_, _ = fmt.Fprint(observer.writer, semanticDegradedWarning)
	})
}
