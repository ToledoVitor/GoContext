# GoContext

GoContext é um copiloto local de inteligência para repositórios. A proposta é transformar código-fonte em contexto pesquisável, responder perguntas com evidências verificáveis e expor esse contexto por uma interface MCP somente leitura.

Este repositório está na transição de **M1 para M2**. Fundação, scanner local, descoberta inicial de declarações, chunking por símbolo, snapshots JSON e busca lexical local estão implementados. Parsing estrutural, índice invertido, embeddings, busca vetorial, LLM, MCP e frontend ainda não estão implementados.

## Objetivo do MVP

Em um fim de semana, provar um fluxo vertical pequeno:

1. abrir um repositório local autorizado;
2. encontrar arquivos Python e TypeScript elegíveis;
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
  └─ filesystem        scanner local seguro para Python e TypeScript
  └─ lineparser        descoberta top-level preliminar, sem AST
  └─ symbolchunker     chunks por limites de declarações top-level
  └─ localstore        snapshots JSON locais e atômicos por repositório
internal/search        contratos para busca e resultados ranqueados
  └─ lexical           ranking lexical determinístico sobre snapshots
internal/answer        contratos para geração fundamentada e guardrails
docs/architecture.md   arquitetura, fluxo de dados e limites de segurança
docs/decisions/        decisões técnicas registradas
docs/roadmap.md        sequência curta de entregas
```

Interfaces ficam próximas do fluxo consumidor, em vez de formar um pacote genérico de abstrações. Implementações concretas só serão adicionadas quando um marco precisar delas.

## Como verificar

Requer Go 1.24 ou superior.

```bash
go test ./...
go vet ./...
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

Comando percorre fontes Python/TypeScript, descobre declarações, cria chunks e substitui snapshot local. Store padrão fica no diretório de cache do sistema, fora do repositório. Para escolher outro local:

```bash
go run ./cmd/gocontext index --store /caminho/do/cache /caminho/do/repositório
```

## Consultar snapshot

Depois de indexar, consulte o snapshot lexical pelo mesmo caminho de repositório:

```bash
go run ./cmd/gocontext search /caminho/do/repositório "carregar usuário"
```

Cada resultado mostra score, citação `arquivo:linha-inicial-linha-final`, símbolo quando disponível e trecho-fonte. Limite resultados ou use um store explícito:

```bash
go run ./cmd/gocontext search --limit 5 --store /caminho/do/cache /caminho/do/repositório carregar usuário
```

## Arquitetura proposta

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

Detalhes e decisões: [arquitetura](docs/architecture.md), [stack](docs/decisions/0001-stack.md) e [roadmap](docs/roadmap.md).

## Fora do escopo do MVP

- frontend React/TypeScript;
- edição ou geração automática de código;
- execução de comandos sugeridos pelo repositório;
- sincronização com GitHub, GitLab ou serviços SaaS;
- autenticação, times e multi-tenancy;
- agentes autônomos;
- suporte amplo a linguagens além de Python e TypeScript.

## Licença

Ainda não definida. Não assuma permissão de redistribuição até uma licença ser adicionada.
