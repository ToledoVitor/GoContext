# Arquitetura proposta

## Escopo

GoContext começa como aplicação local, de processo único e orientada a CLI. O núcleo Go coordena ingestão, recuperação e resposta. React/TypeScript poderá consumir uma API local depois que o fluxo CLI provar utilidade; não participa do MVP de fim de semana.

## Fluxo de dados

1. **Scanner** percorre somente uma raiz autorizada, aplica regras de exclusão, rejeita binários e mantém caminhos relativos.
2. **Parser** extrai símbolos de Python e TypeScript com intervalos de linhas.
3. **Chunker** prefere um símbolo por chunk; símbolos grandes podem ser divididos com pequena sobreposição e mesma origem.
4. **Store** grava chunks e metadados em snapshot consistente.
5. **Busca lexical** favorece nomes exatos, identificadores e mensagens de erro.
6. **Busca vetorial** opt-in usa embeddings provider-agnostic e recupera conceitos expressos com vocabulário diferente.
7. **Fusão híbrida** combina rankings, inicialmente por Reciprocal Rank Fusion, evitando calibrar escalas incompatíveis.
8. **Gerador** recebe somente melhores chunks e pergunta do usuário.
9. **Guardrail** rejeita citações inexistentes ou fora da evidência recuperada.
10. **CLI ou MCP** apresenta resposta e referências `caminho:linha-inicial-linha-final`.

Cada etapa preserva `source.Reference`. Citação não é reconstruída a partir de texto gerado.

## Módulos Go

| Módulo | Responsabilidade agora | Implementação futura |
| --- | --- | --- |
| `cmd/gocontext` | entrada do processo e versão | comandos `index`, `search` e `ask` |
| `internal/source` | fatos e proveniência do código | IDs estáveis e hashes de conteúdo |
| `internal/ingest` | contratos do pipeline | filesystem scanner, parsers e chunker |
| `internal/search` | consulta e resultado ranqueado | lexical, vetorial e fusão |
| `internal/answer` | geração e validação | adapter LLM e guardrails determinísticos |

Após a fundação M0, novos pacotes devem nascer junto de comportamento executável. Não haverá pastas vazias para `mcp`, `storage`, `embeddings` ou `web`.

## Limites e dependências

- `source` não depende de outros pacotes internos.
- `ingest` e `search` dependem de `source`.
- `answer` depende de `search` e `source`.
- `cmd/gocontext` será o composition root; implementações concretas não devem ser importadas pelos tipos centrais.
- Interfaces descrevem necessidades do consumidor. Adapters de SQLite, parser, embedding e LLM ficam nas bordas.

## Persistência e busca

MVP usará armazenamento local único. M2 introduzirá SQLite versionado para gerações atômicas de chunks e vetores. Primeiro mecanismo vetorial fará scan exato e cosseno em Go; extensão vetorial e ANN só entram após medição. O ranking híbrido recebe candidatos independentes e combina posições por RRF, não scores crus.

Nenhum índice externo é necessário. Reindexação completa de repositórios pequenos é aceitável antes de otimizar atualizações incrementais.

`internal/ingest/localstore` oferece persistência inicial antes do índice SQLite. Cada repositório recebe snapshot JSON versionado, identificado no disco pelo SHA-256 do repository ID. Escrita usa arquivo temporário, `fsync` e rename atômico; arquivos usam permissão `0600`. Leitura valida versão, repository ID, chunks duplicados, referências e limite de 64 MiB. Esse adapter não oferece busca e será substituído ou absorvido pelo armazenamento do M2.

`internal/search/lexical` implementa recuperação lexical inicial diretamente sobre snapshots. Consulta e campos de chunk são tokenizados em Unicode, com separação de `camelCase`, `snake_case` e pontuação. Score normalizado combina presença do termo em texto (`0.6`), símbolo (`0.3`) e caminho (`0.1`); empates usam caminho, linha e chunk ID. Snapshot é carregado a cada consulta. Não há índice invertido, frequência de termos ou BM25 nesta fase. Migração para SQLite preservará esse comportamento antes de qualquer mudança de algoritmo.

## Embeddings e recuperação híbrida planejados

- `internal/embedding` define profile, purpose, vector e seam mínimo `Embedder`.
- `internal/embedding/openaicompat` será primeiro adapter concreto; OpenAI e Ollama diferem por configuração, não por contrato.
- `internal/index` esconderá batching, revisão de corpus e publicação atômica de geração.
- `internal/index/sqlite` guardará chunks canônicos, referências, vetores e manifest ativo.
- `internal/search/vector` criará embedding de query e retornará somente chunks da geração canônica.
- `internal/search/hybrid` combinará lexical e vector por RRF com fallback lexical.
- `cmd/gocontext` continuará sendo único composition root de adapters, configuração e credenciais.

Sem modo `preferred|required` explícito, nenhuma chamada de rede ocorre, mesmo se endpoint/modelo estiverem no ambiente. Modo `preferred` degrada somente erros temporários tipados para lexical e emite aviso sanitizado; corrupção, configuração inválida e cancelamento continuam erros. Modelo, dimensão ou fingerprint diferente exige rebuild completo. Detalhes: [ADR 0002](decisions/0002-embeddings-vector-search.md).

## Parsing e chunking

Python e TypeScript exigem parser estrutural. Tree-sitter é a direção preferida; binding Go e impacto de CGO precisam ser validados no marco de ingestão.

`internal/ingest/lineparser` oferece descoberta preliminar enquanto esse spike não ocorre. Reconhece declarações top-level comuns de Python (`def`, `async def`, `class`) e TypeScript (`function`, `class`, `interface`, `type`, `enum`) e registra linha declaratória. Não interpreta AST, bodies, métodos, símbolos aninhados, arrow functions ou declarações multilinha. Essas limitações impedem uso do parser atual para chunking final.

Política inicial de chunking:

- função, método, classe ou declaração exportada forma unidade natural;
- assinatura e comentários de documentação acompanham corpo;
- arquivo sem símbolos usa um chunk de arquivo no recorte inicial; divisão por linhas entra junto do orçamento de tokens;
- chunk registra linguagem, símbolo, caminho e linhas;
- tamanho máximo será medido em tokens do modelo de embedding escolhido.

`internal/ingest/symbolchunker` implementa recorte inicial dessa política. Cada símbolo começa na linha declaratória e termina antes da próxima declaração top-level; linhas vazias finais são removidas. Arquivo não vazio sem símbolos gera um chunk de arquivo. IDs usam SHA-256 sobre versão, origem, linguagem, símbolo e texto, mantendo reindexações determinísticas. Divisão de símbolos grandes por orçamento de tokens permanece pendente.

## Scanner filesystem atual

`internal/ingest/filesystem` implementa `ingest.Scanner` usando `os.Root`, disponível no Go 1.24, para manter leituras confinadas à raiz autorizada. Scanner:

- inclui `.py`, `.ts` e `.tsx`;
- retorna caminhos relativos normalizados e intervalos de linhas;
- não segue symlinks;
- exclui diretórios de VCS, dependências, ambientes virtuais, caches, cobertura e builds;
- ignora arquivos não regulares, binários com byte NUL e fontes acima de 1 MiB;
- respeita cancelamento de contexto.

Antes de qualquer avaliação profissional, scanner receberá policy default-deny auditável. Paths reconhecidamente sensíveis são recusados antes de `Open`: `.env`, `.env.*`, `.git/**`, `.github/**`, metadata/automação, credenciais/chaves/certificados, nested repos, symlinks, dependências e caches. Arquivos de nome permitido são lidos com limite pelo scanner para classificar binário, UTF-8, gerado, tamanho e padrões conservadores de segredo; itens detectados não viram `source.File` nem atravessam parser/rede/store. Report expõe somente contagens por categoria.

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
- fixtures pequenas de Python e TypeScript;
- testes de integração para `repositório → índice → busca` sem rede;
- teste end-to-end da CLI com provider fake antes de qualquer LLM real;
- testes explícitos de symlink escape, arquivos secretos e prompt injection.
- teste taint instrumenta parser, chunker, embedder, transporte, store e logs; bytes excluídos devem aparecer em zero sinks.

Validação profissional usa primeiro inventário e benchmark lexical offline. Taba App, Tivita Backend e Tivita Web App não enviam código a provider externo; Ollama loopback é único modo semântico permitido. Relatórios guardam agregados sanitizados. Ver [plano de validação](plans/2026-08-27-tivita-professional-repository-validation.md).

## Não decisões desta etapa

Modelo de embeddings, adapter além do protocolo OpenAI-compatible, provider LLM, binding Tree-sitter, extensão vetorial, protocolo HTTP local e biblioteca MCP serão escolhidos somente no marco que os utilizar. Anthropic não é provider de embeddings; poderá aparecer futuramente como adapter de geração em `internal/answer`. Adiar escolhas restantes mantém fundação compilável e reversível.
