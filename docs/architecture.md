# Arquitetura proposta

## Escopo

GoContext começa como aplicação local, de processo único e orientada a CLI. O núcleo Go coordena ingestão, recuperação e resposta. React/TypeScript poderá consumir uma API local depois que o fluxo CLI provar utilidade; não participa do MVP de fim de semana.

## Fluxo de dados

1. **Scanner** percorre somente uma raiz autorizada, aplica regras de exclusão, rejeita binários e mantém caminhos relativos.
2. **Parser** extrai símbolos de Python e TypeScript com intervalos de linhas.
3. **Chunker** prefere um símbolo por chunk; símbolos grandes podem ser divididos com pequena sobreposição e mesma origem.
4. **Store** grava chunks e metadados em snapshot consistente.
5. **Busca lexical** favorece nomes exatos, identificadores e mensagens de erro.
6. **Busca vetorial** recupera conceitos expressos com vocabulário diferente.
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

MVP usará armazenamento local único. Direção preferida: SQLite para metadados e busca textual; extensão ou tabela vetorial local será escolhida após um spike curto. O ranking híbrido recebe candidatos independentes e combina posições, não scores crus.

Nenhum índice externo é necessário. Reindexação completa de repositórios pequenos é aceitável antes de otimizar atualizações incrementais.

## Parsing e chunking

Python e TypeScript exigem parser estrutural. Tree-sitter é a direção preferida; binding Go e impacto de CGO precisam ser validados no marco de ingestão. Até essa validação, nenhum parser é fixado como dependência.

Política inicial de chunking:

- função, método, classe ou declaração exportada forma unidade natural;
- assinatura e comentários de documentação acompanham corpo;
- arquivo sem símbolos usa blocos delimitados por linhas;
- chunk registra linguagem, símbolo, caminho e linhas;
- tamanho máximo será medido em tokens do modelo de embedding escolhido.

## Guardrails

- Canonicalizar raiz autorizada e impedir escape por `..` ou symlink.
- Honrar `.gitignore` e ignorar `.git`, dependências, artefatos, binários e arquivos acima do limite configurado.
- Não registrar conteúdo-fonte, prompts, segredos ou embeddings por padrão.
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

## Não decisões desta etapa

Provider de embeddings, LLM, binding Tree-sitter, extensão vetorial, protocolo HTTP local e biblioteca MCP serão escolhidos somente no marco que os utilizar. Adiar essas escolhas mantém fundação compilável e reversível.
