# GoContext

GoContext é um copiloto local de inteligência para repositórios. A proposta é transformar código-fonte em contexto pesquisável, responder perguntas com evidências verificáveis e expor esse contexto por uma interface MCP somente leitura.

O núcleo de recuperação do **M2** está implementado: snapshot JSON e busca lexical continuam sendo o caminho padrão offline; SQLite, embeddings OpenAI-compatible, busca vetorial exata e fusão híbrida são capacidades opt-in. A prova taint ponta a ponta, o primeiro baseline profissional agregado e um tracer bullet sintético de JavaScript/JSX estão concluídos. Parsing estrutural, nova medição profissional após esse suporte, LLM, MCP, frontend, ANN, reranker e reuso incremental permanecem futuros.

## Objetivo do MVP

Em um fim de semana, provar um fluxo vertical pequeno:

1. abrir um repositório local autorizado;
2. encontrar arquivos JavaScript/JSX, Python e TypeScript elegíveis;
3. extrair símbolos e produzir chunks rastreáveis;
4. indexar texto e vetores localmente;
5. responder perguntas combinando busca lexical e vetorial;
6. mostrar citações com arquivo e intervalo de linhas;
7. oferecer consulta via CLI e MCP somente leitura.

Sucesso significa responder perguntas sobre um repositório pequeno com citações úteis. Não significa competir com uma IDE, hospedar múltiplos usuários ou construir uma plataforma RAG genérica.

## Princípios

- **Local primeiro:** código e índices permanecem na máquina, salvo envio explícito a um provedor de embeddings ou LLM.
- **Evidência antes de fluência:** resposta sem suporte suficiente deve dizer que não encontrou evidência.
- **Citação de ponta a ponta:** caminho e linhas nascem no scanner e acompanham cada transformação.
- **Conteúdo não confiável:** instruções encontradas dentro do repositório são dados, não comandos.
- **Somente leitura:** GoContext não altera arquivos do repositório e o servidor MCP futuro não expõe escrita nem shell.
- **Poucas peças:** monólito modular, um processo e armazenamento local no MVP.

## Estado atual

```text
cmd/gocontext          CLI de indexação, consulta e ponto de composição
internal/source        tipos centrais: arquivo, símbolo, chunk e citação
internal/ingest        contratos para scanner, parser, chunker e armazenamento
  └─ filesystem        scanner local seguro para JavaScript/JSX, Python e TypeScript
  └─ lineparser        descoberta top-level preliminar, sem AST
  └─ symbolchunker     chunks por limites de declarações top-level
  └─ localstore        snapshots JSON locais e atômicos por repositório
internal/embedding     contratos provider-agnostic e adapter HTTP OpenAI-compatible
internal/index         builder de gerações completas e store SQLite local
  └─ sqlite            gerações atômicas, chunks canônicos e cosseno exato
internal/search        contratos e resultados com source.Reference preservado
  ├─ lexical           ranking lexical determinístico de primeira classe
  ├─ vector            recuperação vetorial canônica
  └─ hybrid            fusão RRF e fallback lexical observável
internal/answer        contratos para geração fundamentada e guardrails
docs/architecture.md   arquitetura, fluxo de dados e limites de segurança
docs/decisions/        decisões técnicas registradas
docs/roadmap.md        sequência curta de entregas
```

Interfaces ficam próximas do fluxo consumidor, em vez de formar um pacote genérico de abstrações. Implementações concretas só serão adicionadas quando um marco precisar delas.

## Como verificar

Requer Go 1.24 ou superior. O gate suportado também prova o driver SQLite puro Go com CGO desabilitado.

```bash
go test ./...
go vet ./...
CGO_ENABLED=0 go test ./...
go run ./cmd/gocontext --version
```

Saída esperada da última linha:

```text
gocontext dev
```

## Indexar repositório

```bash
go run ./cmd/gocontext index /caminho/do/repositório
```

Sem flags, o comando percorre fontes JavaScript/JSX, Python e TypeScript, descobre declarações conservadoras, cria chunks e substitui somente o snapshot local; não abre SQLite nem rede. O store padrão fica no diretório de cache do sistema, fora do repositório. Para escolher outro local:

```bash
go run ./cmd/gocontext index --store /caminho/do/cache /caminho/do/repositório
```

### Policy de scanner v6 e reindexação

A policy `scanner-v6` mantém todos os hard denies e classificadores de `scanner-v5` e adiciona somente `.js` e `.jsx`, com comparação case-insensitive, à allowlist de fontes. Todos os gates de filename, diretório, nested repo, symlink, arquivo regular, tamanho, binário, UTF-8, gerado e segredo continuam anteriores à criação de `source.File`. JSON, Markdown, assets, binários e extensões arbitrárias permanecem não suportados; em particular, JSON não é habilitado cegamente porque pode misturar configuração útil com credenciais ou outros dados sensíveis.

Snapshots e corpora `scanner-v5` ou anteriores são incompatíveis e falham com pedido sanitizado de reindexação; não existe migração in-place. Reexecute `index` no backend desejado para produzir um corpus `scanner-v6`. O suporte JavaScript foi provado somente com fixtures sintéticas e não altera os resultados profissionais já registrados sob `scanner-v5`; nenhuma melhoria de qualidade profissional é alegada antes de uma nova execução local que repita os gates de taint, no-egress e go/no-go.

### Opt-in SQLite sem embeddings

A migração suportada é uma reindexação completa explícita. O schema SQLite v2 liga metadados semânticos e bytes vetoriais por digests SHA-256; caches SQLite v1 não são migrados no lugar e exigem reindexação. A operação publica a geração v2 e um snapshot companheiro do mesmo corpus, sem rede:

```bash
go run ./cmd/gocontext index --store /caminho/do/cache --index-backend sqlite --semantic off /caminho/do/repositório
go run ./cmd/gocontext search --store /caminho/do/cache --index-backend auto /caminho/do/repositório "carregar usuário"
```

`auto` também é opt-in: usa a geração SQLite ativa quando ela existe e, quando o banco/store/repositório está ausente, usa o snapshot validado. Busca nunca cria ou repara SQLite.

### Opt-in semântico local via Ollama

O mesmo adapter OpenAI-compatible aceita um endpoint de IP loopback, normalmente sem chave. Modelo não possui default:

```bash
go run ./cmd/gocontext index --store /caminho/do/cache --index-backend sqlite \
  --semantic preferred --embedding-base-url http://127.0.0.1:11434/v1 \
  --embedding-model '<modelo-local-escolhido>' /caminho/do/repositório

go run ./cmd/gocontext search --store /caminho/do/cache --index-backend auto \
  --semantic preferred --embedding-base-url http://127.0.0.1:11434/v1 \
  --embedding-model '<modelo-local-escolhido>' /caminho/do/repositório "carregar usuário"
```

### Opt-in semântico com endpoint externo

> **Aviso de egress:** indexar pode enviar fonte permitida e buscar pode enviar a consulta para fora da máquina. Use somente com autorização explícita para o repositório e o endpoint.

A chave é aceita somente por `GOCONTEXT_EMBEDDING_API_KEY`; não existe flag ou arquivo de credencial. O endpoint abaixo é deliberadamente não operacional e o modelo é placeholder:

```bash
export GOCONTEXT_EMBEDDING_API_KEY='<chave-fornecida-pelo-operador>'

go run ./cmd/gocontext index --store /caminho/do/cache --index-backend sqlite \
  --semantic required --embedding-base-url https://embeddings.example.invalid/v1 \
  --embedding-model '<modelo-escolhido>' /caminho/do/repositório
```

Configurar endpoint/modelo sem `preferred|required` não ativa rede. O CLI emite aviso fixo antes do primeiro possível egress externo.

## Consultar snapshot

Depois de indexar, consulte o snapshot lexical pelo mesmo caminho de repositório:

```bash
go run ./cmd/gocontext search /caminho/do/repositório "carregar usuário"
```

Cada resultado mostra score, citação `arquivo:linha-inicial-linha-final`, símbolo quando disponível e trecho-fonte. Limite resultados ou use um store explícito:

```bash
go run ./cmd/gocontext search --limit 5 --store /caminho/do/cache /caminho/do/repositório carregar usuário
```

Busca lexical continua primeira classe e fallback obrigatório em `preferred`. Tanto snapshot quanto SQLite preservam o `source.Reference` canônico; a camada vetorial não reconstrói citações.

## Rollback explícito para snapshot

O snapshot implícito permanece a compatibilidade padrão. Já `--index-backend snapshot` fornecido explicitamente é uma solicitação de rollback:

```bash
go run ./cmd/gocontext search --store /caminho/do/cache --index-backend snapshot /caminho/do/repositório "carregar usuário"
```

Sem banco SQLite, ou sem geração ativa para esse repositório, basta existir snapshot atual validado. Com geração ativa, o comando exige snapshot companheiro, marker regular `rollback_ready` sem symlink/reparse, policy atual, mesma revisão de corpus e mesma geração SQLite ativa; chunks, vetores e digests da geração fixada também são validados antes da autorização. Em sistemas POSIX, bits de grupo/outros tornam o marker permissivo e inválido. No Windows, rollback explícito apoiado em marker é deliberadamente indisponível no M2 e falha fechado com a mesma categoria sanitizada de reindexação: ACL herdada não prova privacidade owner-only. SQLite e o snapshot padrão continuam suportados. Suporte futuro exige criação e validação de DACL owner-only testadas em runtime Windows. Marker ausente, legado, malformado ou divergente — e SQLite corrupto/inacessível — também falham fechados.

Em POSIX, recupere um par saudável reindexando primeiro com `--index-backend sqlite --semantic off` e só então solicite o rollback explícito. No Windows, omita `--index-backend snapshot` para usar o snapshot padrão ou reindexe no backend pretendido; o marker explícito continuará bloqueado nesta versão. Se SQLite estiver corrupto, uma reindexação snapshot padrão restaura o caminho implícito atual, mas não torna o rollback explícito pronto; o store SQLite precisa de recuperação administrativa separada. Não exclua SQLite apenas para contornar o guard.

Uma reindexação snapshot padrão depois de SQLite invalida `rollback_ready` antes de substituir o snapshot e continua autoritativa para a busca padrão. Se a invalidação falhar, o CLI reporta falha sanitizada e não anuncia nem grava o novo snapshot. Promover SQLite/`auto` a default exige uma decisão futura.

## Escala da busca exata

A busca vetorial SQLite faz cosseno exato em Go. Acima de 20.000 chunks, cada busca SQLite emite no máximo um aviso fixo; 20.000 não avisa e 20.001 avisa. O aviso não muda ranking, não seleciona ANN e não afeta o caminho snapshot padrão. O benchmark manual reproduzível fica em `internal/index/sqlite/benchmark_test.go`; resultados são evidência local não bloqueante, não um SLA.

## Arquitetura atual e futura

```text
repositório local
      │
      ▼
scanner → parser → chunks por símbolo → índice local
                                      ├─ lexical
                                      └─ vetorial
                                            │
pergunta → busca híbrida → contexto citado → guardrails → resposta
                                                    │
                                             CLI / MCP read-only
```

Detalhes e decisões: [arquitetura](docs/architecture.md), [stack](docs/decisions/0001-stack.md), [embeddings e busca vetorial](docs/decisions/0002-embeddings-vector-search.md), [plano de implementação](docs/plans/2026-08-27-provider-agnostic-embeddings-vector-search.md), [validação profissional local](docs/plans/2026-08-27-tivita-professional-repository-validation.md) e [roadmap](docs/roadmap.md).

## Fora do escopo do MVP

- frontend React/TypeScript;
- edição ou geração automática de código;
- execução de comandos sugeridos pelo repositório;
- sincronização com GitHub, GitLab ou serviços SaaS;
- autenticação, times e multi-tenancy;
- agentes autônomos;
- suporte amplo a linguagens além do recorte atual de JavaScript/JSX, Python e TypeScript.

## Licença

Ainda não definida. Não assuma permissão de redistribuição até uma licença ser adicionada.
