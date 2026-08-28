# ADR 0002: embeddings e busca vetorial provider-agnostic

- **Status:** aceito para implementação do M2; revisão humana pendente
- **Data:** 2026-08-27

## Contexto

GoContext já possui chunks determinísticos com `source.Reference`, snapshot JSON local, busca lexical e comandos CLI de indexação e consulta. M2 precisa adicionar recuperação semântica sem tornar provider, protocolo HTTP ou mecanismo vetorial parte dos contratos centrais.

Busca lexical continua obrigatória: identificadores, caminhos e mensagens de erro frequentemente são melhor recuperados por correspondência exata. Busca vetorial é capacidade adicional. Falha ou ausência semântica não pode inutilizar consulta lexical nem produzir citação reconstruída a partir do índice vetorial.

## Premissas e defaults seguros

- nenhuma chamada de rede ocorre sem configuração explícita de embeddings;
- primeiro adapter usa protocolo HTTP compatível com `POST /v1/embeddings` da OpenAI;
- mesmo adapter cobre OpenAI e Ollama por `base URL`, modelo e credencial configuráveis;
- Anthropic não oferece modelo nativo de embeddings; futuro adapter Anthropic pertence ao seam de geração em `internal/answer`;
- Voyage AI pode ser considerado futuramente como provider de embeddings separado, nunca rotulado como Anthropic;
- repositórios do MVP são pequenos; busca vetorial exata por cosseno é preferível a ANN até medição justificar complexidade;
- `source.Chunk` permanece fonte canônica de texto e `source.Reference`.

## Opções consideradas

### A. SDK e adapter por vendor

Criar adapters distintos para OpenAI, Ollama e futuros providers reduziria código HTTP inicial. Porém OpenAI e Ollama já compartilham protocolo suficiente para embeddings, SDK introduziria tipos e defaults de vendor no núcleo, e cada adapter repetiria validação, batching, retry e proteção de segredos.

**Rejeitada:** baixa localidade e acoplamento prematuro.

### B. Ports públicos para cada etapa

Expor `Embedder`, `VectorStore`, `Indexer`, `Fusion`, `FallbackPolicy` e configuração completa daria máxima flexibilidade a callers.

**Rejeitada:** interface quase tão complexa quanto implementação. Callers passariam a coordenar lifecycle, compatibilidade de modelos e degradação.

### C. Módulos profundos com seams internos

Manter `search.Searcher` como interface de recuperação, adicionar módulo profundo de construção/publicação de índice e esconder batching, provider HTTP, persistência, validação vetorial e RRF atrás de interfaces pequenas.

**Escolhida:** maior leverage para CLI e futuros consumidores; mudanças de provider e store ficam locais.

## Decisão

### Módulos e dependências

```text
cmd/gocontext  (composition root)
    │
    ├── internal/index.Builder
    │      ├── internal/embedding.Embedder
    │      │      └── internal/embedding/openaicompat.Client
    │      └── internal/index.Store
    │             └── internal/index/sqlite.Store
    │
    └── internal/search.Searcher
           ├── internal/search/lexical.Searcher
           └── internal/search/hybrid.Searcher
                  └── internal/search/vector.Searcher
                         ├── internal/embedding.Embedder
                         └── índice SQLite somente leitura

internal/answer.Generator
    └── adapters LLM futuros, inclusive Anthropic
```

Regras de dependência:

- `source` não depende de embedding, index, search ou providers;
- `embedding` define valores provider-agnostic e seam externo mínimo;
- `index` depende de `source` e `embedding`, nunca de CLI ou provider concreto;
- `search/vector` e `search/hybrid` implementam `search.Searcher`;
- adapter SQLite implementa interfaces consumidoras de `index`, `lexical` e `vector` sem ser importado por elas;
- `cmd/gocontext` escolhe adapters, lê configuração e injeta dependências;
- `answer` consome resultados de busca, sem conhecer provider de embeddings.

### Seam de embeddings

Interface proposta:

```go
package embedding

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
	Embed(ctx context.Context, purpose Purpose, texts []string) (Batch, error)
}
```

`Fingerprint` identifica protocolo, endpoint normalizado, modelo, dimensões solicitadas, normalização e versão do contrato. Nunca inclui API key, query string ou userinfo. Perfil diferente exige reindexação completa. `Embed` preserva quantidade e ordem das entradas; adapter rejeita resposta com índice ausente/duplicado, dimensão inconsistente, `NaN`, infinito ou vetor zero.

`Purpose` permite futuro provider distinguir documento de consulta. Adapter OpenAI-compatible pode ignorá-lo quando protocolo não oferece campo equivalente.

### Geração de índice

`internal/index.Builder` oferece uma operação de alto nível. É tipo concreto; somente seams externos e de store usam interfaces Go:

```go
type Builder struct { /* dependencies and policy hidden */ }

func NewBuilder(config Config, embedder embedding.Embedder, store Store) (*Builder, error)
func (b *Builder) Replace(ctx context.Context, repositoryID string, corpus source.Corpus) (Report, error)
```

Builder:

1. valida repository ID, policy/revisão do corpus, chunks, referências e IDs duplicados;
2. usa `source.Corpus.Revision`, calculada uma vez sobre versão de schema + policy + IDs ordenados;
3. solicita embeddings; adapter executa e valida batches internamente;
4. valida profile, quantidade e dimensões do resultado agregado;
5. entrega geração completa ao store;
6. publica geração em uma transação SQLite;
7. retorna relatório sanitizado de modo, contagens, tokens e degradação.

Embeddings são calculados antes da transação. Escrita usa uma transação curta; readers continuam vendo geração anterior até commit. Cancelamento ou erro fatal mantém geração anterior ativa.

### Política semântica

Três modos:

- `off`: não cria nem consulta embeddings; comportamento lexical atual;
- `preferred`: semântica é tentada; indisponibilidade temporária grava geração lexical-only e produz aviso;
- `required`: qualquer falha semântica aborta publicação e preserva geração anterior.

Default é sempre `off`. Endpoint + modelo sem modo explícito não habilitam rede; configuração fica inerte. `preferred` ou `required` precisam ser escolhidos explicitamente junto de endpoint/modelo, inclusive quando valores vêm de env. Configuração parcial em modo habilitado é erro de uso.

Erros degradáveis em `preferred`: timeout interno, conexão indisponível, HTTP `408`, HTTP `429`, HTTP `5xx`, ausência de índice vetorial ou perfil incompatível. Erros fatais: cancelamento do contexto pai, falha lexical/store, resposta remota malformada, vetor inválido, corrupção local, chunk ID vetorial sem chunk canônico ou configuração insegura.

### Persistência e compatibilidade

Primeiro store persistente será SQLite local, usando driver Go sem CGO após teste de compatibilidade com Go 1.24. SQLite guarda:

O gate executado em 2026-08-28 resolveu a release corrente `modernc.org/sqlite v1.57.0` (tag `v1.57.0`, commit `6e86ac4a89e3f36359d1947e36355c469b18430c`, fonte `https://gitlab.com/cznic/sqlite`) via tooling de módulos Go. A licença da distribuição é BSD-3-Clause, permissiva. Essa release declara `go 1.25.0` e foi rejeitada porque o projeto exige compatibilidade com Go 1.24; por isso nenhum driver foi adotado, o teste `CGO_ENABLED=0` não prosseguiu e a implementação do store permanece bloqueada até decisão específica. Não houve substituição por driver CGO nem downgrade silencioso.

- gerações e manifest ativo por repositório;
- chunks canônicos e `source.Reference`;
- perfil, dimensão, métrica e revisão do corpus;
- vetores `float32` em BLOB com encoding versionado;
- metadados filtráveis: linguagem, caminho e símbolo.

Busca vetorial inicial faz scan exato e cosseno em Go. WAL permite readers concorrentes; publicações são transações curtas e writers são serializados por banco. ANN, extensão SQLite vetorial e banco externo ficam fora do M2.

Mudança de modelo, fingerprint ou dimensão cria nova geração. Vetores incompatíveis nunca são truncados, preenchidos ou comparados. Geração anterior fica disponível apenas durante publicação; depois de commit bem-sucedido, store remove corpus/vetores antigos e executa checkpoint/truncate do WAL. Rollback de produto usa snapshot atual reindexado pela mesma `ScanPolicyVersion`, nunca geração histórica contendo fonte.

Reindexação M2 sempre reconstrói geração completa. Schema preserva `Chunk.ID`, `CorpusRevision` e fingerprint para evolução futura: reuso incremental só será válido quando chunk ID e fingerprint forem idênticos. Mudança de perfil invalida todo reuso. Nenhuma otimização incremental entra sem teste provando que resultado equivale a rebuild completo.

Snapshot JSON atual permanece disponível durante rollout, mas snapshots anteriores à policy segura são incompatíveis e exigem reindex. SQLite entra como opt-in; `index` e `search` continuam usando snapshot por default. `auto` é escolha explícita até promoção em ADR futura, evitando que uma geração SQLite antiga se torne autoritativa depois de uma indexação snapshot comum.

Cada resultado do scanner carrega `ScanPolicyVersion`; composition root a transfere explicitamente para snapshot/corpus e stores nunca a inferem. Loader rejeita versão ausente, antiga ou diferente. Mudança em hard/built-in deny, precedência, detector de segredo/binário/UTF-8/gerado, regra de symlink/nested repo ou decisão pré-open exige nova versão e teste de rejeição da anterior.

Reader SQLite é ligado a um `GenerationID` e read transaction imutáveis por consulta, com `Close`: corpus lexical, vetores e hidratação usam mesma geração mesmo se manifest mudar concorrentemente. Cleanup pode apagar linhas logicamente, mas checkpoint/truncate espera readers fecharem.

### Filtros

`search.Query` recebe campo aditivo de zero value compatível:

```go
type Filter struct {
	PathPrefixes []string
	Languages    []source.Language
}
```

Categorias são combinadas por AND; valores de mesma categoria, por OR. Prefixos são caminhos relativos normalizados. Busca lexical e vetorial aplicam mesmos filtros antes de limitar candidatos. Mapas arbitrários e expressões específicas de SQL/provider não entram no contrato.

### Busca vetorial e hidratação

`search/vector.Searcher`:

1. valida query e filtro;
2. verifica se geração ativa aceita fingerprint configurado antes de chamar provider;
3. cria embedding de consulta;
4. busca candidatos exatos no SQLite;
5. hidrata candidato com `source.Chunk` canônico na mesma geração;
6. retorna `search.Hit` com `source.Reference` original e ordem determinística.

Similaridade bruta não atravessa seam híbrido. Dentro do searcher vetorial, cosseno é normalizado para `[0,1]` somente para manter invariante de `Hit.Score`. Empates usam caminho, linha inicial e chunk ID.

### Busca híbrida

`search/hybrid.Searcher` sempre recebe lexical como backend obrigatório e vetorial como backend opcional. Busca ambos concorrentemente com deadline semântico menor que deadline do caller. Cada backend busca janela:

```text
candidateLimit = min(max(query.Limit * 4, 20), 200)
```

Resultados são deduplicados por chunk ID e fundidos por RRF com `k = 60` e pesos `1.0`/`1.0`:

```text
raw(id)   = Σ weight_backend / (60 + rank_backend(id))
score(id) = raw(id) / Σ weight_backend / 61
```

Rank começa em 1. Empates usam maior quantidade de backends, melhor rank individual, caminho, linha e chunk ID. Scores lexicais e cosseno nunca são somados diretamente.

Fallback lexical retorna resultados lexicais sem reranking, preservando scores e ordem atuais. Degradação em configuração semântica produz evento sanitizado para CLI/observabilidade; ausência intencional (`off`) não produz aviso.

### Configuração e credenciais

Configuração inicial:

| Campo | Flag/env | Regra |
| --- | --- | --- |
| modo | `--semantic off|preferred|required` / `GOCONTEXT_SEMANTIC_MODE` | default `off` |
| endpoint | `--embedding-base-url` / `GOCONTEXT_EMBEDDING_BASE_URL` | HTTPS; HTTP somente IP loopback |
| modelo | `--embedding-model` / `GOCONTEXT_EMBEDDING_MODEL` | obrigatório quando habilitado |
| dimensões | `--embedding-dimensions` / `GOCONTEXT_EMBEDDING_DIMENSIONS` | opcional, positivo |
| credencial | `GOCONTEXT_EMBEDDING_API_KEY` | somente env; nunca flag ou arquivo |
| batch | `GOCONTEXT_EMBEDDING_BATCH_SIZE` | default 32, máximo 128 |
| concorrência | `GOCONTEXT_EMBEDDING_MAX_IN_FLIGHT` | default 2, máximo 8 |
| timeout | `GOCONTEXT_EMBEDDING_TIMEOUT` | default 30s |

Flags vencem env para valores não secretos. Endpoint não aceita userinfo, query ou fragment. Authorization só é enviado quando API key existe. Redirect é rejeitado. Corpo de erro remoto nunca entra em erro/log, mesmo sanitizado; somente status, categoria e headers allowlisted como `Retry-After` podem aparecer.

### Retry, limites e custo

- no máximo duas tentativas adicionais para erro temporário antes de deadline;
- retry somente em conexão temporária, `408`, `429` e `5xx`;
- após retries, conexão temporária, `408`, `429` e `5xx` compartilham erro tipado degradável; matriz única vale para adapter, builder e busca;
- respeitar `Retry-After` dentro do deadline;
- backoff exponencial com jitter e máximo de 2s;
- batch limitado simultaneamente por quantidade e 256 KiB de texto UTF-8;
- resposta HTTP limitada a 16 MiB;
- relatório mostra chunks, requests e tokens informados pelo provider, sem conteúdo;
- busca pode guardar cache somente em memória para embedding de consulta, com limite pequeno e chave pelo fingerprint; persistência de queries fica fora de escopo.

### Privacidade e observabilidade

- scanner aplica policy default-deny antes de abrir paths conhecidos: `.env`, `.env.*`, `.git/**`, `.github/**`, credenciais, chaves, certificados, metadata de automação, nested repos, symlinks, builds, caches, dependências, binários, gerados e arquivos grandes ficam fora do pipeline;
- nome de arquivo não prova ausência de segredo. Conteúdo permitido passa por detector conservador antes de virar `source.File`; segredo detectado é lido somente pelo scanner para classificação e não segue para parser/rede/store. Detector admite falsos negativos e positivos; por isso avaliação profissional continua proibida em provider externo;
- M2 não lê `.gitignore` nem metadata do repositório para relaxar policy. Regras futuras podem somente excluir mais; hard deny nunca é reabilitado;
- exclusões e falhas produzem contagens por categoria, sem paths, basenames, conteúdo ou exemplos;
- configuração semântica é opt-in explícito porque indexação pode enviar código a endpoint remoto;
- Ollama local usa mesmo adapter com URL de IP loopback e normalmente sem API key; transport desabilita proxy/redirect e valida peer loopback no modo profissional;
- logs nunca contêm fonte, consulta, vetor, prompt ou credencial;
- eventos permitidos: operação, repository hash, fingerprint abreviado, modelo sanitizado, contagens, latência, tentativas, status HTTP e motivo categorizado;
- `off` garante modo offline sem chamadas de rede;
- timeout/degradação produz aviso claro: busca ou índice lexical continua disponível;
- `source.Reference` é carregado somente do chunk canônico persistido.

### Validação em repositórios profissionais

Taba App, Tivita Backend e Tivita Web App serão avaliados somente depois dos gates de scanner e não vazamento. Inventário descobre linguagens, extensões, tamanhos e padrões reais; nenhuma stack é presumida. Resultados versionados contêm somente agregados e IDs opacos.

Para esses três repositórios, código permanece local: baseline lexical/offline e semântica opcional via Ollama loopback. Embeddings externos ficam proibidos mesmo que configuração genérica do produto aceite opt-in. Queries, gold sets, paths, símbolos e trechos não entram em documentação. Novo parser/chunker nasce de gap medido e usa fixture sintética ou suficientemente minimizada.

Excluir `.github/**` sacrifica contexto útil de workflows e configuração, mas é trade-off aceito por requisito explícito. Checklist go/no-go e matriz completa: [plano de validação Tivita](../plans/2026-08-27-tivita-professional-repository-validation.md).

## Consequências

### Positivas

- provider e wire protocol ficam concentrados em um adapter;
- OpenAI e Ollama não exigem código duplicado;
- lexical continua útil offline e durante indisponibilidade semântica;
- publicação atômica impede mistura de chunks e vetores de gerações diferentes;
- RRF evita calibrar scores incompatíveis;
- futuros stores e providers podem trocar implementação sem alterar callers;
- Anthropic futuro permanece isolado no seam correto de geração.

### Custos

- SQLite adiciona primeira dependência externa do módulo;
- reindexação guarda temporariamente geração anterior e nova;
- busca exata é O(n × dimensão), aceitável somente para repositórios pequenos;
- modo `preferred` exige evento de degradação para evitar fallback invisível;
- rollout precisa coexistir temporariamente com snapshots JSON.

## Não objetivos

- criar adapter de embeddings Anthropic;
- escolher modelo default ou enviar código automaticamente;
- oferecer vector database externo, ANN, reranker ou busca multi-repositório;
- atualizar embeddings incrementalmente no M2; reindexação completa é default;
- alterar validação de citações ou permitir referência proveniente de vetor;
- implementar geração/`ask`, MCP ou frontend.

## Referências de provider

- OpenAI, endpoint de embeddings: <https://developers.openai.com/api/reference/resources/embeddings>
- Ollama, compatibilidade OpenAI e `/v1/embeddings`: <https://docs.ollama.com/api/openai-compatibility>
- Anthropic, ausência de modelo próprio de embeddings e indicação de provider externo: <https://platform.claude.com/docs/en/build-with-claude/embeddings>

Referências consultadas em 2026-08-27. Capacidade de provider deve ser reverificada quando adapter futuro entrar no roadmap.
