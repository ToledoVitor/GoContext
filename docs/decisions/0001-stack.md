# ADR 0001: stack do MVP

- **Status:** aceito para fundação; adapters externos pendentes
- **Data:** 2026-08-27

## Contexto

MVP precisa indexar repositórios pequenos, combinar busca lexical e semântica, preservar citações e oferecer uma superfície somente leitura. Prazo de fim de semana favorece operação local, poucas dependências e um fluxo vertical antes de UI.

## Decisão

- **Núcleo:** Go 1.24+, em monólito modular e um binário.
- **CLI inicial:** biblioteca padrão `flag`; framework de comandos só entra quando comandos aninhados justificarem dependência.
- **Parsing estrutural futuro:** Tree-sitter para JavaScript/JSX, Python e TypeScript, após spike do binding Go e distribuição com ou sem CGO. O line parser JavaScript atual é apenas um tracer bullet conservador, não substitui essa decisão.
- **Persistência:** SQLite local como direção para metadados e busca lexical; opção vetorial será validada por spike.
- **Busca híbrida:** candidatos lexicais e vetoriais combinados por Reciprocal Rank Fusion.
- **Embeddings e LLM:** portas internas com adapters substituíveis; nenhuma chamada de rede implícita.
- **MCP:** servidor local somente leitura após CLI funcional.
- **Frontend:** React com TypeScript depois do MVP, consumindo API local; não acoplado aos pacotes internos Go.

Fundação atual usa somente biblioteca padrão. Escolhas ainda dependentes de benchmark ou compatibilidade não viram dependências antecipadas.

## Alternativas consideradas

### Backend TypeScript único

Reduz troca de linguagem com frontend, mas perde parte do objetivo de aprendizado em Go e tende a misturar cedo UI, runtime e pipeline. Rejeitado para núcleo.

### Serviços separados por etapa

Facilitariam escalabilidade independente, irrelevante para repositório local pequeno. Aumentariam deploy, observabilidade e falhas distribuídas. Rejeitados para MVP.

### Banco vetorial externo

Entrega recursos prontos, mas adiciona serviço, credenciais e envio de dados. Rejeitado no MVP local-first.

### Busca somente vetorial

Perde precisão em identificadores, caminhos e mensagens de erro. Rejeitada; busca lexical permanece componente de primeira classe.

## Consequências

- Binário e testes permanecem simples nesta fase.
- Fronteiras permitem trocar parser, índice e providers sem alterar tipos de origem.
- SQLite vetorial e Tree-sitter ainda exigem spikes, pois distribuição e CGO podem afetar portabilidade.
- React não bloqueia validação do valor principal.
