# Roadmap curto

Cada marco deve terminar compilável, testado e utilizável sem depender do seguinte.

## M0 — Fundação (agora)

- módulo Go, CLI mínima e contratos centrais;
- modelo de proveniência e citações;
- arquitetura, decisão de stack e limites do MVP.

**Pronto quando:** `go test ./...`, `go vet ./...` e build da CLI passam sem dependências externas.

## M1 — Ingestão observável

- scanner seguro com regras de exclusão e limites;
- spike e escolha do binding Tree-sitter;
- parsing de Python e TypeScript;
- chunks por símbolo;
- comando `gocontext inspect <repo>` que mostra metadados, sem embeddings.

**Pronto quando:** fixtures retornam símbolos, linhas e chunks determinísticos; escapes por symlink falham.

## M2 — Índice e busca local

- spike de SQLite textual e opção vetorial local;
- snapshot por repositório e reindexação completa;
- adapter de embeddings configurável;
- busca lexical, vetorial e fusão RRF;
- comando `gocontext search` com hits citados.

**Pronto quando:** consultas exatas e conceituais encontram fixtures sem serviço de índice externo.

## M3 — Perguntas fundamentadas

- adapter LLM explícito e provider fake para testes;
- montagem de contexto com orçamento;
- validação determinística de citações;
- recusa por evidência insuficiente;
- comando `gocontext ask`.

**Pronto quando:** teste end-to-end responde usando apenas chunks fornecidos e rejeita citação inventada.

## M4 — MCP somente leitura

- tools para busca, leitura de símbolo e pergunta;
- raiz de repositório fixa por sessão;
- nenhum recurso de escrita, shell ou Git mutável.

**Pronto quando:** cliente MCP consulta índice sem conseguir acessar caminho externo ou modificar arquivos.

## Depois do MVP

React/TypeScript para navegação e histórico, atualização incremental, mais linguagens, métricas de recuperação e adapters adicionais. Só entram após uso real indicar prioridade.
