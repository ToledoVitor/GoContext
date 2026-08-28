# Arquitetura proposta

## Escopo

GoContext começa como aplicação local, de processo único e orientada a CLI. O núcleo Go coordena ingestão, recuperação e resposta. React/TypeScript poderá consumir uma API local depois que o fluxo CLI provar utilidade; não participa do MVP de fim de semana.

## Fluxo de dados

1. **Scanner** percorre somente uma raiz autorizada, aplica regras de exclusão, rejeita binários e mantém caminhos relativos.
2. **Line parser preliminar** descobre um subconjunto conservador de declarações top-level de JavaScript/JSX, Python e TypeScript com intervalos de linhas; parser estrutural permanece pendente.
3. **Chunker atual** recorta por limites dessas declarações; divisão de símbolos grandes com sobreposição permanece futura.
4. **Store** grava chunks e metadados em snapshot consistente.
5. **Busca lexical** favorece nomes exatos, identificadores e mensagens de erro.
6. **Busca vetorial** opt-in usa embeddings provider-agnostic e recupera conceitos expressos com vocabulário diferente.
7. **Fusão híbrida** combina rankings, inicialmente por Reciprocal Rank Fusion, evitando calibrar escalas incompatíveis.
8. **Gerador futuro** receberá somente melhores chunks e pergunta do usuário.
9. **Guardrail futuro** rejeitará citações inexistentes ou fora da evidência recuperada.
10. **CLI atual / MCP futuro** apresenta ou apresentará referências `caminho:linha-inicial-linha-final`.

Cada etapa preserva `source.Reference`. Citação não é reconstruída a partir de texto gerado.

## Módulos Go

| Módulo | Responsabilidade entregue | Implementação futura |
| --- | --- | --- |
| `cmd/gocontext` | composition root dos comandos `index` e `search`, configuração e avisos | `ask` e MCP |
| `internal/source` | fatos, IDs, revisão de corpus e `source.Reference` canônico | novas linguagens sem mudar proveniência |
| `internal/ingest` | scanner default-deny, line parser preliminar e chunker | parser estrutural guiado pela Task 14 |
| `internal/embedding` | seam provider-agnostic e adapter HTTP OpenAI-compatible | adapters adicionais somente quando medidos |
| `internal/index` | builder de geração completa e publicação SQLite | reuso incremental equivalente a rebuild |
| `internal/search` | lexical, vetor exato e híbrido RRF com fallback | ANN e reranker após medição |
| `internal/answer` | contratos para geração e validação | adapters LLM e guardrails determinísticos no M3 |

Após a fundação M0, novos pacotes devem nascer junto de comportamento executável. Não haverá pastas vazias para `mcp`, `storage`, `embeddings` ou `web`.

## Limites e dependências

- `source` não depende de outros pacotes internos.
- `ingest` e `search` dependem de `source`.
- `answer` depende de `search` e `source`.
- `cmd/gocontext` é o composition root; implementações concretas não são importadas pelos tipos centrais.
- Interfaces descrevem necessidades do consumidor. Adapters de SQLite, parser, embedding e LLM ficam nas bordas.

## Persistência e busca

O snapshot JSON permanece backend padrão e SQLite versionado é opt-in. SQLite publica gerações atômicas de chunks e vetores; o schema v2 persiste um digest determinístico dos registros vetoriais ordenados e um manifesto que liga repositório, geração, revisão/conteúdo do corpus, policy, profile/modelo, dimensão, métrica e digest vetorial. O estado lexical-only usa o digest canônico do conjunto vetorial vazio. Bind valida formato e manifesto; a leitura valida os bytes fixados sem nova consulta de chunks. Caches v1 falham com reindex-required e não recebem migração in-place. O primeiro mecanismo vetorial entregue faz scan exato e cosseno em Go. Extensão vetorial, ANN e banco vetorial externo só entram após medição. O ranking híbrido recebe candidatos independentes e combina posições por RRF, não scores crus.

Nenhum índice externo é necessário. Reindexação completa de repositórios pequenos é aceitável antes de otimizar atualizações incrementais.

`internal/ingest/localstore` oferece o caminho de compatibilidade offline. Cada repositório recebe snapshot JSON versionado, identificado no disco pelo SHA-256 do repository ID. Escrita usa arquivo temporário, `fsync` e rename atômico; arquivos usam permissão `0600`. Leitura valida versão, policy, revisão, repository ID, chunks duplicados, referências e limite de 64 MiB. Esse adapter não oferece busca; `internal/search/lexical` consome seu seam mínimo de loader.

`internal/search/lexical` implementa recuperação lexical de primeira classe tanto sobre snapshot quanto sobre reader SQLite fixado a uma geração. Consulta e campos de chunk são tokenizados em Unicode, com separação de `camelCase`, `snake_case` e pontuação. Score normalizado combina presença do termo em texto (`0.6`), símbolo (`0.3`) e caminho (`0.1`); empates usam caminho, linha e chunk ID. Não há índice invertido, frequência de termos ou BM25 nesta fase.

## Embeddings e recuperação híbrida entregues no M2

- `internal/embedding` define profile, purpose, vector e seam mínimo `Embedder`.
- `internal/embedding/openaicompat` é o adapter concreto; OpenAI-compatible remoto e Ollama diferem por configuração, não por contrato.
- `internal/index` esconde batching, revisão de corpus e publicação atômica de geração completa.
- `internal/index/sqlite` guarda chunks canônicos, referências, vetores e manifest ativo.
- `internal/search/vector` cria embedding de query e retorna somente chunks da geração canônica fixada.
- `internal/search/hybrid` combina lexical e vector por RRF com fallback lexical observável.
- `cmd/gocontext` é o único composition root de adapters, configuração, credenciais e disclosure de egress.

Sem modo `preferred|required` explícito, nenhuma chamada de rede ocorre, mesmo se endpoint/modelo estiverem no ambiente. Modo `preferred` degrada somente erros temporários tipados para lexical e emite aviso sanitizado; corrupção, configuração inválida e cancelamento continuam erros. Modelo, dimensão ou fingerprint diferente exige rebuild completo. `source.Reference` sempre vem do chunk canônico, nunca do vetor. Detalhes: [ADR 0002](decisions/0002-embeddings-vector-search.md).

### Seleção, rollout e rollback

- Sem flags/env, `index` e `search` usam snapshot/semantic-off e não abrem SQLite ou rede.
- `--index-backend sqlite` e `auto` são opt-in. `auto` lê SQLite ativo quando presente e snapshot validado quando banco/store/repositório está ausente; busca não cria store.
- Uma indexação SQLite bem-sucedida publica snapshot companheiro e marker ligados à mesma policy, revisão e geração ativa. POSIX exige marker owner-only.
- `--index-backend snapshot` explícito é rollback. Com geração SQLite ativa, mantém uma transação de leitura fixada, valida chunks, vetores e digests persistidos e então lê marker limitado, no-follow, regular e de schema estrito. No Windows, esse rollback explícito é fail-closed/indisponível no M2 antes de confiar no marker: ACL herdada não substitui prova owner-only. O caminho snapshot implícito e SQLite seguem suportados; o operador omite a flag explícita ou reindexa. Uma futura liberação requer criação e validação de DACL owner-only testadas no host Windows. Qualquer divergência ou SQLite desconhecido/corrupto falha com categoria sanitizada de reindexação; cancelamento e deadline preservam suas categorias.
- Uma indexação snapshot padrão posterior remove a prontidão de rollback antes de substituir o snapshot. Falha nessa remoção é reportada de forma sanitizada, sem commit nem mensagem de sucesso. A busca padrão lê o snapshot novo após sucesso; `auto` continua sendo a escolha explícita que pode ler a geração SQLite anterior.

Recuperação é reindex-first: um store SQLite saudável recebe nova indexação completa para recriar o par; SQLite corrupto ou schema v1 não é mascarado nem migrado por cache antigo. O operador pode reindexar snapshot para restaurar o caminho padrão, mas rollback explícito continua bloqueado até recuperação administrativa do store — e permanece indisponível no Windows M2. Promoção de SQLite a default exige ADR futura.

### Escala exata

O reader SQLite faz cosseno exato O(n × dimensão), sem ANN implícito e sem alterar ranking. A primeira carga fixa, valida e guarda internamente uma cópia imutável dos chunks canônicos; consumidores recebem cópias defensivas e `Close` libera a referência ao cache. O híbrido reutiliza esse cache no caminho vetorial, que consulta somente linhas de vetores e ainda verifica digest persistido, ausências, duplicatas, órfãos, encoding, dimensões e norma. Assim, chunks são consultados uma vez por reader, a mesma carga alimenta o lexical e o aviso acima de 20.000, e exatamente 20.000 permanece silencioso. O backend snapshot padrão não emite esse diagnóstico. Benchmark manual e evidência host-specific ficam no [ADR 0002](decisions/0002-embeddings-vector-search.md).

## Parsing e chunking

JavaScript/JSX, Python e TypeScript exigem parser estrutural. Tree-sitter é a direção preferida; binding Go e impacto de CGO precisam ser validados no marco de ingestão.

`internal/ingest/lineparser` oferece descoberta preliminar enquanto esse spike não ocorre. Reconhece declarações top-level comuns de Python (`def`, `async def`, `class`) e TypeScript (`function`, `class`, `interface`, `type`, `enum`). Para JavaScript/JSX reconhece somente funções nomeadas, inclusive `async` e geradoras, classes nomeadas e uma variável top-level `const`/`let`/`var` atribuída diretamente a arrow function ou function expression, com `export`/`async` apenas nas formas previstas pelo contrato. Uma passagem lexical linear acompanha profundidade de blocos/parênteses/colchetes, distingue fechamento de header de controle de fechamento de expressão para decidir regex e mantém chaves, comentários, strings, templates e regex reconhecidos fora da decisão de top-level. Regiões JSX, inclusive tags multilinha, atributos e substituições simples, permanecem opacas para descoberta de declaração. Parâmetros aceitos são apenas identificadores ASCII simples, únicos e não reservados em módulo estrito; lista incompleta, segmento vazio, destructuring, default, duplicata e nome reservado falham fechado. Arrow concisa precisa começar com um starter de expressão conservador, enquanto block body começa com `{`. A linha só pode produzir símbolo se a passagem completa continuar lexicalmente confiável. Preserva a linha declaratória exata e não inventa nome para default anônimo.

Não interpreta AST, bodies, métodos, símbolos aninhados/indentados, declarações multilinha ou toda a gramática ECMAScript; arrow functions TypeScript continuam fora desse recorte. Formas lexicamente ambíguas ou não modeladas, incluindo template aninhado dentro de substituição, JSX aninhado dentro de expressão JSX, lista com default/destructuring e trailing comma, podem suprimir símbolos válidos do restante do arquivo em vez de arriscar boundary inventado. Esses falsos negativos conservadores e as demais limitações impedem apresentar o parser atual como estrutural ou completo.

Política inicial de chunking:

- função, método, classe ou declaração exportada forma unidade natural;
- assinatura e comentários de documentação acompanham corpo;
- arquivo sem símbolos usa um chunk de arquivo no recorte inicial; divisão por linhas entra junto do orçamento de tokens;
- chunk registra linguagem, símbolo, caminho e linhas;
- tamanho máximo será medido em tokens do modelo de embedding escolhido.

`internal/ingest/symbolchunker` implementa recorte inicial dessa política. Cada símbolo começa na linha declaratória e termina antes da próxima declaração top-level; linhas vazias finais são removidas. Arquivo não vazio sem símbolos gera um chunk de arquivo. IDs usam SHA-256 sobre versão, origem, linguagem, símbolo e texto, mantendo reindexações determinísticas. Divisão de símbolos grandes por orçamento de tokens permanece pendente.

## Scanner filesystem atual

`internal/ingest/filesystem` implementa `ingest.Scanner` usando `os.Root`, disponível no Go 1.24, para manter leituras confinadas à raiz autorizada. Scanner:

- inclui `.js`, `.jsx`, `.py`, `.ts` e `.tsx`, com extensão case-insensitive;
- retorna caminhos relativos normalizados e intervalos de linhas;
- não segue symlinks;
- exclui diretórios de VCS, dependências, ambientes virtuais, caches, cobertura e builds;
- ignora arquivos não regulares, binários com byte NUL e fontes acima de 1 MiB;
- respeita cancelamento de contexto.

O scanner já aplica policy default-deny auditável antes de qualquer avaliação profissional. Paths reconhecidamente sensíveis são recusados antes de `Open`: `.env`, `.env.*`, `.git/**`, `.github/**`, metadata/automação, credenciais/chaves/certificados, nested repos, symlinks, dependências e caches. Arquivos de nome permitido são lidos com limite para classificar binário, UTF-8, gerado, tamanho e padrões conservadores de segredo; itens detectados não viram `source.File` nem atravessam parser/rede/store. Report expõe somente contagens por categoria.

`scanner-v6` representa a ampliação de elegibilidade JavaScript/JSX. Snapshot ou geração SQLite `scanner-v5` falha com a categoria sanitizada de reindexação e nunca recebe migração in-place. JSON continua fora da allowlist e não existe fallback cru para arquivo não suportado.

M2 deliberadamente não lê `.gitignore`: arquivo controlado pelo próprio repositório não pode relaxar hard deny, e leitura ampla de metadata aumenta superfície. Futuro suporte será deny-only e exigirá decisão explícita. Exclusão integral de `.github/**` perde contexto potencialmente útil, trade-off aceito por segurança.

## Guardrails

- Canonicalizar raiz autorizada e impedir escape por `..` ou symlink.
- Aplicar hard deny antes de abrir path; regras adicionais somente excluem e nunca reabilitam item protegido.
- Ignorar `.git`, `.github`, metadata de automação, credenciais, dependências, artefatos, symlinks, nested repos, binários e arquivos acima do limite.
- Não registrar conteúdo-fonte, prompts, segredos ou embeddings por padrão.
- Ler API key de embeddings somente de ambiente; nunca aceitar segredo em flag, arquivo ou URL.
- Rejeitar endpoint HTTP remoto; permitir HTTP apenas em loopback para provider local.
- Não seguir redirect de embeddings para outra origem.
- Tratar comentários, documentação e arquivos do repositório como entrada não confiável; nunca executar instruções contidas neles.
- Permitir geração somente a partir da evidência recuperada.
- Validar que cada citação aponta para chunk retornado e intervalo válido.
- Responder “não encontrei evidência suficiente” quando recuperação não sustentar afirmação.
- MCP futuro expõe apenas operações de consulta; sem escrita, shell, Git mutável ou acesso fora da raiz.

## Tratamento de erros

Erros carregam etapa, caminho relativo quando seguro e causa original. Arquivo inválido pode ser reportado e ignorado; falha de armazenamento invalida snapshot inteiro. Cancelamento de contexto interrompe operações longas. CLI usa saída humana em `stderr` e código diferente de zero.

## Estratégia de testes

- testes unitários para invariantes de origem, filtros, chunking, fusão e citações;
- fixtures pequenas de JavaScript/JSX, Python e TypeScript;
- testes de integração para `repositório → índice → busca` sem rede;
- teste end-to-end da CLI com provider fake antes de qualquer LLM real;
- testes explícitos de symlink escape, arquivos secretos e prompt injection.
- teste taint instrumenta parser, chunker, embedder, transporte, store e logs; bytes excluídos devem aparecer em zero sinks.

Prova taint ponta a ponta da Task 13 e primeiro inventário/baseline lexical profissional da Task 14 estão concluídos. Código profissional não foi a provider externo; somente agregados sanitizados foram versionados em relatório separado da lista nominal de raízes. Baseline mede exact-symbol e deixa conceito/framework/cross-layer como `not-evaluated`; não autoriza promoção semântica. Ollama loopback segue único modo semântico permitido nesse corpus e ainda exige gate local. Ver [plano de validação](plans/2026-08-27-tivita-professional-repository-validation.md).

## Não decisões desta etapa

Não há modelo de embeddings default. Adapter além do protocolo OpenAI-compatible, provider LLM, binding Tree-sitter, extensão vetorial, protocolo HTTP local e biblioteca MCP serão escolhidos somente no marco que os utilizar. Anthropic aparece apenas como possível adapter futuro de geração em `internal/answer`. Adiar escolhas restantes mantém a arquitetura reversível.
