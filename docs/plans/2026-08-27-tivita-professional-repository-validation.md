# Validação local em repositórios profissionais Tivita

**Objetivo:** validar cobertura, qualidade de recuperação, segurança e custo operacional do GoContext nos repositórios Taba App, Tivita Backend e Tivita Web App sem copiar código, caminhos, consultas ou segredos para documentação, logs ou serviços externos.

**Dependência:** executar somente depois do gate de segurança do scanner e do teste de não vazamento ponta a ponta definidos no [plano de embeddings e busca vetorial](2026-08-27-provider-agnostic-embeddings-vector-search.md). Ausência de path autorizado ou falha em qualquer item do checklist é `no-go`, não motivo para relaxar policy.

## Regras invariantes

- Descobrir linguagens, extensões, tamanhos e padrões; não presumir stack de nenhum repositório.
- Código profissional fica local. Para estes três repositórios, embeddings externos são proibidos mesmo que produto genérico ofereça opt-in.
- O harness profissional aggregate-only entregue (`gocontext eval inventory`) executa somente lexical/offline com `semantic off`; não compõe SQLite vetorial, busca híbrida ou Ollama.
- O caminho semântico opt-in do produto aceita OpenAI-compatible apontando para Ollama em loopback, mas não constitui um harness profissional agregado. Uma comparação profissional futura só poderá usar esse modo depois dos gates humanos e de privacidade abaixo.
- `.env`, `.env.*`, `.git/**`, `.github/**` e demais hard denies nunca são abertos. Exclusão integral de `.github/**` perde contexto útil de workflows/configuração, mas atende requisito explícito.
- Relatórios persistem somente contagens, percentuais, histogramas, IDs opacos de query e achados categóricos. Nunca persistem conteúdo, símbolo, query, path, basename, URL interna, nome de pessoa ou amostra de código.
- Gold sets, queries e ranks por item ficam em workspace local não sincronizado: diretório `0700`, arquivos `0600`, fora do Git, com retenção até revisão e remoção depois da agregação. Documentação recebe somente categoria e métricas agregadas.
- Fixtures de regressão para novo suporte são sintéticas ou minimizadas até não reproduzirem código proprietário.

## Go/no-go por repositório

Repetir checklist para Taba App, Tivita Backend e Tivita Web App. Guardar resultado local com identificador opaco; relatório versionado registra apenas `go`/`no-go` e categorias de bloqueio.

- [ ] proprietário autorizou acesso local para avaliação;
- [ ] raiz canônica foi fornecida explicitamente e existe em filesystem local;
- [ ] processo roda sem escrita na raiz e com cache fora do repositório;
- [ ] testes de hard deny, symlink, traversal, nested repo, binário, arquivo grande e redaction estão verdes;
- [ ] teste taint confirma que bytes excluídos não chegam a parser, chunker, embedder, network transport, store, stdout, stderr ou logs;
- [ ] `--semantic off` foi fixado para inventário e baseline lexical;
- [ ] nenhuma variável/flag aponta para endpoint remoto;
- [ ] eventual avaliação semântica aponta para loopback e provider local verificado;
- [ ] transport usa IP loopback, sem DNS/proxy/redirect, e valida peer conectado;
- [ ] provider local está configurado sem telemetry, request logging ou retenção de payload;
- [ ] relatório/output está configurado para agregados, permissão `0600` e sem paths;
- [ ] orçamento de tempo, disco e tamanho máximo está definido;
- [ ] rollback consiste em descartar cache local da avaliação, sem tocar repositório;
- [ ] operador revisou preview somente de contagens de inclusão/exclusão.

Qualquer item falso bloqueia scan. Não existe modo `force` para hard deny.

## Fase A — inventário de capacidades

Inventário usa scanner endurecido e único traversal. Conteúdo permitido pode ser lido pelo pipeline para classificar suporte, mas output é agregado. Não existe segunda caminhada do evaluator. Arquivos excluídos contribuem somente com razão e contagem; extensão sensível não é reportada individualmente.

Preencher matriz separadamente, após descoberta:

| Dimensão | Taba App | Tivita Backend | Tivita Web App |
| --- | --- | --- | --- |
| arquivos/bytes elegíveis | descobrir | descobrir | descobrir |
| arquivos/bytes incluídos | descobrir | descobrir | descobrir |
| exclusões por categoria | descobrir | descobrir | descobrir |
| linguagens suportadas | descobrir | descobrir | descobrir |
| extensões não suportadas | descobrir | descobrir | descobrir |
| faixas de tamanho de arquivo | descobrir | descobrir | descobrir |
| arquivos sem símbolos estruturais | descobrir | descobrir | descobrir |
| nested repos/symlinks detectados | descobrir | descobrir | descobrir |
| padrões/frameworks relevantes | descobrir e categorizar | descobrir e categorizar | descobrir e categorizar |
| chunks e bytes indexados | descobrir | descobrir | descobrir |
| tempo e pico de memória | medir | medir | medir |

`padrões/frameworks` usa taxonomia genérica, por exemplo componente UI, rota, handler, modelo, migração, teste, configuração permitida e código gerado. Para extensão/linguagem não suportada, valor é `unknown/not evaluated`: não inferir padrão sem abrir conteúdo e não criar segunda caminhada. Classificação manual futura pode ocorrer localmente sob novo gate e reportar somente agregado. Nenhum nome concreto entra no relatório.

A primeira execução autorizada desta fase produziu somente agregados e mostrou duas limitações do sinal: taxonomia incompleta de extensões não suportadas e contagens compatíveis com provável ruído de dependências/build/cache. Essa leitura é uma hipótese sobre as categorias, não uma afirmação sobre conteúdo oculto; nenhum path, basename, exemplo ou byte de diretório negado foi usado. `scanner-v5` passa a negar antes de metadata/open os roots de alta confiança `Pods`, `.gradle`, `.dart_tool`, `.pub-cache`, `DerivedData`, `Carthage`, `.cxx`, `.expo`, `.turbo`, `.nx`, `.parcel-cache`, `.vite` e `.bundle`, sem negar o nome ambíguo `packages`, e amplia apenas buckets lowercase agregados para famílias não sensíveis.

Esses buckets continuam report-only: não habilitam parser ou ingestão. JSON permanece `unknown/not evaluated` e unsupported; não será habilitado cegamente porque configuração JSON pode incluir credenciais ou outros dados sensíveis. Qualquer suporte futuro exige plano próprio, fixtures sintéticas e novo gate de segurança. Snapshots/corpora anteriores a `scanner-v5` exigem reindex completo e nunca são migrados no lugar.

Task 14D implementa depois desse baseline um tracer bullet `scanner-v6` para `.js`/`.jsx` usando somente fixtures sintéticas. A ampliação não autoriza automaticamente nova leitura profissional e não transforma o baseline `scanner-v5` em evidência de qualidade JavaScript. Antes de repetir qualquer raiz, o operador deve reindexar explicitamente, repetir taint/no-egress/hard-deny/secret scan e preencher novamente o checklist go/no-go individual. JSON permanece `unknown/not evaluated` e unsupported. Relatórios continuam aggregate-only, sem registros por query.

Checkpoint 2026-08-28: depois da revisão independente do tracer bullet, esses
gates e os três checklists foram renovados. Três runs seriais `semantic off`
terminaram `go`, sem provider/rede. Em agregado, 378 arquivos JavaScript foram
incluídos; seis candidatos adicionais foram excluídos por segredo/tamanho. A
variação estrutural foi de seis símbolos `function` e 372 arquivos sem símbolo,
evidência de inclusão/chunking agregado, mas insuficiente para alegar cobertura
de linguagem ou citação específica de JavaScript. Essa fronteira foi validada
separadamente por fixtures sintéticas. Nenhum nome, root, path, query ou hit foi
versionado.

## Fase B — benchmark de recuperação

Executar primeiro baseline lexical. A comparação vetorial/híbrida profissional não foi entregue nem executada: permanece bloqueada até existir gold set humano válido e um gate verificado de Ollama em IP loopback, sem DNS/proxy/redirect, telemetry, request logging ou retenção de payload. Depois desses gates, ainda será necessário entregar e revisar uma composição aggregate-only que use exatamente o mesmo corpus permitido; o caminho Ollama genérico do produto, sozinho, não satisfaz esse requisito.

### Matriz de consultas

Cada repositório recebe IDs opacos e quantidade comparável por categoria:

| Categoria | Intenção | Critério principal |
| --- | --- | --- |
| símbolo exato | localizar definição conhecida | hit relevante no top 3 |
| mensagem de erro | localizar origem literal | lexical não regride |
| caminho/configuração permitida | encontrar responsabilidade por convenção | referência válida |
| conceito | localizar implementação sem mesmo vocabulário | ganho híbrido sobre lexical |
| fluxo cross-layer | recuperar evidências de duas camadas | cobertura no top 10 |
| padrão de framework | achar construção idiomática | parser/chunker não fragmenta evidência |
| evidência negativa | consulta sem suporte no corpus | nenhuma citação inventada |

Mínimo inicial: 5 queries distintas por categoria aplicável e repositório. `ParseGoldSet` rejeita qualquer categoria fornecida com menos de cinco casos antes de criar o pipeline. O avaliador marca relevância localmente. Dados por query — ID, texto, ranks, booleanos e hits — permanecem somente no workspace privado; resultado versionado agrega por categoria e repositório.

### Métricas

- cobertura elegível e taxa de arquivos/chunks sem suporte;
- Recall@5, Recall@10, MRR@10 e nDCG@10 por categoria;
- validade de `source.Reference`: 100% dos hits apontam para chunk canônico permitido;
- taxa de resposta/fallback lexical e motivo categórico;
- tempo de scan, indexação, p50/p95 de consulta e pico de memória;
- número de requests/tokens do provider local, sem payload;
- determinismo: mesma geração e mesma query produzem mesma ordem;
- segurança: zero bytes excluídos em qualquer sink instrumentado.

Não definir ganho semântico universal antes do primeiro baseline. Critério de promoção vem dos dados; regressão lexical em símbolo/erro é bloqueante.

O baseline `scanner-v6` preservou, na precisão publicada, Recall@5/10, MRR@10,
nDCG@10, citações e determinismo do baseline `scanner-v5`. Só a categoria
exact-symbol automática foi avaliada; conceito, fluxo cross-layer, framework,
mensagem de erro, configuração/path e evidência negativa permanecem
`not-evaluated`. Gold set humano privado é gate obrigatório antes de qualquer
afirmação de qualidade ou execução semântica profissional.

## Fase C — priorização de suporte

Para cada linguagem, formato ou padrão não suportado, calcular prioridade:

```text
prioridade = cobertura_de_bytes * frequência_de_queries * impacto_de_retrieval
             / (complexidade + risco_de_parser)
```

Agrupar achados em:

1. allowlist/scanner já parseável com segurança;
2. adapter de linguagem para parser existente;
3. parser estrutural novo;
4. chunker especializado por padrão/framework;
5. formato deliberadamente não indexável por segurança ou baixo valor.

Cada adição exige plano/commit próprio e estes critérios:

- fixture sintética/minimizada cobrindo declaração, nesting, multiline e arquivo malformado;
- parser preserva linha, linguagem e path relativos em `source.Reference`;
- chunk IDs são determinísticos e mudança de versão é explícita;
- arquivo não suportado falha ou é excluído com razão agregada, nunca vira texto cru silenciosamente;
- hard deny roda antes do novo parser/chunker e não pode ser substituído;
- testes de cancelamento, limite, binário e entrada não confiável;
- teste end-to-end `scan → parse → chunk → lexical` e, quando aplicável, busca híbrida local;
- benchmark mostra ganho na categoria alvo sem regressão bloqueante nas categorias existentes.

## Saída sanitizada

Formato versionado atual, ilustrado somente com agregados vazios e sem registros
por query. Todas as sete categorias têm o mesmo conjunto completo de campos; em
`negative_evidence`, os quatro campos de qualidade positiva permanecem zero:

```json
{
  "schema": 2,
  "repository": "repo-ab",
  "decision": "go",
  "blockers": {},
  "inventory": {
    "eligible_files": 0,
    "eligible_bytes": 0,
    "included_files": 0,
    "included_bytes": 0,
    "excluded_by_category": {},
    "supported_languages": {},
    "supported_extensions": {},
    "unsupported_extensions": {},
    "size_bands": {},
    "symbol_kinds": {},
    "files_without_symbols": 0,
    "pattern_buckets": {},
    "chunks": 0,
    "indexed_bytes": 0
  },
  "retrieval": {
    "status": "not-evaluated",
    "categories": {
      "exact_symbol": {"status":"not-evaluated","query_count":0,"recall_at_5":0,"recall_at_10":0,"mrr_at_10":0,"ndcg_at_10":0,"no_evidence_accuracy":0},
      "concept": {"status":"not-evaluated","query_count":0,"recall_at_5":0,"recall_at_10":0,"mrr_at_10":0,"ndcg_at_10":0,"no_evidence_accuracy":0},
      "cross_layer": {"status":"not-evaluated","query_count":0,"recall_at_5":0,"recall_at_10":0,"mrr_at_10":0,"ndcg_at_10":0,"no_evidence_accuracy":0},
      "framework": {"status":"not-evaluated","query_count":0,"recall_at_5":0,"recall_at_10":0,"mrr_at_10":0,"ndcg_at_10":0,"no_evidence_accuracy":0},
      "error_message": {"status":"not-evaluated","query_count":0,"recall_at_5":0,"recall_at_10":0,"mrr_at_10":0,"ndcg_at_10":0,"no_evidence_accuracy":0},
      "configuration_path": {"status":"not-evaluated","query_count":0,"recall_at_5":0,"recall_at_10":0,"mrr_at_10":0,"ndcg_at_10":0,"no_evidence_accuracy":0},
      "negative_evidence": {"status":"not-evaluated","query_count":0,"recall_at_5":0,"recall_at_10":0,"mrr_at_10":0,"ndcg_at_10":0,"no_evidence_accuracy":0}
    },
    "citation_validity": 0,
    "deterministic_order_rate": 0,
    "fallback_count": 0,
    "fallback_reason": "zero"
  },
  "performance": {
    "scan_milliseconds": 0,
    "index_milliseconds": 0,
    "query_p50_microseconds": 0,
    "query_p95_microseconds": 0,
    "peak_heap_bytes_approximate": 0
  },
  "capability_gaps": {
    "concept": "not-evaluated",
    "cross_layer": "not-evaluated",
    "framework": "not-evaluated",
    "error_message": "not-evaluated",
    "configuration_path": "not-evaluated",
    "negative_evidence": "not-evaluated"
  },
  "limitations": {
    "heap_peak_approximate": 1,
    "process_local_latency": 1,
    "automatic_exact_symbol_only": 1,
    "frameworks_not_inferred": 1
  }
}
```

Antes de versionar qualquer resultado, executar scanner de segredos no arquivo e revisão manual para confirmar ausência de paths, conteúdo e identificadores internos.

O comando atual emite schema 2. Resultados schema 1 anteriores são históricos e
não são migrados no lugar. Um input humano opcional usa schema privado 1, fica
somente em workspace local owner-only e entra por `--gold-set ABS_PATH` depois
dos gates de output/root/checklist. O harness resolve cada julgamento para um
único chunk canônico já permitido; o relatório continua sem query, caso, path,
referência, julgamento, hit, rank ou chunk ID. Nenhum conjunto humano foi
autorizado, criado ou executado durante a implementação desse seam.

## Critérios de conclusão

- três checklists registrados localmente; `no-go` permanece resultado válido;
- inventário reproduzível sem assumir linguagem ou framework;
- baseline lexical registrado; comparação híbrida local permanece pendente até os gates e a composição aggregate-only descritos na Fase B;
- nenhum acesso a hard deny nem egress externo;
- gaps ordenados por impacto e convertidos em planos pequenos com fixtures não proprietárias;
- relatório Git contém somente agregados sanitizados e limitações explícitas.
