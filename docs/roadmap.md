# Roadmap curto

Cada marco deve terminar compilável, testado e utilizável sem depender do seguinte.

## M0 — Fundação (entregue)

- módulo Go, CLI mínima e contratos centrais;
- modelo de proveniência e citações;
- arquitetura, decisão de stack e limites do MVP.

**Pronto quando:** `go test ./...`, `go vet ./...` e build da CLI passam sem dependências externas.

## M1 — Ingestão observável (parcial)

- scanner default-deny com decisão antes de leitura, exclusões auditáveis e fail-closed;
- testes de segredos, `.git/**`, `.github/**`, symlink, traversal, nested repo, binário, gerado e arquivo grande;
- spike e escolha do binding Tree-sitter;
- line parsing conservador de JavaScript/JSX, Python e TypeScript;
- chunks por símbolo;
- comando `gocontext inspect <repo>` que mostra metadados, sem embeddings.

**Pronto quando:** fixtures retornam símbolos, linhas e chunks determinísticos; escapes por symlink falham; bytes hard-denied não chegam a nenhum estágio ou log.

Scanner `scanner-v6`, relatório, snapshots seguros, tracer bullet JavaScript/JSX, chunking preliminar e prova taint ponta a ponta estão entregues. `scanner-v5` exige reindex explícito, sem migração in-place. Parser estrutural continua pendente; o line parser atual não é apresentado como parser final.

## M2 — Índice e busca local (núcleo entregue; validação ampla pendente)

- gate de compatibilidade do driver SQLite puro Go;
- gerações SQLite atômicas por repositório e reindexação completa;
- adapter HTTP OpenAI-compatible configurável para OpenAI ou Ollama;
- busca lexical primeira classe, busca vetorial exata e fusão RRF;
- modos semânticos `off`, `preferred` e `required`, com fallback lexical observável;
- comando `gocontext search` com hits citados.
- snapshot/semantic-off permanece default offline; SQLite, `auto` e semântica são opt-in;
- rollback explícito de snapshot exige par atual validado e é fail-closed/indisponível no Windows M2 até existir gate runtime de DACL owner-only; snapshot padrão e reindexação permanecem a recuperação suportada;
- schema SQLite v2 liga corpus, perfil e bytes vetoriais por digests; caches v1 exigem reindexação sem migração in-place;
- cosseno exato em Go avisa acima de 20.000 chunks sem trocar ranking ou backend.

**Pronto quando:** consultas exatas e conceituais encontram fixtures sem serviço de índice externo; modo default não abre rede; falha temporária semântica preserva busca lexical e referências canônicas.

Esses comportamentos do núcleo estão entregues com `source.Reference` canônico e composition root no CLI. Task 13 concluiu prova taint e Task 14 concluiu primeiro inventário/baseline lexical profissional aggregate-only. O harness aceita agora um gold set humano local opcional, estrito e owner-only, resolve julgamentos apenas contra chunks canônicos permitidos e emite somente agregados schema 2. O conteúdo humano ainda não foi criado nem executado; portanto a promoção ampla continua bloqueada e as categorias conceituais, framework, erro, configuração, cross-layer e evidência negativa dos baselines publicados permanecem `not-evaluated`.

## M3 — Perguntas fundamentadas

- adapter LLM explícito e provider fake para testes; Anthropic pode ser adapter futuro de geração;
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

React/TypeScript para navegação e histórico, atualização incremental, ANN, reranker, banco vetorial externo, mais linguagens, métricas de recuperação e adapters adicionais. Só entram após uso real indicar prioridade.

Primeiro ciclo de uso real em três repositórios profissionais autorizados passou os gates da Task 14 e concluiu inventário/baseline lexical aggregate-only sob `scanner-v5`, sem rede. O tracer bullet JavaScript/JSX posterior usa somente fixtures sintéticas: não atualiza aquele baseline nem sustenta delta de qualidade profissional. Nova medição exige reindex `scanner-v6` e repetição dos gates de taint, no-egress e go/no-go. Categorias não exact-symbol continuam `not-evaluated`; híbrido via Ollama loopback permanece futuro e condicionado ao gate local. Código, paths e queries profissionais não entram em relatórios nem providers externos.
