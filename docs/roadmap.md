# Roadmap curto

Cada marco deve terminar compilável, testado e utilizável sem depender do seguinte.

## M0 — Fundação (agora)

- módulo Go, CLI mínima e contratos centrais;
- modelo de proveniência e citações;
- arquitetura, decisão de stack e limites do MVP.

**Pronto quando:** `go test ./...`, `go vet ./...` e build da CLI passam sem dependências externas.

## M1 — Ingestão observável

- scanner default-deny com decisão antes de leitura, exclusões auditáveis e fail-closed;
- testes de segredos, `.git/**`, `.github/**`, symlink, traversal, nested repo, binário, gerado e arquivo grande;
- spike e escolha do binding Tree-sitter;
- parsing de Python e TypeScript;
- chunks por símbolo;
- comando `gocontext inspect <repo>` que mostra metadados, sem embeddings.

**Pronto quando:** fixtures retornam símbolos, linhas e chunks determinísticos; escapes por symlink falham; bytes hard-denied não chegam a nenhum estágio ou log.

## M2 — Índice e busca local

- gate de compatibilidade do driver SQLite puro Go;
- gerações SQLite atômicas por repositório e reindexação completa;
- adapter HTTP OpenAI-compatible configurável para OpenAI ou Ollama;
- busca lexical primeira classe, busca vetorial exata e fusão RRF;
- modos semânticos `off`, `preferred` e `required`, com fallback lexical observável;
- comando `gocontext search` com hits citados.

**Pronto quando:** consultas exatas e conceituais encontram fixtures sem serviço de índice externo; modo default não abre rede; falha temporária semântica preserva busca lexical e referências canônicas.

## M3 — Perguntas fundamentadas

- adapter LLM explícito e provider fake para testes; Anthropic pode ser adapter futuro de geração, não embeddings;
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

Primeiro ciclo de uso real avalia Taba App, Tivita Backend e Tivita Web App localmente, após gates de segurança. Inventário descobre stacks e gaps; benchmark compara lexical/offline com híbrido via Ollama loopback. Código, paths e queries profissionais não entram em relatórios nem providers externos. Suporte novo nasce de gaps priorizados e fixtures não proprietárias.
