# ADR 0002: embeddings e busca vetorial provider-agnostic

- **Status:** aceito; núcleo M2, prova taint e harness agregado implementados; priorização de novos parsers e revisão humana pendentes
- **Data:** 2026-08-27

## Contexto

GoContext possui chunks determinísticos com `source.Reference`, snapshot JSON local e busca lexical. O núcleo M2 adiciona recuperação semântica opt-in sem tornar provider, protocolo HTTP ou mecanismo vetorial parte dos contratos centrais.

Busca lexical continua obrigatória: identificadores, caminhos e mensagens de erro frequentemente são melhor recuperados por correspondência exata. Busca vetorial é capacidade adicional. Falha ou ausência semântica não pode inutilizar consulta lexical nem produzir citação reconstruída a partir do índice vetorial.

## Premissas e defaults seguros

- nenhuma chamada de rede ocorre sem configuração explícita de embeddings;
- primeiro adapter usa protocolo HTTP compatível com `POST /v1/embeddings` da OpenAI;
- mesmo adapter cobre OpenAI e Ollama por `base URL`, modelo e credencial configuráveis;
- Anthropic aparece apenas como possível provider futuro de geração no seam `internal/answer`;
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

Interface entregue:

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

func NewBuilder(store Store, embedder embedding.Embedder, config BuilderConfig) (*Builder, error)
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

O store SQLite local usa driver Go sem CGO após teste de compatibilidade com Go 1.24. SQLite guarda:

O gate executado em 2026-08-28 resolveu a release corrente `modernc.org/sqlite v1.57.0` (tag `v1.57.0`, commit `6e86ac4a89e3f36359d1947e36355c469b18430c`, fonte `https://gitlab.com/cznic/sqlite`) via tooling de módulos Go. A licença da distribuição é BSD-3-Clause, permissiva. Essa release declara `go 1.25.0` e foi rejeitada porque o projeto exige compatibilidade com Go 1.24; `v1.46.2` também já declara Go 1.25.

A decisão de compatibilidade fixa explicitamente `modernc.org/sqlite v1.46.1`, a versão mais nova cujo módulo declara `go 1.24.0`. O tag é de 2026-02-18, fonte `https://gitlab.com/cznic/sqlite`, commit `c777b9066dd7f97147b35345fce4c6a7a728c3ff`, checksum `h1:eFJ2ShBLIEnUWlLy12raN0Z1plqmFX9Qe3rjQTKt6sU=` e licença BSD-3-Clause. O source gate não encontrou `import "C"` ou diretivas `#cgo`; build do driver e suite completa passaram com `CGO_ENABLED=0`. O custo aceito é manter um pin anterior à release corrente e revisar manualmente correções upstream enquanto o baseline do projeto permanecer Go 1.24; aumentar o baseline ou trocar o pin exige nova decisão e repetição dos gates de licença, CGO e testes.

- gerações e manifest ativo por repositório;
- chunks canônicos e `source.Reference`;
- perfil, dimensão, métrica e revisão do corpus;
- vetores `float32` em BLOB com encoding versionado;
- metadados filtráveis: linguagem, caminho e símbolo.

O schema SQLite v2 também persiste `vector_digest`, calculado deterministicamente sobre chunk IDs, versão de encoding, dimensões e bytes BLOB em ordem de chunk ID, e `manifest_digest`. O manifesto provider-agnostic liga repository/generation ID, revisão e digest de conteúdo do corpus, `ScanPolicyVersion`, estado lexical/semântico, fingerprint/modelo, dimensões, métrica e digest vetorial. Lexical-only usa o digest canônico do conjunto vazio. O bind rejeita digests fora do SHA-256 hexadecimal estrito ou manifesto divergente; validação/busca recomputa o digest durante o único scan de linhas vetoriais. Isso cobre corrupção acidental ou por processo cooperante, inclusive outro vetor unitário do mesmo tamanho; um processo malicioso do mesmo UID que reescreva coerentemente dados e todos os digests permanece fora da fronteira. Caches schema v1 exigem reindexação completa e nunca são migrados in-place.

Busca vetorial inicial faz scan exato e cosseno em Go. WAL permite readers concorrentes; publicações são transações curtas e writers são serializados por banco. ANN, extensão SQLite vetorial e banco externo ficam fora do M2.

O CLI emite um aviso operacional fixo quando a geração SQLite fixada possui mais de 20.000 chunks. Exatamente 20.000 permanece silencioso. O reader carrega e valida chunks canônicos uma vez em cache imutável, entrega cópias defensivas e reutiliza esse corpus no caminho lexical e na hidratação vetorial. A busca exata consulta depois somente as linhas de vetores, preservando recomputação do digest, validação de encoding, dimensão, norma, linhas ausentes/duplicadas/órfãs, filtros, cancelamento e ordenação. Não há segunda consulta de chunks apenas para contar ou para o híbrido. O aviso aparece no máximo uma vez por operação, não altera ranking e não escolhe ANN automaticamente. Snapshot padrão permanece silencioso.

#### Evidência manual de busca exata

O benchmark `BenchmarkExactSearch10000x1536` constrói corpus e vetores sintéticos determinísticos fora do timer, usa somente o reader de cosseno exato, valida o primeiro hit estável, reporta alocações e não acessa rede. Cleanup também fica fora do timer. Ele não roda com testes normais nem constitui gate de CI.

Observação host-specific, não SLA: em 2026-08-28, macOS 26.5.2, darwin/arm64, Apple M4 Pro, Go 1.24.0 e `CGO_ENABLED=0`, o comando abaixo mediu uma iteração:

```bash
GOTOOLCHAIN=go1.24.0 CGO_ENABLED=0 go test ./internal/index/sqlite \
  -run '^$' -bench '^BenchmarkExactSearch10000x1536$' -benchtime=1x -count=1 -benchmem
```

```text
BenchmarkExactSearch10000x1536-12  1  111693417 ns/op  207107712 B/op  490284 allocs/op
```

Esse número inclui leitura, decoding, recomputação do digest vetorial, validação canônica e ranking do caminho exato no host descrito. O aviso conservador acima de 20.000 chunks é política operacional, não limite universal derivado desta única medição. ANN, reranker e store vetorial externo permanecem futuros.

Mudança de modelo, fingerprint ou dimensão cria nova geração. Vetores incompatíveis nunca são truncados, preenchidos ou comparados. Geração anterior fica disponível apenas durante publicação; depois de commit bem-sucedido, store remove corpus/vetores antigos e executa checkpoint/truncate do WAL. Rollback de produto usa snapshot atual reindexado pela mesma `ScanPolicyVersion`, nunca geração histórica contendo fonte.

Reindexação M2 sempre reconstrói geração completa. Schema preserva `Chunk.ID`, `CorpusRevision` e fingerprint para evolução futura: reuso incremental só será válido quando chunk ID e fingerprint forem idênticos. Mudança de perfil invalida todo reuso. Nenhuma otimização incremental entra sem teste provando que resultado equivale a rebuild completo.

Snapshot JSON atual permanece disponível durante rollout, mas snapshots anteriores à policy segura são incompatíveis e exigem reindex. SQLite entra como opt-in; `index` e `search` continuam usando snapshot por default. `auto` é escolha explícita até promoção em ADR futura, evitando que uma geração SQLite antiga se torne autoritativa depois de uma indexação snapshot comum.

### Rollout e rollback explícito

Sem flags/env, `index` e `search` permanecem byte-compatíveis com snapshot/semantic-off e não abrem SQLite ou rede. `search --index-backend auto` nunca cria store: usa snapshot validado quando banco/store/repositório está ausente e a geração SQLite ativa quando presente. SQLite, `auto` e semântica continuam opt-in.

Indexação SQLite bem-sucedida publica snapshot companheiro do mesmo corpus e marker ligado ao hash do repositório, `ScanPolicyVersion`, revisão do corpus e geração ativa. Em POSIX, o marker precisa permanecer owner-only. Publicação SQLite que falha preserva o par anterior; falha de snapshot/marker depois do commit deixa rollback indisponível e produz resultado explícito. Indexação snapshot invalida primeiro o marker e só então substitui o snapshot; se a remoção falha, não há novo commit nem relato de sucesso. Falha de manutenção pós-commit também é reportada, sem disfarçar a geração já publicada.

`search --index-backend snapshot` fornecido explicitamente é solicitação de rollback; ausência da flag continua sendo caminho de compatibilidade. Sem banco SQLite, ou sem geração ativa para o repositório pedido, o snapshot atual validado pode ser lido. Com geração ativa, um reader fixa a transação e valida conteúdo canônico, manifesto e vetores persistidos antes de autorizar; repositório ausente continua distinto de manifest apontando para geração inexistente, que é corrupção. O CLI lê de forma limitada, no-follow e read-only um marker regular, rejeita campos desconhecidos/trailing data e compara schema, hash, policy, revisão do snapshot, revisão SQLite e geração fixada. POSIX exige ausência de bits de grupo/outros. No Windows, rollback explícito marker-backed é fail-closed e indisponível no M2 antes de confiar no arquivo, pois `FileMode` sintético e ACL herdada não provam DACL owner-only. Snapshot implícito e SQLite continuam suportados; recuperação usa o default sem a flag explícita ou reindexação. Suporte futuro requer criação e validação owner-only testadas em runtime Windows. Ausência, permissões amplas quando verificáveis, corrupção, tamanho excessivo ou qualquer divergência falha com uma única categoria sanitizada de reindexação. Cancelamento e deadline preservam suas categorias próprias. SQLite existente corrupto, desconhecido ou inacessível também falha fechado; busca não cria, conserta nem mascara o store com cache antigo.

Recuperação é por reindexação completa. Em POSIX e store saudável, `index --index-backend sqlite --semantic off` recria geração, snapshot e marker coerentes antes do rollback. No Windows M2, o operador usa snapshot implícito ou reindexa no backend desejado; marker explícito permanece bloqueado. Em store SQLite corrupto ou schema v1, reindexação snapshot restaura somente o default implícito; rollback explícito permanece bloqueado até recuperação administrativa separada. Uma indexação snapshot padrão posterior remove a prontidão de rollback, e a busca padrão lê esse snapshot novo mesmo que uma geração SQLite antiga exista. Promoção de SQLite/`auto` a default é decisão futura.

Cada resultado do scanner carrega `ScanPolicyVersion`; composition root a transfere explicitamente para snapshot/corpus e stores nunca a inferem. Loader rejeita versão ausente, antiga ou diferente. Mudança em hard/built-in deny, precedência, detector de segredo/binário/UTF-8/gerado, regra de symlink/nested repo ou decisão pré-open exige nova versão e teste de rejeição da anterior.

`scanner-v5` adiciona deny case-insensitive, antes de metadata/open, para raízes de dependências/build/cache de alta confiança: `Pods`, `.gradle`, `.dart_tool`, `.pub-cache`, `DerivedData`, `Carthage`, `.cxx`, `.expo`, `.turbo`, `.nx`, `.parcel-cache`, `.vite` e `.bundle`. Nomes ambíguos como `packages` não são negados. A mesma versão amplia apenas buckets agregados sanitizados para famílias source/build, assets, imagens/fontes, archives e binários comuns; labels continuam extensões lowercase centralmente permitidas, com `<none>`/`<other>` para o restante. Nenhuma extensão nova se torna ingestível. Snapshot/corpus `scanner-v4` ou anterior exige reindex completo, sem migração in-place.

`scanner-v6` adiciona `.js` e `.jsx`, case-insensitive, à allowlist somente depois de todos os gates de path, tipo, tamanho e conteúdo já definidos. O line parser JavaScript é deliberadamente conservador: funções/classes precisam ser nomeadas; variáveis top-level só viram símbolo quando atribuídas diretamente a arrow/function expression; uma pilha lexical única, limitada e ordenada impede que declarações sem indentação dentro de blocos virem top-level e rejeita delimitadores cruzados; parâmetros precisam formar lista completa de identificadores simples, únicos e não reservados em módulo estrito. Contextos distintos para header/bloco de controle e bloco de statement no início da linha reconhecem regex após `)`/`}`, enquanto object literals provados por atribuição/agrupamento preservam divisão. `/` imediatamente após outra chave é ambíguo e torna irreversivelmente incerto o restante do arquivo, inclusive através de comentário/quebra de linha. Regiões JSX multilinha ficam opacas à descoberta; comentários, strings, templates e regex reconhecidas não alteram profundidade por seus delimitadores internos. Candidate só vira símbolo se a linha inteira continuar lexicalmente confiável. A validação adicional do arrow body preserva cancelamento/checkpoints lineares e visita tokens também em containers, templates/JSX substitutions, mas não em literais, comentários, regex, template raw ou texto/tag JSX. Arrow body precisa começar pelo subconjunto conservador documentado: unários suportados têm operando, `await` em qualquer profundidade exige arrow sintaticamente `async`, `yield` falha fechado, `function`/`class` têm prefixo completo e JSX conciso fecha na mesma linha; starters primários e containers reconhecidos permanecem aceitos. Default anônimo, forma multiline, destructuring/default de parâmetro, propriedade/chave homônima aos tokens proibidos, slash-after-brace ambíguo, template aninhado em substituição, JSX aninhado em expressão JSX, starter fora do subconjunto ou estado lexical ambíguo não recebem nomes inventados e podem causar falso negativo fail-closed. Não existe alegação de AST ou completude estrutural. JSON e demais extensões permanecem unsupported e não geram fallback cru. Snapshot ou corpus `scanner-v5` exige reindex completo pela categoria existente, sem migração in-place. A mudança foi validada apenas com fixtures sintéticas e taint renovado; qualquer delta de qualidade profissional depende de novo gate e nova execução local agregada.

Reader SQLite é ligado a um `GenerationID` e read transaction imutáveis por consulta, com `Close`: corpus lexical, vetores e hidratação usam mesma geração mesmo se manifest mudar concorrentemente. Cleanup pode apagar linhas logicamente, mas checkpoint/truncate espera readers fecharem.

### Filtros

`search.Query` recebe campo aditivo de zero value compatível:

```go
type Filter struct {
	PathPrefixes []string
	Languages    []source.Language
}
```

Categorias são combinadas por AND; valores de mesma categoria, por OR. Prefixos são caminhos relativos normalizados. As linguagens aceitas são `javascript`, `python` e `typescript`. Busca lexical e vetorial aplicam mesmos filtros antes de limitar candidatos. Mapas arbitrários e expressões específicas de SQL/provider não entram no contrato.

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
- a primeira descoberta profissional autorizada expôs somente agregados: a taxonomia de extensões estava incompleta e as contagens eram compatíveis com provável ruído de dependências/build/cache. Isso não afirma conteúdo oculto nem autoriza abrir esses diretórios;
- JSON permanece não suportado. Sua presença agregada não prova conteúdo seguro e não justifica habilitação cega de parser/ingestão;
- configuração semântica é opt-in explícito porque indexação pode enviar código a endpoint remoto;
- Ollama local usa mesmo adapter com URL de IP loopback e normalmente sem API key; transport desabilita proxy/redirect e valida peer loopback no modo profissional;
- logs nunca contêm fonte, consulta, vetor, prompt ou credencial;
- eventos permitidos: operação, repository hash, fingerprint abreviado, modelo sanitizado, contagens, latência, tentativas, status HTTP e motivo categorizado;
- `off` garante modo offline sem chamadas de rede;
- timeout/degradação produz aviso claro: busca ou índice lexical continua disponível;
- `source.Reference` é carregado somente do chunk canônico persistido.

### Validação em repositórios profissionais — evidência aggregate-only

Depois da prova taint da Task 13, a fase autorizada de inventário local executou somente o harness offline e persistiu sinal agregado com IDs opacos. A descoberta indicou taxonomia incompleta e provável ruído de dependências/build/cache, sem revelar paths, nomes, amostras ou conteúdo e sem justificar inferência de stack. `scanner-v6` foi reavaliado somente depois de novos checklists privados e revisão independente: três runs seriais `semantic off` terminaram `go`, sem provider/rede; 378 JavaScript foram incluídos e seis candidatos continuaram excluídos por segredo/tamanho. O parser conservador acrescentou apenas seis símbolos `function`, enquanto 372 arquivos adicionais ficaram sem símbolo. O ciclo confirma mudança agregada do corpus e citação exact-symbol geral em 1,0, mas a amostra limitada não identifica linguagem por query; fronteira de citação JavaScript continua sustentada por fixtures sintéticas, não por esse run profissional. Não há alegação de cobertura estrutural ou ganho de retrieval. Novos parsers continuam dependentes de fixture sintética, revisão de risco e decisão separada; JSON permanece unsupported.

O seam de gold set humano local já existe no harness: `gocontext eval inventory --gold-set ABS_PATH` exige arquivo privado owner-only fora da raiz, schema privado 1 estrito e referências que resolvam exatamente um chunk canônico permitido. A execução continua lexical/offline e não consulta provider. O relatório aggregate-only passa a schema 2, acrescenta nDCG graduado e acurácia de ausência sem publicar queries, casos, paths, julgamentos, hits ou IDs. Relatórios schema 1 anteriores permanecem artefatos históricos, sem migração in-place. Nenhum gold set profissional foi criado ou executado nesta mudança; categorias além de exact-symbol continuam `not-evaluated` nos baselines existentes e o conteúdo humano privado segue gate para alegação de qualidade ou semântica profissional. Parser estrutural e novos chunkers permanecem pendentes; só nascerão de gap medido e usarão fixture sintética ou suficientemente minimizada.

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

- criar adapters de embeddings específicos de vendor além do protocolo OpenAI-compatible;
- escolher modelo default ou enviar código automaticamente;
- oferecer vector database externo, ANN, reranker ou busca multi-repositório;
- atualizar embeddings incrementalmente no M2; reindexação completa é default;
- alterar validação de citações ou permitir referência proveniente de vetor;
- implementar geração/`ask`, MCP ou frontend.

## Referências de provider

- OpenAI, endpoint de embeddings: <https://developers.openai.com/api/reference/resources/embeddings>
- Ollama, compatibilidade OpenAI e `/v1/embeddings`: <https://docs.ollama.com/api/openai-compatibility>

Referências consultadas em 2026-08-27. Capacidade de provider deve ser reverificada quando adapter futuro entrar no roadmap.
