# ADR 0001: stack do MVP

- **Status:** aceito; restrição stdlib-only da fundação supersedida no M2
- **Data:** 2026-08-27

## Contexto

MVP precisa indexar repositórios pequenos, combinar busca lexical e semântica, preservar citações e oferecer uma superfície somente leitura. Prazo de fim de semana favorece operação local, poucas dependências e um fluxo vertical antes de UI.

## Decisão

- **Núcleo:** Go 1.24+, em monólito modular e um binário.
- **CLI inicial:** biblioteca padrão `flag`; framework de comandos só entra quando comandos aninhados justificarem dependência.
- **Parsing estrutural futuro:** Tree-sitter para JavaScript/JSX, Python e TypeScript, após spike do binding Go e distribuição com ou sem CGO. O line parser JavaScript atual é apenas um tracer bullet conservador, não substitui essa decisão.
- **Persistência:** SQLite local por `modernc.org/sqlite`, driver puro Go revisado, para gerações de chunks e vetores; snapshot JSON continua sendo o default offline.
- **Busca híbrida:** candidatos lexicais e vetoriais combinados por Reciprocal Rank Fusion.
- **Embeddings e LLM:** portas internas com adapters substituíveis; nenhuma chamada de rede implícita.
- **MCP:** servidor local somente leitura após CLI funcional.
- **Frontend:** React com TypeScript depois do MVP, consumindo API local; não acoplado aos pacotes internos Go.

A fundação começou somente com a biblioteca padrão. No M2, essa restrição foi supersedida por duas dependências diretas revisadas: `modernc.org/sqlite` para persistência SQLite pura em Go e `golang.org/x/sys` para primitivas de filesystem, locking e segurança específicas de plataforma. O monólito Go, o uso de `flag` da stdlib no CLI e as portas/adapters provider-agnostic permanecem decisões vigentes; novas dependências continuam exigindo necessidade concreta e revisão.

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

- O produto continua sendo um binário Go e um monólito modular, agora com módulos externos diretos e transitivos registrados em `go.mod`/`go.sum`.
- Fronteiras permitem trocar parser, índice e providers sem alterar tipos de origem.
- SQLite puro Go, cosseno vetorial exato e busca híbrida foram entregues; ANN e Tree-sitter ainda exigem decisões próprias de benchmark, binding e portabilidade.
- React não bloqueia validação do valor principal.
