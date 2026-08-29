# Embeddings e busca vetorial provider-agnostic — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Entregar indexação semântica opt-in e busca híbrida local, com adapter HTTP OpenAI-compatible configurável para OpenAI/Ollama, fallback lexical e citações preservadas.

**Architecture:** `internal/index.Builder` publica uma geração completa de chunks e vetores em SQLite. `internal/search/lexical`, `internal/search/vector` e `internal/search/hybrid` implementam `search.Searcher`; RRF e degradação ficam escondidos. `cmd/gocontext` permanece composition root e nunca cria chamada de rede sem configuração explícita.

**Tech Stack:** Go 1.24+, biblioteca padrão HTTP/JSON/crypto, `database/sql`, driver SQLite puro Go validado e fixado em `go.mod`, `httptest`, SQLite WAL, cosseno exato e RRF.

**Spec:** `docs/decisions/0002-embeddings-vector-search.md`; validation profissional: `docs/plans/2026-08-27-tivita-professional-repository-validation.md`

## Global Constraints

- Nenhum provider ou código de produto é implementado durante a fase de documentação que criou este plano.
- Primeiro adapter concreto usa protocolo OpenAI-compatible; OpenAI e Ollama são configurações do mesmo adapter.
- Não criar adapter de embeddings Anthropic. Anthropic pertence somente ao futuro seam `answer.Generator`; Voyage AI é provider separado.
- Nenhuma chamada de rede por default. Modo semântico sem configuração é `off`.
- Busca lexical permanece primeira classe, testável isoladamente e fallback obrigatório em modo `preferred`.
- `source.Chunk` e `source.Reference` são origem canônica das citações; índice vetorial nunca reconstrói referência.
- Reindexação completa é comportamento M2. Reuso incremental exige mesmo chunk ID e fingerprint, mas não será implementado neste plano.
- SQLite e vetores ficam locais; banco vetorial externo, ANN e extensão SQLite vetorial ficam fora de escopo.
- API key vem somente de `GOCONTEXT_EMBEDDING_API_KEY`; nunca de flag, arquivo, log ou mensagem de erro.
- Scanner aplica hard deny antes de abrir arquivo. No mínimo `.env`, `.env.*`, `.git/**` e `.github/**` nunca chegam a parser, chunker, embedder, store ou log.
- Hard deny cobre credenciais/chaves/certificados, metadata/automação, dependências, builds, caches, gerados, nested repos, symlinks, binários e arquivos grandes. M2 não oferece override de inclusão.
- Diagnósticos de exclusão são agregados e redigidos; citações solicitadas pelo usuário continuam mostrando `source.Reference` de conteúdo permitido.
- Repositórios profissionais ficam locais por default. Avaliação externa exige opt-in separado, disclosure de egress e autorização por repositório.
- Nenhum conteúdo, path, query ou trecho dos repositórios Taba App, Tivita Backend e Tivita Web App entra em docs/reports; somente métricas agregadas e achados sanitizados.
- Mudança de fingerprint/modelo/dimensão exige nova geração completa. Nunca truncar ou preencher vetor.
- `go test ./...`, `go vet ./...` e testes sem rede real devem passar ao fim de cada tarefa.

---

## Sequência e gates

```text
0 scanner hard-deny ─────────────────────────────┐
                                                 v
1 contratos embedding
  └─> 2 config OpenAI-compatible
       └─> 3 cliente HTTP

4 filtros lexicais ──────────────────────────────┐
5 SQLite/corpus ─> 6 vetores exatos ─> 7 builder├─> 9 híbrido ─> 10 config CLI
                                  └─> 8 vector ──┘               ├─> 11 rollout CLI
                                                                 └─> 12 E2E/docs
                                                                       └─> 13 taint/security
                                                                            └─> 14 validação Tivita
```

Cada tarefa termina em artefato testável e commit independente. Não iniciar tarefa dependente antes de gate anterior passar.

## Estrutura de arquivos planejada

| Arquivo | Responsabilidade |
| --- | --- |
| `internal/ingest/filesystem/policy.go` | hard deny por path/tipo antes de qualquer `Open` |
| `internal/ingest/filesystem/report.go` | contagens sanitizadas por motivo, sem paths/conteúdo |
| `internal/embedding/contracts.go` | tipos provider-agnostic, fingerprint, vetores, validação de batch |
| `internal/embedding/openaicompat/config.go` | validação de URL/modelo/limites e fingerprint sem segredos |
| `internal/embedding/openaicompat/client.go` | `/v1/embeddings`, auth, batching, retry e validação de resposta |
| `internal/index/contracts.go` | geração completa, modo semântico, store e relatório de indexação |
| `internal/index/builder.go` | revisão de corpus, batches, política off/preferred/required e publicação |
| `internal/index/sqlite/schema.go` | schema versionado e migrações forward-only do banco v2 |
| `internal/index/sqlite/store.go` | publicação transacional, manifest ativo e `Load` de chunks |
| `internal/index/sqlite/vector.go` | encoding float32, validação e busca exata por cosseno |
| `internal/search/contracts.go` | filtro aditivo e contratos existentes |
| `internal/search/lexical/searcher.go` | aplicação idêntica de filtros antes de scoring/limit |
| `internal/search/vector/searcher.go` | preflight de perfil, embedding de query e candidatos canônicos |
| `internal/search/hybrid/searcher.go` | concorrência, RRF, fallback e eventos sanitizados |
| `cmd/gocontext/embedding_config.go` | flags/env não secretos, modos e criação de adapters |
| `cmd/gocontext/index.go` | composição do builder e saída de relatório |
| `cmd/gocontext/search.go` | escolha snapshot/SQLite, composição lexical/híbrida e avisos |

Testes ficam ao lado de cada arquivo, seguindo padrão atual `package ..._test` quando testam interface pública do pacote.

## Contratos-alvo

Assinaturas abaixo são contrato entre tarefas. Mudança exige atualizar tarefas consumidoras e ADR antes de implementação.

```go
// internal/embedding/contracts.go
type Purpose string

const (
	PurposeDocument Purpose = "document"
	PurposeQuery    Purpose = "query"
)

type Profile struct {
	Fingerprint string
	Model       string
}

type Vector []float32

type Batch struct {
	Profile     Profile
	Dimensions  int
	Vectors     []Vector
	Requests    int
	UsageTokens int
}

type Embedder interface {
	Profile() Profile
	Embed(context.Context, Purpose, []string) (Batch, error)
}
```

```go
// internal/index/contracts.go
type SemanticMode string

const (
	SemanticOff       SemanticMode = "off"
	SemanticPreferred SemanticMode = "preferred"
	SemanticRequired  SemanticMode = "required"
)

type VectorRecord struct {
	ChunkID string
	Values  embedding.Vector
}

type Generation struct {
	RepositoryID  string
	ID            string
	BaseGeneration string
	CorpusRevision string
	ScanPolicyVersion string
	Chunks         []source.Chunk
	Profile        *embedding.Profile
	Dimensions     int
	Vectors        []VectorRecord
}

type Store interface {
	ActiveGeneration(context.Context, string) (string, error)
	Replace(context.Context, Generation) error
}

type Builder struct { /* dependencies and policy hidden */ }

func NewBuilder(BuilderConfig, embedding.Embedder, Store) (*Builder, error)
func (b *Builder) Replace(context.Context, string, source.Corpus) (Report, error)
```

```go
// internal/search/contracts.go
type Filter struct {
	PathPrefixes []string
	Languages    []source.Language
}

type Query struct {
	RepositoryID string
	Text         string
	Limit        int
	Filter       Filter
}
```

## Phase 0 — Gate de segurança do scanner

### Task 0: Política default-deny, auditável e fail-closed

**Files:**

- Modify: `internal/ingest/contracts.go`
- Create: `internal/source/corpus.go`
- Test: `internal/source/corpus_test.go`
- Create: `internal/ingest/filesystem/policy.go`
- Create: `internal/ingest/filesystem/report.go`
- Modify: `internal/ingest/filesystem/scanner.go`
- Test: `internal/ingest/filesystem/policy_test.go`
- Test: `internal/ingest/filesystem/scanner_test.go`
- Modify: `internal/ingest/localstore/store.go`
- Test: `internal/ingest/localstore/store_test.go`
- Modify: `cmd/gocontext/index.go`
- Test: `cmd/gocontext/index_test.go`

**Interfaces:**

- Produces: `ingest.ScanResult{Files, Report}`, `ingest.ScanReport`, `ExclusionReason`
- Consumes: somente raiz autorizada e policy built-in; nenhuma regra do repositório é aberta para decidir hard deny

```go
type ExclusionReason string

type ScanReport struct {
	EligibleFiles int
	EligibleBytes int64
	IncludedFiles int
	IncludedBytes int64
	Excluded      map[ExclusionReason]int
	IncludedByLanguage map[source.Language]int
	SizeBands map[string]int
	UnsupportedByExtension map[string]int
}

type ScanResult struct {
	PolicyVersion string
	Files         []source.File
	Report        ScanReport
}

const ScanPolicyVersion = "scanner-v6"

// internal/source/corpus.go
type Corpus struct {
	PolicyVersion string
	Revision      string
	Chunks        []Chunk
}

func NewCorpus(policyVersion string, chunks []Chunk) (Corpus, error)

// internal/ingest/contracts.go
type Store interface {
	Replace(context.Context, string, source.Corpus) error
}

type Scanner interface {
	Scan(context.Context, string) (ScanResult, error)
}
```

- [ ] **Step 1: Escrever tabela falha de hard deny**

Tabela inclui, em qualquer profundidade:

```text
directories:
  .git .github .gitlab .circleci .azure .aws .ssh .gnupg .kube
  .gocontext .idea .vscode .devcontainer .terraform .serverless
  node_modules vendor .venv venv __pycache__ .pytest_cache .mypy_cache
  .ruff_cache .cache .next .nuxt .svelte-kit dist build out target
  coverage tmp temp Pods .gradle .dart_tool .pub-cache DerivedData Carthage
  .cxx .expo .turbo .nx .parcel-cache .vite .bundle

files/basenames:
  .env .env.* .npmrc .pypirc .netrc .htpasswd
  credentials credentials.* secret secret.* secrets secrets.*
  id_rsa* id_ed25519* authorized_keys known_hosts
  Jenkinsfile .travis.yml azure-pipelines.yml bitbucket-pipelines.yml
  renovate.json dependabot.yml

extensions/suffixes:
  *.pem *.key *.p12 *.pfx *.crt *.cer *.der *.jks *.keystore
  *.kdbx *.asc *.gpg *.tfstate *.tfstate.* *.min.* *.map
  *.generated.* *.gen.*
```

Usar fixtures com extensões hoje suportadas dentro de cada diretório e nomes como `credentials.py`, `secret.ts` e `.env.ts`. Confirmar zero `source.File` e contagem correta por categoria.

Matching de diretório/basename/extensão usa `strings.EqualFold`; path precisa ser UTF-8 válido e segmentos com caracteres Unicode de controle/formatação falham fechados. Testes incluem case variants, UTF-8 inválido e caracteres invisíveis.

- [ ] **Step 2: Provar decisão antes de leitura**

Extrair seam interno de filesystem/open usado somente por scanner e white-box tests. Spy deve falhar se `Open` receber path hard-denied. Rodar scan sobre fixtures e confirmar nenhum path sensível foi aberto. Policy de path roda antes de `languageForPath`, `entry.Info` e `Open` quando informação de `DirEntry` basta.

- [ ] **Step 3: Testar nested repos, symlinks, races e traversal**

Antes de processar qualquer filho, scanner faz preflight do diretório inteiro; presença de `.git` directory ou gitfile marca nested repo e ignora subtree. Não usar `fs.WalkDir` streaming para essa decisão. Symlink de arquivo/diretório nunca é seguido. Open descriptor-relative usa no-follow e compara identidade/tipo antes/depois do open para detectar swap; teste determinístico troca arquivo por symlink entre inspeção e abertura. Dentro do traversal, paths precisam ser relativos: path absoluto, `..`, barra invertida e mudança de raiz falham. Argumento CLI `root` continua raiz canônica absoluta. Testar nested repo com arquivo cujo nome ordena antes de `.git`.

- [ ] **Step 4: Testar binários, grandes, especiais, gerados e segredo em fonte comum**

Arquivos não regulares, byte NUL, UTF-8 inválido, acima de 1 MiB, nomes gerados e headers `Code generated ... DO NOT EDIT` não entram. Binary/large podem ser abertos somente até limite atual; conteúdo lido não aparece em report/error. Detector conservador cobre PEM/private-key markers, credenciais cloud conhecidas e assignments de token/password/secret com valor literal; match exclui arquivo antes de criar `source.File`. Documentar falso positivo e limite: scanner precisa ler arquivo de nome permitido para detectar segredo desconhecido. Arquivo permitido com erro de leitura causa falha fechada do scan, não skip silencioso.

- [ ] **Step 5: Fixar precedência de policy**

Ordem M2:

```text
1. hard deny security/metadata/nested repo/symlink (não substituível)
2. built-in deny de deps/build/cache/generated (não substituível)
3. deny adicional fornecido fora do repositório (somente exclui)
4. allowlist de extensão/language
5. limite/tipo/binary check
```

M2 não lê `.gitignore`, `.github/**` ou config interna para alterar policy. Futuro suporte deny-only a `.gitignore` exige ADR explícito; regra nunca pode reabilitar item hard-denied.

- [ ] **Step 6: Implementar report sanitizado**

Report contém somente contagens, bytes elegíveis/incluídos, linguagens, size bands fixos e extensões normalizadas não sensíveis. Extensão em item security-denied fica agregada somente na categoria. Sem paths, basenames, conteúdo ou amostras. Erros/logs usam categoria e hash curto do path quando correlação for necessária; nunca conteúdo. Search output solicitado continua mostrando citação permitida.

- [ ] **Step 7: Atualizar scanner/callers e confirmar fail-closed**

`cmd/gocontext/index.go` consome `ScanResult.Files`; estatísticas podem usar contagens agregadas. Falha de policy, auditoria ou leitura permitida aborta indexação antes de parser/chunker/store.

Bump do snapshot JSON para schema/policy segura invalida snapshot legado. Depois do chunking, composition root cria `source.Corpus` com policy do `ScanResult`; revisão é SHA-256 de versão do corpus + policy + IDs ordenados. `localstore.Replace` recebe corpus completo e nunca marca versão implicitamente; `Load` retorna erro tipado pedindo reindex. Testes cobrem payload v1 contendo canário e confirmam ausência em search/output. Bump é obrigatório quando deny/precedência/classificadores de segredo, binário, UTF-8, gerado, symlink ou nested repo mudarem. Tasks SQLite recebem mesmo corpus.

Refinamento Task 14C: `scanner-v5` acrescenta os roots de dependências/build/cache acima com decisão em `classifyPath` antes de `inspectRepositoryEntry`/open e mantém nomes ambíguos como `packages` permitidos. A taxonomia central de buckets agrega extensões source/build não sensíveis e assets/binários comuns, sempre lowercase e sem paths, nomes ou amostras; `<none>` e `<other>` continuam cobrindo extension-less e valores arbitrários. Isso melhora somente o sinal de inventário e não torna extensão nova ingestível. Corpora/snapshots `scanner-v4` são rejeitados e exigem reindex completo, sem migração.

Refinamento Task 14D: `scanner-v6` torna `.js`/`.jsx` elegíveis, case-insensitive, somente depois de todos os gates anteriores. `source.LanguageJavaScript` atravessa scanner, parser conservador, chunker, snapshot/SQLite, busca lexical/vetorial/híbrida, filtro CLI e evaluator aggregate-only. O parser reconhece apenas funções/classes nomeadas e variáveis top-level diretamente atribuídas a arrow/function expression; uma pilha lexical única, limitada e ordenada exclui blocos aninhados mesmo sem indentação, rejeita delimitadores cruzados, e listas de parâmetros só aceitam identificadores simples, completos, únicos e não reservados. Contextos de header/bloco de controle e bloco de statement no início da linha protegem regex após `)`/`}`; object literal provado por atribuição/agrupamento preserva divisão e slash após outra chave torna o restante fail-closed, inclusive por comentários/quebra de linha. JSX multilinha fica opaco e candidate exige linha lexicalmente confiável. A validação adicional do arrow body preserva cancelamento/checkpoints lineares e percorre containers/substituições fora de literais, comentários, regex, template raw e texto/tag JSX. Arrow body usa um subconjunto conservador de starters com unários completos, `await` em qualquer profundidade somente em arrow sintaticamente `async`, `yield` rejeitado, prefixos `function`/`class` completos e JSX conciso fechado na mesma linha. Regex/templates/JSX reconhecidos não contaminam profundidade; propriedades homônimas aos tokens proibidos, slash-after-brace ambíguo, starters fora do subconjunto e outras formas não modeladas falham para falso negativo em vez de inventar símbolo. Não há default anônimo nem alegação de completude estrutural. JSON continua unsupported e não há fallback de bytes crus. Corpora/snapshots `scanner-v5` exigem reindex completo pela categoria existente. A implementação e o taint renovado usam exclusivamente fixtures sintéticas; o baseline profissional `scanner-v5` não é reinterpretado e nenhuma qualidade profissional nova é alegada antes de outra execução gated.

- [ ] **Step 8: Verificar regressão e commit**

Run: `go test -race ./internal/ingest/filesystem ./internal/ingest/localstore ./cmd/gocontext && go test ./... && go vet ./...`

```bash
git add internal/ingest cmd/gocontext/index.go cmd/gocontext/index_test.go cmd/gocontext/search_test.go
git commit -m "feat: harden repository scan policy"
```

## Phase 1 — Contratos e provider HTTP

### Task 1: Contrato provider-agnostic de embeddings

**Files:**

- Create: `internal/embedding/contracts.go`
- Test: `internal/embedding/contracts_test.go`

**Interfaces:**

- Produces: `Purpose`, `Profile`, `Vector`, `Batch`, `Embedder`, `ValidateBatch(Batch, int) error`
- Consumes: somente `context`; nenhuma dependência de `search`, `index` ou provider

- [ ] **Step 1: Escrever testes falhos de invariantes**

```go
func TestValidateBatchAcceptsOrderedFiniteVectors(t *testing.T) {
	batch := embedding.Batch{
		Profile: embedding.Profile{Fingerprint: "profile", Model: "model"},
		Dimensions: 2,
		Vectors: []embedding.Vector{{1, 0}, {0, 1}},
		Requests: 1,
	}
	if err := embedding.ValidateBatch(batch, 2); err != nil {
		t.Fatalf("ValidateBatch() error = %v", err)
	}
}

func TestValidateBatchRejectsInvalidShapeAndValues(t *testing.T) {
	tests := []embedding.Batch{
		{Profile: embedding.Profile{}, Dimensions: 2, Vectors: []embedding.Vector{{1, 0}}, Requests: 1},
		{Profile: embedding.Profile{Fingerprint: "p", Model: "m"}, Dimensions: 2, Requests: 1},
		{Profile: embedding.Profile{Fingerprint: "p", Model: "m"}, Dimensions: 2, Vectors: []embedding.Vector{{1}}, Requests: 1},
		{Profile: embedding.Profile{Fingerprint: "p", Model: "m"}, Dimensions: 1, Vectors: []embedding.Vector{{float32(math.NaN())}}, Requests: 1},
		{Profile: embedding.Profile{Fingerprint: "p", Model: "m"}, Dimensions: 2, Vectors: []embedding.Vector{{0, 0}}, Requests: 1},
		{Profile: embedding.Profile{Fingerprint: "p", Model: "m"}, Dimensions: 2, Vectors: []embedding.Vector{{1, 0}}},
	}
	for _, batch := range tests {
		if err := embedding.ValidateBatch(batch, 1); err == nil {
			t.Errorf("ValidateBatch(%#v) error = nil", batch)
		}
	}
}
```

- [ ] **Step 2: Executar testes e confirmar falha de compilação**

Run: `go test ./internal/embedding -run TestValidateBatch -v`

Expected: FAIL porque pacote/tipos ainda não existem.

- [ ] **Step 3: Implementar tipos e validação mínima**

Validação deve exigir profile completo, `Dimensions > 0`, `Requests > 0`, quantidade igual a `expected`, dimensões uniformes, números finitos e vetor não zero. Erros exportados: `ErrInvalidBatch`, `ErrInvalidVector`.

- [ ] **Step 4: Executar testes do pacote e suite**

Run: `go test ./internal/embedding -v && go test ./...`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/embedding
git commit -m "feat: define provider-agnostic embedding contract"
```

### Task 2: Configuração e fingerprint OpenAI-compatible

**Files:**

- Create: `internal/embedding/openaicompat/config.go`
- Test: `internal/embedding/openaicompat/config_test.go`

**Interfaces:**

- Consumes: `embedding.Profile`
- Produces: `Config`, `New(Config) (*Client, error)`, profile estável sem segredo

```go
type Config struct {
	BaseURL           string
	Model             string
	APIKey            string
	Dimensions        int
	BatchSize         int
	MaxBatchBytes     int
	MaxInFlight       int
	Timeout           time.Duration
	MaxRetries        int
}
```

- [ ] **Step 1: Escrever tabela de validação de config**

Cobrir: HTTPS aceito; `http://127.0.0.1` e `http://[::1]` aceitos; `http://localhost`, HTTP remoto, userinfo, query, fragment, modelo vazio, limites negativos e path fora de `/v1` rejeitados. HTTP exige literal cujo `net.IP.IsLoopback()` seja true; não resolve hostname. `BaseURL` normaliza barra final e endpoint final é `<base>/embeddings`.

- [ ] **Step 2: Escrever teste de fingerprint**

```go
func TestProfileFingerprintExcludesCredential(t *testing.T) {
	first := validConfig()
	first.APIKey = "secret-one"
	second := first
	second.APIKey = "secret-two"

	firstClient, _ := openaicompat.New(first)
	secondClient, _ := openaicompat.New(second)
	if firstClient.Profile() != secondClient.Profile() {
		t.Fatal("credential changed embedding profile")
	}
	if strings.Contains(fmt.Sprintf("%+v", firstClient.Profile()), "secret") {
		t.Fatal("profile contains credential")
	}
}
```

- [ ] **Step 3: Executar teste falho**

Run: `go test ./internal/embedding/openaicompat -run 'TestConfig|TestProfile' -v`

Expected: FAIL porque `Config` e `New` não existem.

- [ ] **Step 4: Implementar defaults e fingerprint**

Defaults exatos: batch 32, 256 KiB, max in-flight 2, timeout 30s, retries 2. Fingerprint é SHA-256 de JSON canônico contendo `protocol_version`, endpoint normalizado, modelo, dimensões solicitadas, wire encoding `float32-v1` e normalização vetorial `cosine-unit-f32-v1`. Client cria transport próprio com proxy desabilitado e redirects sempre rejeitados; testes podem injetar somente clock/sleeper privados, não policy de rede.

- [ ] **Step 5: Executar testes e commit**

Run: `go test ./internal/embedding/openaicompat -v && go test ./...`

```bash
git add internal/embedding/openaicompat
git commit -m "feat: validate OpenAI-compatible embedding config"
```

### Task 3: Cliente OpenAI-compatible com batching e retry

**Files:**

- Create: `internal/embedding/openaicompat/client.go`
- Test: `internal/embedding/openaicompat/client_test.go`

**Interfaces:**

- Consumes: `embedding.Embedder`, config da Task 2
- Produces: `Client.Embed` compatível com OpenAI e Ollama sem SDK

- [ ] **Step 1: Escrever teste `httptest` do wire contract**

Servidor deve afirmar `POST /v1/embeddings`, `Content-Type: application/json`, bearer quando configurado, `model`, `input`, `encoding_format: "float"` e `dimensions` somente quando positivo. Resposta retorna itens fora de ordem; cliente deve ordenar por `index`.

- [ ] **Step 2: Escrever testes de segurança e erros**

Cobrir: sem Authorization quando key vazia; qualquer redirect rejeitado; corpo acima de 16 MiB rejeitado; corpo remoto nunca é incluído em erro/log; status/erro não vazam API key/input; count/índice/dimensão inválidos retornam `ErrInvalidBatch`; cancelamento preserva `context.Canceled`. `DialContext` para URL HTTP local valida `RemoteAddr` com `net.IP.IsLoopback()` antes de HTTP; testes cobrem IPv4, IPv6 e dialer hostil retornando peer não-loopback.

- [ ] **Step 3: Escrever testes de retry determinístico**

Injetar clock/sleeper privado. Confirmar máximo de três tentativas totais em conexão temporária, `408`, `429` e `5xx`; zero retry em outros `4xx`; `Retry-After` respeitado somente dentro do deadline. Após esgotar, todos temporários retornam `ErrSemanticUnavailable`; matriz table-driven é reutilizada por builder/hybrid via `errors.Is`.

- [ ] **Step 4: Executar testes e confirmar falhas**

Run: `go test ./internal/embedding/openaicompat -run 'TestClient|TestRetry' -v`

Expected: FAIL porque `Embed` não existe.

- [ ] **Step 5: Implementar request, batching e retry**

Batch respeita simultaneamente `BatchSize` e `MaxBatchBytes`; ordem final corresponde à ordem global das entradas. Concorrência usa errgroup equivalente com biblioteca padrão, limite `MaxInFlight`, cancelamento do primeiro erro fatal e acumulação segura de `Requests` e `usage.total_tokens`. Transport próprio implementa validação dial-time para HTTP loopback, além da validação sintática da Task 2.

Fixtures de conformidade cobrem subset planejado de respostas OpenAI e Ollama. Teste de integração Ollama real é opt-in por env, usa apenas texto sintético e loopback; ausência não falha CI. Compatibilidade permanece alvo verificado por protocolo, não promessa de versão/vendor live.

- [ ] **Step 6: Verificar interface, race e suite**

Run: `go test -race ./internal/embedding/openaicompat && go test ./... && go vet ./...`

Expected: PASS, nenhuma rede externa.

- [ ] **Step 7: Commit**

```bash
git add internal/embedding/openaicompat
git commit -m "feat: add OpenAI-compatible embedding client"
```

## Phase 2 — Contratos de consulta e store local

### Task 4: Filtros tipados preservando busca lexical

**Files:**

- Modify: `internal/search/contracts.go`
- Modify: `internal/search/lexical/searcher.go`
- Test: `internal/search/lexical/searcher_test.go`

**Interfaces:**

- Produces: `search.Filter`; `Query.Filter` com zero value igual ao comportamento atual
- Consumes: `source.Language`, paths canônicos de `source.Reference`

- [ ] **Step 1: Adicionar testes falhos de filtro e regressão**

Testar OR entre `PathPrefixes`, OR entre `Languages`, AND entre categorias, prefixo `internal/` sem casar `internalized/`, filtro inválido rejeitado e query sem filtro mantendo IDs/scores atuais.

- [ ] **Step 2: Executar testes focados**

Run: `go test ./internal/search/lexical -run 'TestSearcherFilters|TestSearcherRanks' -v`

Expected: FAIL nos novos casos.

- [ ] **Step 3: Implementar validação compartilhada**

Adicionar `search.ValidateFilter(Filter) error` e `search.MatchesFilter(source.Chunk, Filter) bool`. Normalizar apenas separadores já aceitos por `source.Reference`; rejeitar path absoluto, `..`, barra invertida e language desconhecida.

- [ ] **Step 4: Aplicar filtro antes de scoring e limit**

Não mudar pesos `0.6/0.3/0.1`, tokenização ou tie-breakers.

- [ ] **Step 5: Verificar regressão e commit**

Run: `go test ./internal/search/... && go test ./cmd/gocontext -run TestRunSearch -v`

```bash
git add internal/search/contracts.go internal/search/lexical
git commit -m "feat: add repository search filters"
```

### Task 5: Spike-gate SQLite e persistência de corpus

**Files:**

- Modify: `go.mod`
- Modify: `go.sum`
- Create: `internal/index/contracts.go`
- Create: `internal/index/sqlite/schema.go`
- Create: `internal/index/sqlite/store.go`
- Test: `internal/index/sqlite/store_test.go`
- Modify: `docs/decisions/0002-embeddings-vector-search.md` somente para registrar versão do driver escolhida

**Interfaces:**

- Produces: `index.Generation`, `index.Store`, `sqlite.Store.Replace`, `sqlite.Store.Load`, `sqlite.Store.BindActive`
- `sqlite.Store.Load` satisfaz `lexical.SnapshotLoader` por structural typing

- [ ] **Step 1: Validar driver puro Go em spike curto**

Run: `go get modernc.org/sqlite@latest && go mod tidy`

Aceitar somente se `go test ./...`, `go vet ./...`, build sem `CGO_ENABLED=1` obrigatório e licença permitirem distribuição do projeto. Fixar versão resolvida em `go.mod` e registrar no ADR. Se gate falhar, parar tarefa e abrir decisão específica; não trocar silenciosamente por driver CGO.

- [ ] **Step 2: Escrever testes falhos do store**

Casos: criar banco em diretório privado; publicar chunks e carregar igualdade; isolamento por repository ID; referências/IDs/policy inválidos; policy antiga retorna `ErrReindexRequired`; geração concorrente com `BaseGeneration` obsoleto retorna `ErrConcurrentIndex`; republicação do mesmo ID é idempotente; cancelamento; transação falha preserva manifest anterior.

- [ ] **Step 3: Executar testes**

Run: `go test ./internal/index/sqlite -run TestStore -v`

Expected: FAIL porque store/schema não existem.

- [ ] **Step 4: Criar schema v1**

Schema deve conter `schema_version`, `repositories`, `generations`, `chunks` e `vectors`; foreign keys habilitadas; manifest ativo em `repositories.active_generation`; chave composta inclui repositório + geração. `PRAGMA journal_mode=WAL`, `foreign_keys=ON`, busy timeout 5s. Arquivo: `<store-dir>/index-v2.sqlite3`, permissão do diretório `0700` e banco `0600` quando criado.

- [ ] **Step 5: Implementar publicação transacional**

Inserir geração/chunks, comparar `BaseGeneration`, atualizar manifest por último e commit. Se ID já for geração ativa e metadados coincidirem, retornar sucesso sem reescrever. Depois do commit, remover todas as gerações inativas, executar checkpoint/truncate do WAL e testar que canário removido não existe no banco/WAL. Geração anterior permanece visível somente até conclusão atômica da publicação; rollback usa snapshot atual com mesma policy, não corpus histórico SQLite.

- [ ] **Step 6: Verificar concorrência, fallback loader e build**

Run: `go test -race ./internal/index/sqlite && CGO_ENABLED=0 go test ./... && go vet ./...`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum internal/index docs/decisions/0002-embeddings-vector-search.md
git commit -m "feat: add transactional SQLite corpus store"
```

### Task 6: Vetores versionados e cosseno exato

**Files:**

- Create: `internal/search/vector/contracts.go`
- Create: `internal/index/sqlite/vector.go`
- Test: `internal/index/sqlite/vector_test.go`
- Modify: `internal/index/sqlite/store.go`

**Interfaces:**

- Consumes: `index.Generation.Vectors`, `embedding.Profile`, `embedding.Vector`
- Produces: `vector.Metadata`, `vector.IndexQuery`, `vector.Candidate`, `vector.Index`; SQLite satisfaz interface que Task 8 consumirá

- [ ] **Step 1: Escrever testes de encoding**

Round trip `float32` little-endian; rejeitar BLOB com tamanho não múltiplo de 4, dimensão divergente, `NaN`, infinito e vetor zero.

- [ ] **Step 2: Escrever testes de ranking exato**

Fixture com três vetores conhecidos deve ordenar por cosseno, aplicar filtro de linguagem/path antes de limit, manter tie-break por path/linha/ID e retornar chunk canônico completo.

- [ ] **Step 3: Escrever testes de incompatibilidade**

Fingerprint ou dimensão divergente retorna `ErrIncompatibleSpace`; vetor referenciando chunk inexistente causa falha de integridade; geração lexical-only retorna `ErrVectorUnavailable`.

`BindActive(ctx, repositoryID)` inicia read transaction SQLite e retorna `BoundReader` com `GenerationID()` e `Close()`. Loader lexical e busca vetorial desse reader usam mesma transaction/ID; troca do manifest e purge concorrentes não alteram consulta em andamento. `Close` é obrigatório e idempotente. Cleanup remove linhas inativas, mas checkpoint/truncate espera readers fecharem; teste bloqueia busca híbrida entre lexical/vector, publica geração nova, executa cleanup e confirma resultado antigo consistente até `Close`, depois ausência do canário no banco/WAL.

- [ ] **Step 4: Executar testes falhos**

Run: `go test ./internal/index/sqlite -run 'TestVector|TestExactSearch' -v`

- [ ] **Step 5: Implementar persistência e scan exato**

Normalizar vetores uma vez antes de persistir e query antes do cálculo. Similaridade usa acumulador `float64`; score interno retorna cosseno e Task 8 normaliza para `[0,1]`.

- [ ] **Step 6: Verificar race, corrupção e suite**

Run: `go test -race ./internal/index/sqlite && go test ./...`

- [ ] **Step 7: Commit**

```bash
git add internal/index/sqlite
git commit -m "feat: persist and search exact vectors"
```

## Phase 3 — Lifecycle e recuperação semântica

### Task 7: Builder de geração atômica

**Files:**

- Create: `internal/index/builder.go`
- Test: `internal/index/builder_test.go`

**Interfaces:**

- Consumes: `embedding.Embedder`, `index.Store`, `SemanticMode`
- Produces: `index.NewBuilder`, `(*index.Builder).Replace`, `index.Report`, erros categorizados `ErrSemanticUnavailable`, `ErrConcurrentIndex`, `ErrCostLimit`

```go
type BuilderConfig struct {
	Mode           SemanticMode
	MaxChunks      int
	MaxSourceBytes int64
}

type Report struct {
	GenerationID  string
	CorpusRevision string
	Chunks         int
	Vectors        int
	Requests       int
	UsageTokens    int
	Semantic       string
	DegradedReason string
}
```

- [ ] **Step 1: Escrever testes do modo `off`**

Confirmar zero chamadas ao embedder, geração com chunks e sem vetores, revisão determinística independente da ordem de entrada e publicação com `BaseGeneration` atual.

- [ ] **Step 2: Escrever testes `preferred` e `required`**

`preferred` + erro temporário publica geração lexical-only e retorna report degradado; `required` + mesmo erro não chama `Store.Replace`; resposta malformada é fatal em ambos; contexto cancelado sempre retorna erro e não publica.

- [ ] **Step 3: Escrever testes de modelo/dimensão e custo**

Batch cujo profile difere de `Embedder.Profile`, quantidade/dimensão inválida ou vetor inválido falha; mais de 20.000 chunks ou 64 MiB de texto falha antes de rede; todos os chunks são reembeddados mesmo quando IDs já existiam, documentando rebuild completo do M2.

- [ ] **Step 4: Executar testes falhos**

Run: `go test ./internal/index -run TestBuilder -v`

- [ ] **Step 5: Implementar revisão e publicação**

Usar `source.Corpus.Revision`, já validada por `source.NewCorpus`; não duplicar hashing. `GenerationID` combina policy + revisão + fingerprint ou literal `lexical-only`. Validar corpus antes de chamar provider. Somar tokens/requests sem registrar texto.

- [ ] **Step 6: Implementar classificação de erro**

Somente timeout interno e `ErrSemanticUnavailable` produzido após conexão temporária/`408`/`429`/`5xx` esgotarem são degradáveis. Erros de config, integridade, batch e cancelamento pai são fatais. Usar `errors.Is`/`errors.As`, nunca comparação por texto; tabela é mesma da Task 3.

- [ ] **Step 7: Verificar race e suite**

Run: `go test -race ./internal/index && go test ./... && go vet ./...`

- [ ] **Step 8: Commit**

```bash
git add internal/index
git commit -m "feat: build atomic repository generations"
```

### Task 8: Searcher vetorial com preflight e citação canônica

**Files:**

- Create: `internal/search/vector/searcher.go`
- Test: `internal/search/vector/searcher_test.go`
- Modify: `internal/index/sqlite/vector.go` somente para satisfazer interface final

**Interfaces:**

- Consumes: `embedding.Embedder`, `search.Query`, índice abaixo
- Produces: `vector.Searcher` satisfazendo `search.Searcher`

```go
type Metadata struct {
	GenerationID  string
	Profile        embedding.Profile
	Dimensions     int
	CorpusRevision string
}

type IndexQuery struct {
	RepositoryID string
	GenerationID string
	Profile      embedding.Profile
	Dimensions   int
	Vector       embedding.Vector
	Filter       search.Filter
	Limit        int
}

type Candidate struct {
	Chunk      source.Chunk
	Similarity float64
}

type Index interface {
	Describe(context.Context, string) (Metadata, error)
	Search(context.Context, IndexQuery) ([]Candidate, error)
}
```

- [ ] **Step 1: Escrever teste de preflight**

Quando `Describe` retorna índice ausente, policy antiga ou fingerprint incompatível, `Embed` não é chamado. `Describe` vem do reader ligado por `BindActive`; `IndexQuery.GenerationID` precisa repetir metadata ou busca falha com `ErrGenerationChanged`. Erro tipado sobe para híbrido decidir fallback.

- [ ] **Step 2: Escrever teste de query e referência**

Uma query produz exatamente um embedding com `PurposeQuery`; `Index.Search` recebe mesmo generation ID e filtro; hits retornam o `source.Chunk` original com `Reference` idêntica byte a byte. Teste troca manifest entre `Describe` e `Search` e confirma leitura da geração fixada, nunca mistura.

- [ ] **Step 3: Escrever testes de integridade e ordem**

Rejeitar embedding de query com dimensão incompatível, candidato duplicado, referência inválida, chunk ID vazio e score fora de `[-1,1]`. Ordenar score normalizado desc, path, linha, ID.

- [ ] **Step 4: Executar testes falhos**

Run: `go test ./internal/search/vector -v`

- [ ] **Step 5: Implementar searcher mínimo**

Validar `search.Query` com mesmas regras lexicais. Respeitar exatamente `Query.Limit` recebido; expansão de janela pertence somente ao híbrido. Converter cosseno por `(similarity + 1) / 2`; limitar somente após validação/ordenação.

- [ ] **Step 6: Executar testes de integração com SQLite**

Run: `go test ./internal/search/vector ./internal/index/sqlite -v && go test ./...`

Expected: PASS; nenhum adapter concreto importado por `vector`.

- [ ] **Step 7: Commit**

```bash
git add internal/search/vector internal/index/sqlite/vector.go
git commit -m "feat: add canonical vector searcher"
```

### Task 9: Fusão RRF e fallback lexical observável

**Files:**

- Create: `internal/search/hybrid/searcher.go`
- Test: `internal/search/hybrid/searcher_test.go`

**Interfaces:**

- Consumes: lexical obrigatório e vector opcional, ambos `search.Searcher`
- Produces: `hybrid.Searcher` satisfazendo `search.Searcher`; `Observer` é seam interno para eventos sanitizados

```go
type Event struct {
	Backend string
	Kind    string
	Reason  string
	Latency time.Duration
}

type Observer interface {
	Observe(context.Context, Event)
}

type Config struct {
	Mode              SemanticMode
	RRFK              int
	LexicalWeight     float64
	VectorWeight      float64
	CandidateMultiplier int
	SemanticTimeout   time.Duration
}
```

`hybrid.SemanticMode` pertence ao read-side (`off|preferred|required`) e não importa `internal/index`. Composition root mapeia configuração CLI para modos de builder e hybrid.

Definir constantes homônimas localmente; não criar pacote genérico só para compartilhar três strings.

- [ ] **Step 1: Escrever testes de modo off/fallback**

`off` retorna slice lexical sem mudar score/ordem e sem evento. Vector nil/ausente: `preferred` retorna lexical com evento `vector_unavailable`; `required` falha; somente `off` permanece silencioso. Erro degradável retorna mesmo slice e evento sem query/endpoint/key.

- [ ] **Step 2: Escrever tabela RRF**

```go
func TestSearcherFusesRanksAndDeduplicatesChunks(t *testing.T) {
	lexical := fakeSearcher("a", "b", "c")
	vector := fakeSearcher("c", "b", "d")
	// c e b aparecem uma vez; ranking segue soma 1/(60+rank).
	// ties usam número de backends, melhor rank, path, line, ID.
}
```

Cobrir normalização `(0,1]`, limit, janela interna, igualdade determinística e ausência de soma de scores crus.

- [ ] **Step 3: Escrever testes de concorrência/cancelamento**

Lexical e vector começam antes de qualquer um terminar. Timeout semântico menor produz fallback sem cancelar lexical. Cancelamento pai retorna `context.Canceled`. Corrupção/integridade vector é fatal em `preferred`.

- [ ] **Step 4: Executar testes falhos**

Run: `go test ./internal/search/hybrid -v`

- [ ] **Step 5: Implementar concorrência e RRF**

Defaults: `k=60`, pesos 1/1, multiplier 4, janela mínima 20/máxima 200, timeout semântico 3s para busca. Não iniciar goroutine vector em `off`.

- [ ] **Step 6: Verificar race e regressão lexical**

Run: `go test -race ./internal/search/hybrid ./internal/search/lexical && go test ./...`

- [ ] **Step 7: Commit**

```bash
git add internal/search/hybrid
git commit -m "feat: add hybrid RRF retrieval with lexical fallback"
```

## Phase 4 — Composition root e comportamento CLI

### Task 10: Parser de configuração sem segredos

**Files:**

- Create: `cmd/gocontext/embedding_config.go`
- Test: `cmd/gocontext/embedding_config_test.go`
- Modify: `cmd/gocontext/index.go`
- Modify: `cmd/gocontext/search.go`

**Interfaces:**

- Produces: `embeddingOptions`, `addEmbeddingFlags`, `resolveEmbeddingConfig`, `indexBackend`
- Consumes: flags `semantic`, `embedding-base-url`, `embedding-model`, `embedding-dimensions`, `index-backend`; env definida no ADR

- [ ] **Step 1: Escrever testes de precedência e defaults**

Sem config resulta `off`. Endpoint + modelo, inclusive por env, sem modo explícito continuam `off` e fazem zero request. `preferred|required` exigem endpoint/modelo. Flags não secretos vencem env. `--semantic off` desabilita rede mesmo com env presente. Config parcial em modo habilitado falha com uso, não erro operacional.

- [ ] **Step 2: Escrever testes de segredo**

Não existe flag `--embedding-api-key`. API key só é lida após modo/config válidos. `fmt`, usage e erros nunca contêm valor. Ollama loopback funciona sem key.

- [ ] **Step 3: Escrever testes de backend**

`index` e `search` default `snapshot`; valores aceitos `snapshot|sqlite|auto` conforme comando, mas `auto` de search é sempre opt-in até ADR de promoção. Semântica habilitada com index backend snapshot retorna uso indicando `--index-backend sqlite`.

- [ ] **Step 4: Executar testes falhos**

Run: `go test ./cmd/gocontext -run 'TestEmbeddingConfig|TestIndexBackend' -v`

- [ ] **Step 5: Implementar parser central**

Usar `flag.FlagSet`; nunca copiar API key para mensagens. Validar batch 1..128, in-flight 1..8, timeout positivo, dimensões positivas quando presentes e max chunks positivo. Manter biblioteca padrão `flag`.

- [ ] **Step 6: Verificar usage atual**

Run: `go test ./cmd/gocontext -run 'TestRunIndexRejectsInvalidUsage|TestRunSearchRejectsInvalidUsage' -v && go test ./...`

- [ ] **Step 7: Commit**

```bash
git add cmd/gocontext/embedding_config.go cmd/gocontext/embedding_config_test.go cmd/gocontext/index.go cmd/gocontext/search.go
git commit -m "feat: parse semantic retrieval configuration"
```

### Task 11: Tracer bullet CLI index → busca híbrida

**Files:**

- Modify: `cmd/gocontext/index.go`
- Modify: `cmd/gocontext/index_test.go`
- Modify: `cmd/gocontext/search.go`
- Modify: `cmd/gocontext/search_test.go`
- Modify: `cmd/gocontext/main.go`

**Interfaces:**

- Consumes: modules das Tasks 1–10
- Produces: fluxo executável opt-in sem alterar comportamento CLI default

- [ ] **Step 1: Fixar regressão default antes da fiação**

Adicionar testes que executam `index` e `search` sem env semântico, confirmam zero requests em `httptest`, snapshot JSON atual e saída lexical existente (`0.950`, citação, texto e `nenhum resultado`).

- [ ] **Step 2: Escrever teste E2E OpenAI-compatible**

Servidor `httptest` retorna vetores para dois chunks e query conceitual sem token lexical comum. `index --index-backend sqlite --semantic preferred ...` publica dois vetores; `search --index-backend auto --semantic preferred ...` retorna chunk esperado e referência original.

- [ ] **Step 3: Escrever testes de fallback e modo required**

Provider indisponível em `preferred`: index publica corpus lexical-only, search retorna lexical e stderr contém aviso genérico. Em `required`: index retorna código 1 e geração ativa anterior continua pesquisável durante falha. Ausência de SQLite/vetores em search: preferred avisa e cai para lexical; required falha; off fica silencioso.

- [ ] **Step 4: Escrever testes de filtros e segurança terminal**

Flags repetíveis `--path-prefix` e `--language` chegam aos dois backends. Controle terminal em path/símbolo/texto continua escapado. Aviso nunca contém query, fonte ou key.

- [ ] **Step 5: Executar testes falhos**

Run: `go test ./cmd/gocontext -run 'TestRun(Index|Search).*(Semantic|Fallback|Default|Filter)' -v`

- [ ] **Step 6: Fiar adapters somente em `cmd/gocontext`**

Index SQLite: scanner/parser/chunker existentes → `source.NewCorpus` → `index.Builder`. Após publicação SQLite bem-sucedida, gravar snapshot rollback do mesmo corpus e marcar `rollback_ready` somente quando revisões coincidirem; falha intermediária é reportada e snapshot antigo não é chamado de pronto. Search default lê snapshot seguro. Search `auto|sqlite`: abrir `sqlite.Store.BindActive`, `defer Close`, e construir ambos backends sobre reader/transação ligados. `auto` usa snapshot seguro quando SQLite não existe. Ausência semântica segue modo explícito, nunca fallback silencioso em `required`.

- [ ] **Step 7: Adicionar report e observer CLI**

Stdout mantém hits. Stderr recebe somente warnings sanitizados. Index report inclui arquivos/símbolos/chunks atuais mais vetores/requests/tokens quando semântica executou.

- [ ] **Step 8: Verificar suite, race e vet**

Run: `go test -race ./cmd/gocontext ./internal/... && go test ./... && go vet ./...`

- [ ] **Step 9: Commit**

```bash
git add cmd/gocontext
git commit -m "feat: wire opt-in semantic indexing and hybrid search"
```

## Phase 5 — Rollout, rollback e evidência operacional

### Task 12: Migração por reindex, benchmark e documentação final

**Files:**

- Modify: `README.md`
- Modify: `docs/architecture.md`
- Modify: `docs/roadmap.md`
- Modify: `docs/decisions/0002-embeddings-vector-search.md`
- Create: `internal/index/sqlite/benchmark_test.go`
- Test: `cmd/gocontext/index_test.go`
- Test: `cmd/gocontext/search_test.go`

**Interfaces:**

- Produces: rollout reversível, runbook de reindex, critérios de promoção do backend
- Consumes: CLI concluída na Task 11

- [ ] **Step 1: Criar benchmark manual reproduzível**

`BenchmarkExactSearch10000x1536` gera vetores deterministicamente fora do timer, mede scan/cosseno e reporta alocações. Não vira gate rígido de CI; documentar máquina e resultado no ADR. Emitir warning operacional acima de 20.000 chunks, sem trocar automaticamente para ANN.

- [ ] **Step 2: Escrever teste de rollout**

Com somente snapshot JSON de policy atual, `search --index-backend auto` usa lexical. Após reindex com SQLite, auto usa geração SQLite. Sem flags, index e search continuam snapshot mesmo quando SQLite existe. Teste explícito: criar SQLite, executar index comum e confirmar search comum lê snapshot novo, não SQLite antigo.

- [ ] **Step 3: Escrever teste de rollback**

`search --index-backend snapshot` retorna snapshot somente se policy atual e, quando SQLite existe, `CorpusRevision` coincide com geração ativa/marker `rollback_ready`. Divergência retorna `ErrReindexRequired`. Snapshot v1/policy antiga nunca retorna conteúdo. Falha semântica/SQLite antes da publicação não altera geração ativa; falha ao gravar snapshot depois da publicação deixa rollback não pronto e erro explícito. Após sucesso, gerações inativas e WAL não retêm canário removido. Se SQLite estiver indisponível/corrupto, operador reindexa snapshot antes de usar rollback; não confia em cache antigo.

- [ ] **Step 4: Documentar migração**

Migração suportada é reindex explícito. Index SQLite grava snapshot companheiro do mesmo corpus depois de publicar geração e verifica revisão:

```bash
gocontext index --index-backend sqlite --semantic off /repo
gocontext search --index-backend auto /repo consulta
```

Semântica remota exige endpoint/modelo explícitos; exemplo OpenAI usa env key, exemplo Ollama usa loopback sem key. Incluir aviso de egress antes dos exemplos.

- [ ] **Step 5: Documentar rollback**

```bash
gocontext search --index-backend snapshot /repo consulta
```

Não instruir exclusão do SQLite antes de confirmar `rollback_ready`. Se snapshot estiver ausente, legado ou com revisão diferente, reindexar antes do rollback. Promoção de SQLite a default é decisão separada após uso real; snapshot seguro permanece default/fallback durante M2.

- [ ] **Step 6: Atualizar estado e não objetivos**

README e roadmap devem distinguir entregue vs futuro. Anthropic aparece somente em M3/generation; Voyage, se citado, aparece como terceiro. Incremental reuse, ANN, reranker, provider SDK e vector DB externo permanecem futuros.

- [ ] **Step 7: Rodar verificação completa**

Run:

```bash
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=0 go test ./...
go run ./cmd/gocontext --version
```

Expected: todos passam; versão imprime `gocontext dev`; testes não acessam rede externa.

- [ ] **Step 8: Inspecionar dependências e segredos**

Run:

```bash
go mod tidy
go mod verify
go list -deps ./... >/dev/null
rg -n 'APIKey|Authorization|EMBEDDING_API_KEY' internal cmd docs
git diff --check
```

Confirmar: segredo só é lido no composition root/adapter; nenhuma fixture usa segredo real; logs/errors testados não vazam input.

- [ ] **Step 9: Commit**

```bash
git add README.md docs internal/index/sqlite/benchmark_test.go cmd/gocontext
git commit -m "docs: complete semantic retrieval rollout guide"
```

## Phase 6 — Prova de não vazamento e validação profissional local

### Task 13: Teste taint ponta a ponta de conteúdo excluído

**Files:**

- Create: `internal/ingest/filesystem/taint_test.go`
- Test: `internal/ingest/filesystem/scanner_test.go`
- Test: `cmd/gocontext/index_test.go`
- Test: `cmd/gocontext/search_test.go`

**Interfaces:**

- Produces: evidência executável de que bytes hard-denied alcançam zero sinks
- Consumes: scanner da Task 0 e pipeline CLI da Task 11

- [ ] **Step 1: Criar canários únicos por categoria**

Cada fixture hard-denied contém token aleatório determinístico diferente. Incluir `.env.ts`, `credentials.py`, `.github/workflows/ci.ts`, nested repo, symlink externo, certificado, gerado, binário e oversize.

- [ ] **Step 2: Instrumentar sinks reais**

Usar temp dirs, parser/chunker reais e doubles somente nas bordas embedder/HTTP/store/logger. Capturar argumentos, banco/cache, stdout, stderr e erros. Não afirmar apenas chamada zero de mock: procurar cada canário em todos os bytes observáveis e confirmar índice permitido continua funcional.

- [ ] **Step 3: Cobrir falhas e degradação**

Repetir com semantic `off`, `preferred` com provider temporariamente indisponível e `required` com provider fake. Conteúdo excluído nunca chega ao transport mesmo em retry/error. Diagnóstico contém somente categoria/contagem.

- [ ] **Step 4: Verificar e commit**

Run: `go test -race ./internal/ingest/filesystem ./cmd/gocontext && go test ./... && go vet ./...`

```bash
git add internal/ingest/filesystem cmd/gocontext
git commit -m "test: prove excluded content cannot leave scanner"
```

### Task 14: Inventário e benchmark sanitizados dos repositórios Tivita

**Files:**

- Create: `internal/eval/inventory.go`
- Create: `internal/eval/inventory_test.go`
- Create: `internal/eval/metrics.go`
- Create: `internal/eval/metrics_test.go`
- Create: `cmd/gocontext/eval.go`
- Create: `cmd/gocontext/eval_test.go`
- Modify: `docs/plans/2026-08-27-tivita-professional-repository-validation.md`

**Interfaces:**

- Produces: harness local de inventário/avaliação com output agregado versionado
- Consumes: `ingest.ScanReport`, `search.Searcher`, checklist e matriz do plano de validação

- [ ] **Step 1: Testar schema sanitizado**

Fixtures controladas incluem paths, símbolos, queries, IDs por query e canários. JSON final aceita somente repository ID opaco, decisão, contagens, histogramas e métricas agregadas por categoria. Teste falha se qualquer input sensível ou registro por query aparecer serializado ou em erro.

- [ ] **Step 2: Implementar inventário pelo traversal protegido**

Harness recebe raiz explícita e output fora dela, recusa path ausente/relativo, endpoint não-loopback, output dentro do repo e checklist incompleto. Usa único traversal da Task 0; padrões/capacidades derivam somente de `ScanResult.Files` permitidos e resultados parser/chunker, nunca de segunda caminhada. Corpus não suportado fica `unknown/not evaluated`, sem inferência. Arquivo `0600`, escrita atômica.

- [ ] **Step 3: Implementar avaliação repetível**

Gold set local usa workspace não sincronizado `0700`, arquivos `0600` e relevância; calcular Recall@5/10, MRR@10, nDCG@10, validade de citações, latência e fallback por categoria. Query/hit/ID/rank individual não entra no resultado versionado; dados locais são removidos depois da agregação/revisão.

- [ ] **Step 4: Executar go/no-go individual**

Somente após Task 13 verde e paths locais autorizados encontrados. Rodar baseline `semantic off`. Rodar híbrido apenas via Ollama em IP loopback, transport sem proxy/redirect e provider sem logging/telemetria/retenção, se verificável. Nunca apontar Taba App, Tivita Backend ou Tivita Web App para serviço externo.

- [ ] **Step 5: Registrar agregados e gaps**

Preencher matriz com métricas agregadas; não copiar código, paths ou consultas. Se path/autorização/provider local faltar, registrar categoria `no-go` e continuar demais itens seguros.

- [ ] **Step 6: Priorizar suporte, sem generalização cega**

Ordenar linguagens/formatos/padrões pela fórmula do plano de validação. Cada capacidade nova vira plano pequeno, com fixture sintética/minimizada e critérios próprios; não adicionar parser durante inventário.

A primeira execução aggregate-only mostrou taxonomia incompleta e sinal compatível com provável ruído de dependências/build/cache; não houve observação de conteúdo dentro dos novos hard denies. Task 14C refina buckets sanitizados e policy pré-open antes de escolher qualquer parser. JSON continua `unknown/not evaluated` e não será habilitado cegamente: configuração JSON pode conter segredos ou dados sensíveis e requer plano/testes próprios.

Checkpoint `scanner-v6`: depois do tracer bullet JavaScript/JSX sintético, revisão
independente, taint renovado e três checklists privados novos, os três runs
lexicais seriais terminaram `go`, sem provider/rede. O agregado incluiu 378
JavaScript, barrou outros seis candidatos por segredo/tamanho e acrescentou só
seis símbolos `function`; 372 arquivos novos ficaram sem símbolo. Exact-symbol
geral permaneceu estável na precisão publicada, mas a amostra não informa
linguagem por query. Fronteira de citação JavaScript é evidência dos testes
sintéticos, não do run profissional. Gold set humano privado e adapter
estrutural JavaScript continuam próximos gates antes de alegar qualidade ou
executar semântica profissional.

- [ ] **Step 7: Verificar e commit**

Run: `go test -race ./internal/eval ./cmd/gocontext && go test ./... && go vet ./... && git diff --check`

Antes do commit, revisar resultado sanitizado e rodar scanner de segredos. Commit nunca inclui gold set, cache ou output com IDs/paths reais.

```bash
git add internal/eval cmd/gocontext docs/plans/2026-08-27-tivita-professional-repository-validation.md
git commit -m "feat: add privacy-safe local retrieval evaluation"
```

## Acceptance criteria

- CLI sem flags/env mantém index/search lexical atual e não abre rede.
- Mesmo adapter implementa subset OpenAI-compatible documentado e validado por fixtures OpenAI/Ollama; integração Ollama local é opt-in e nenhum teste live OpenAI é necessário.
- Nenhum tipo OpenAI/Ollama/Anthropic aparece em `source`, `search` ou `index` contracts.
- Indexação `required` é all-or-nothing; `preferred` pode publicar lexical-only com aviso.
- Manifest nunca ativa chunks de uma revisão com vetores de outra; consulta lexical/vector pinada usa um único `GenerationID`.
- Modelo/fingerprint/dimensão divergente nunca produz similaridade; pede reindex ou degrada lexical.
- Busca híbrida usa RRF de ranks, deduplica chunk ID e preserva ordem determinística.
- Falha temporária semântica em `preferred` retorna hits lexicais inalterados e observabilidade sanitizada.
- Falha de integridade/corrupção não é escondida por fallback.
- Todo hit carrega `source.Reference` do chunk canônico da geração ativa.
- Filtros de path/language têm mesma semântica em lexical e vector.
- Cancelamento pai interrompe provider/store; timeout semântico interno não cancela lexical.
- API key não entra em flag, banco, fingerprint, output, log ou erro.
- Conteúdo hard-denied nunca chega a parser, chunker, embedder, transporte, store ou logs; report usa somente agregados.
- Reindex/rollback entre snapshot e SQLite têm testes E2E; conteúdo de policy antiga é rejeitado e geração/WAL inativos são purgados.
- Avaliação Tivita usa apenas lexical/offline ou Ollama loopback e persiste somente métricas sanitizadas.
- `go test`, race, vet e build com CGO desabilitado passam.

## Migration, rollout e rollback

1. Entregar Task 0 e Tasks 1–9 sem mudar CLI default.
2. Entregar backend SQLite opt-in em Task 11.
3. Reindexar fixtures e repositórios de teste; comparar lexical antes/depois.
4. Habilitar `preferred` somente em ambiente de teste com endpoint controlado.
5. Medir latência, requests, tokens, degradação e qualidade conceitual.
6. Manter snapshot JSON de policy `scanner-v6` durante M2; `scanner-v5` ou anterior exige reindex seguro, sem migração in-place.
7. Em regressão, garantir snapshot atual e selecionar `--index-backend snapshot`; não apagar banco.
8. Tornar SQLite default somente em ADR futuro com evidência de uso e migração.

## Risks and mitigations

| Risco | Mitigação verificável |
| --- | --- |
| envio acidental de código | default off; config explícita; teste zero-request |
| vazamento de API key/input | env-only; erros sanitizados; testes de ausência |
| mistura de gerações | manifest transacional + reader ligado a um `GenerationID` |
| mudança silenciosa de modelo | fingerprint persistido + preflight antes de query |
| vetor inválido/corrupto | validação finita/dimensão/zero no adapter e store |
| fallback esconder corrupção | somente erros temporários tipados degradam |
| score híbrido instável | RRF fixo + tie-break determinístico |
| busca exata lenta | warning >20k chunks + benchmark; ANN exige decisão futura |
| SQLite afetar portabilidade | driver puro Go + gate `CGO_ENABLED=0` |
| duas indexações concorrentes | compare-and-swap de `BaseGeneration` |
| docs sugerirem Anthropic embeddings | referência oficial + revisão `rg` de vendor claims |
| scanner abrir segredo antes de excluir | policy por path antes de `Open` + taint test em todos sinks |
| avaliação vazar código profissional | go/no-go, provider externo proibido, output allowlist e revisão de segredo |
| rollback reexpor conteúdo antigo | `ScanPolicyVersion`, rejeição v1, purge de geração/WAL e reindex antes de rollback |

## Dependencies and decision gates

- Driver SQLite: tentar `modernc.org/sqlite@latest`, fixar versão resolvida; gate exige Go 1.24, CGO off e licença aceitável. Falha bloqueia Task 5 e exige ADR, sem fallback CGO implícito.
- Provider/modelo default: nenhum. Usuário sempre escolhe endpoint e modelo.
- Dimensão default: provider decide quando flag ausente; dimensão real da primeira resposta vira invariante da geração.
- RRF: `k=60`, pesos 1/1. Alteração depende de avaliação de retrieval, não config pública inicial.
- Performance: cosseno exato continua até evidência acima do limite operacional; não antecipar ANN.

## Open questions with safe defaults

- **Quando promover SQLite a default?** Após rollout opt-in e ADR separado; até lá snapshot é default de index e fallback de search.
- **Reutilizar embedding de chunk inalterado?** Não no M2. Futuro só reutiliza quando chunk ID e fingerprint forem idênticos.
- **Persistir cache de query?** Não. Cache futuro começa in-memory, pequeno e por fingerprint; nunca persiste texto.
- **Adicionar Voyage AI?** Somente quando prioridade real justificar adapter próprio; não usar nome Anthropic.
- **Adicionar filtros de símbolo/metadados?** Somente com caso de uso; path/language cobrem M2.
- **Escolher modelo recomendado?** Não. Qualidade/custo mudam; configuração explícita evita vendor default oculto.

## Explicit non-goals

- provider Anthropic de embeddings;
- implementação de geração/`ask`, MCP ou frontend;
- sincronização incremental, watch mode ou background indexing;
- ANN/HNSW, sqlite-vec, pgvector ou banco vetorial SaaS;
- reranking, query rewriting ou ensemble com mais de dois retrievers;
- autenticação de usuários, multi-tenant ou índice compartilhado;
- armazenamento de API key ou fonte fora do store local autorizado;
- alteração de `source.Reference`, `source.Citation` ou guardrails de resposta.
- envio de código dos repositórios Tivita a qualquer provider externo;
- inferir stack profissional sem inventário ou copiar fixture proprietária.
