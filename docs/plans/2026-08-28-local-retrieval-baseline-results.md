# Baseline local sanitizado dos repositórios profissionais

**Data:** 2026-08-28  
**Policies:** baseline histórico `scanner-v5`; comparação `scanner-v6`
**Escopo:** três raízes profissionais autorizadas, identificadas apenas como
`repo-2f`, `repo-7c` e `repo-b4`. IDs e ordem das linhas foram atribuídos
independentemente de qualquer lista nominal.

Este relatório contém somente agregados. Não contém mapeamento entre IDs e
nomes, paths, basenames, símbolos, consultas, hits, URLs, conteúdo ou amostras
de código. Os três runs usaram `semantic off`, busca lexical em memória e zero
rede. Nenhum provider, inclusive Ollama, recebeu conteúdo.

## Gate e decisão

Task 13 provou taint zero nos sinks; Task 14A entregou harness aggregate-only;
Task 14C adicionou `scanner-v5`, denials pré-metadata/open e taxonomia segura.
Os três checklists privados passaram e os três runs terminaram `go`. Cache e
outputs ficaram fora das raízes, com permissões privadas e rollback por descarte
do cache.

`go` significa que inventário lexical protegido pôde rodar. Não significa que
todo arquivo é seguro para indexar, que busca semântica foi validada ou que
qualidade geral está aprovada.

## Inventário `scanner-v5`

| Repositório | Elegíveis (arquivos / bytes) | Incluídos (arquivos / bytes) | Cobertura incluída | Chunks / bytes indexados | Sem símbolo |
| --- | ---: | ---: | ---: | ---: | ---: |
| `repo-2f` | 3.828 / 22.273.812 | 3.679 / 19.056.968 | 96,1% / 85,6% | 10.865 / 17.525.898 | 461 |
| `repo-7c` | 3.598 / 10.857.515 | 3.555 / 10.666.903 | 98,8% / 98,2% | 5.918 / 9.365.624 | 1.096 |
| `repo-b4` | 991 / 10.372.649 | 934 / 9.115.985 | 94,2% / 87,9% | 6.284 / 7.839.129 | 82 |

| Repositório | Linguagens/extensões incluídas | Símbolos | Padrões permitidos agregados |
| --- | --- | --- | --- |
| `repo-2f` | Python 3.679 (`.py` 3.679) | class 5.449; function 5.249 | migração 994; teste 1.048 |
| `repo-7c` | TypeScript 3.555 (`.ts` 1.267, `.tsx` 2.288) | class 9; enum 69; function 1.854; interface 841; type 2.050 | configuração 7; teste 586 |
| `repo-b4` | Python 372 (`.py` 372); TypeScript 562 (`.ts` 243, `.tsx` 319) | class 387; function 5.095; interface 583; type 141 | configuração 2; teste 255 |

Contagens de exclusão são decisões agregadas de traversal, não estimativas de
prevalência de paths. Negar um diretório cedo também evita contar nested repos,
symlinks ou metadata existentes dentro dele.

| Repositório | dependency/cache | nested repo | secret | security | symlink | extensão não suportada |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `repo-2f` | 493 | 4 | 149 | 7 | 0 | 181 |
| `repo-7c` | 3 | 2 | 43 | 9 | 0 | 168 |
| `repo-b4` | 33 | 23 | 57 | 13 | 28 | 2.173 |

Em `repo-b4`, `scanner-v4` havia contado 28.579 extensões não suportadas. A
combinação de novos denials e buckets em `scanner-v5` reduziu esse agregado em
92,4%, para 2.173, sem abrir os diretórios negados. Essa diferença confirma que
priorizar parser sobre inventário não refinado teria sido enganoso; não revela
conteúdo oculto.

## Gaps por extensão

Maiores buckets restantes por bytes:

| Repositório | Buckets agregados prioritários (arquivos / bytes) |
| --- | --- |
| `repo-2f` | `<other>` 41 / 1.655.177; `.ttf` 2 / 825.168; `.lock` 1 / 300.628; `.html` 37 / 256.403; `.jpg` 2 / 182.331 |
| `repo-7c` | `<other>` 55 / 1.864.016; `.gif` 2 / 1.667.358; `.png` 16 / 1.253.330; `.lock` 1 / 754.622; `.svg` 31 / 572.942 |
| `repo-b4` | `.json` 1.141 / 397.534.612; `.png` 200 / 17.711.017; `<other>` 80 / 14.076.227; `.js` 376 / 11.195.386; `.md` 213 / 3.271.266 |

`<other>` é deliberadamente opaco. Extensões de imagem, fonte, archive,
biblioteca nativa e executável são reportáveis, mas continuam não indexáveis.
`.json` permanece não suportado: volume não prova valor, pode refletir dados,
artefatos ou configuração sensível, e um fallback de texto cru seria inseguro.

## Baseline lexical exact-symbol

Harness gerou até 1.000 consultas sintéticas a partir de símbolos já permitidos.
Isso mede localização de símbolo conhecido; não constitui gold set humano.

| Repositório | Queries | Recall@5 | Recall@10 | MRR@10 | nDCG@10 | Citação válida | Ordem determinística |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `repo-2f` | 1.000 | 0,929 | 0,954 | 0,878 | 0,897 | 1,000 | 1,000 |
| `repo-7c` | 1.000 | 0,945 | 0,971 | 0,820 | 0,859 | 1,000 | 1,000 |
| `repo-b4` | 1.000 | 0,936 | 0,967 | 0,876 | 0,899 | 1,000 | 1,000 |

| Repositório | Scan | Index | Query p50 / p95 | Pico heap aproximado |
| --- | ---: | ---: | ---: | ---: |
| `repo-2f` | 2.384 ms | 81 ms | 287.600 / 298.842 µs | 93.669.104 B |
| `repo-7c` | 1.462 ms | 68 ms | 167.568 / 169.515 µs | 51.400.632 B |
| `repo-b4` | 1.112 ms | 43 ms | 145.208 / 147.882 µs | 44.763.552 B |

Latência e heap são evidência process-local, não SLA. Os três runs ocorreram
concorrentemente e disputaram CPU; comparação futura deve repetir serialmente
no mesmo host quando performance virar gate.

## Limitações honestas

Estas categorias continuam `not-evaluated` em todos os repositórios: conceito,
configuração/path, fluxo cross-layer, mensagem de erro, framework e evidência
negativa. Não existe gold set humano privado ainda. Logo:

- não há conclusão sobre ganho semântico ou qualidade híbrida;
- não há inferência de framework baseada em nome/path ou conteúdo excluído;
- 100% de citação e determinismo valem somente para baseline exact-symbol;
- ausência de Ollama neste ciclo evita qualquer alegação sobre embeddings locais.

## Priorização resultante

1. **P0 — JavaScript/JSX:** `.js` permanece sinal claro em `repo-b4` após poda
   de cache. Adicionar linguagem explícita, scanner, parser conservador,
   chunking/citação e filtro CLI com fixtures sintéticas. Bump de policy e
   reindex obrigatórios. Antes de novo acesso profissional, repetir testes
   taint/no-egress, hard-deny pré-open, secret scan e go/no-go por raiz. Medir
   delta antes/depois somente se todos os gates passarem.
2. **P1 — gold set local humano:** pelo menos cinco queries aplicáveis por
   categoria/repositório, mantidas somente no workspace privado. Necessário
   antes de promover híbrido ou alegar qualidade conceitual.
3. **P2 — Markdown:** 240 arquivos e 3.424.441 bytes agregados nos três roots.
   Exige chunker de headings/blocos, limites e fixtures de prompt injection;
   nunca fallback cru ilimitado.
4. **P3 — JSON seletivo:** não liberar `.json` globalmente. Primeiro definir
   allowlist de basename/schema permitido, limites, redaction e testes de
   segredo; dados e artefatos permanecem excluídos.
5. **P4 — assets/binários:** manter não indexáveis. Taxonomia serve apenas para
   custo/cobertura. HTML/XML/CSS têm impacto menor e ficam atrás dos itens acima.

Próximo checkpoint recomendado: implementar P0 com TDD e revisão independente;
revalidar taint, hard-deny, secret scan, no-egress e checklist individual;
somente então repetir baseline local protegido. Depois, criar gold set P1 sem
versionar queries ou hits.

## Comparação protegida `scanner-v6`

Depois do tracer bullet sintético, os gates de taint/no-egress/hard-deny foram
reexecutados, três checklists privados novos foram preenchidos e as três raízes
foram avaliadas novamente. Os runs ocorreram serialmente, com `semantic off`,
sem provider/rede, raiz read-only e outputs aggregate-only `0600` fora dos
repositórios. Todos terminaram `go`. Os IDs públicos continuam remapeados e não
posicionais; nenhuma ligação com nomes ou roots foi persistida.

| Repositório | Elegíveis (arquivos / bytes) | Incluídos (arquivos / bytes) | Cobertura incluída | Chunks / bytes indexados | Sem símbolo |
| --- | ---: | ---: | ---: | ---: | ---: |
| `repo-2f` | 3.828 / 22.273.812 | 3.679 / 19.056.968 | 96,1% / 85,6% | 10.865 / 17.525.898 | 461 |
| `repo-7c` | 3.606 / 10.860.453 | 3.563 / 10.669.841 | 98,8% / 98,2% | 5.926 / 9.368.554 | 1.104 |
| `repo-b4` | 1.367 / 21.568.035 | 1.304 / 14.792.881 | 95,4% / 68,6% | 6.650 / 13.515.659 | 446 |

| Repositório | Linguagens/extensões incluídas | Delta JavaScript sobre v5 | Símbolos agregados |
| --- | --- | ---: | --- |
| `repo-2f` | Python 3.679 (`.py` 3.679) | 0 arquivo / 0 byte | class 5.449; function 5.249 |
| `repo-7c` | JavaScript 8 (`.js` 8); TypeScript 3.555 (`.ts` 1.267, `.tsx` 2.288) | +8 arquivos / +2.938 bytes | class 9; enum 69; function 1.854; interface 841; type 2.050 |
| `repo-b4` | JavaScript 370 (`.js` 370); Python 372 (`.py` 372); TypeScript 562 (`.ts` 243, `.tsx` 319) | +370 arquivos / +5.676.896 bytes | class 387; function 5.101; interface 583; type 141 |

Em `repo-b4`, os 376 candidatos `.js` do baseline viraram 370 arquivos
incluídos; quatro foram barrados pelo detector de segredo e dois pelo limite de
tamanho. Isso é comportamento esperado: suporte de extensão não contorna gates
de conteúdo. A contagem unsupported caiu de 2.173 para 1.797. Em `repo-7c`,
oito `.js` foram incluídos e a contagem unsupported caiu de 168 para 160.
`repo-2f` permaneceu byte a byte igual nas contagens de corpus.

O sinal estrutural é deliberadamente conservador. Entre os 378 JavaScript
incluídos, a variação agregada é de seis símbolos `function` e 372 arquivos sem
símbolo adicional. Isso confirma contagens de inclusão/chunking, não cobertura
de AST nem citação específica de JavaScript. A fronteira JS de citação foi
provada por fixtures sintéticas; a amostra profissional exact-symbol é limitada
a 1.000 casos e não informa linguagem por query. Próximo adapter de linguagem
deve atacar declarações/módulos JavaScript medidos com fixtures sintéticas e
manter falso negativo preferível a símbolo inventado.

| Repositório | Queries | Recall@5 | Recall@10 | MRR@10 | nDCG@10 | Citação válida | Ordem determinística |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `repo-2f` | 1.000 | 0,929 | 0,954 | 0,878 | 0,897 | 1,000 | 1,000 |
| `repo-7c` | 1.000 | 0,945 | 0,971 | 0,820 | 0,859 | 1,000 | 1,000 |
| `repo-b4` | 1.000 | 0,936 | 0,967 | 0,876 | 0,899 | 1,000 | 1,000 |

As métricas exact-symbol ficaram iguais ao baseline na precisão publicada.
Isso não demonstra ganho de busca: o conjunto automático continua derivado de
símbolos já reconhecidos, e o parser JavaScript encontrou pouco sinal novo.
Citação e ordem permaneceram 100% dentro desse escopo estreito.

| Repositório | Scan | Index | Query p50 / p95 | Pico heap aproximado |
| --- | ---: | ---: | ---: | ---: |
| `repo-2f` | 2.325 ms | 80 ms | 282.740 / 292.982 µs | 80.723.352 B |
| `repo-7c` | 1.350 ms | 68 ms | 165.820 / 167.754 µs | 42.622.808 B |
| `repo-b4` | 1.894 ms | 76 ms | 258.852 / 262.165 µs | 56.251.072 B |

Runs v5 foram concorrentes; runs v6 foram seriais. Números acima são
diagnóstico process-local, não comparação controlada nem SLA. A carga maior de
`repo-b4` é compatível com corpus indexado 72,4% maior, mas não prova causalidade
de qualquer diferença de latência.

## Decisão após `scanner-v6`

1. **P0 — gold set humano privado:** continua bloqueio para alegar qualidade
   conceitual, cross-layer, framework, erro ou evidência negativa. Queries,
   julgamentos e hits não serão versionados.
2. **P1 — adapter estrutural JavaScript:** medir padrões via gold set e ampliar
   parser/chunker com fixtures sintéticas. Não relaxar fail-closed nem trocar por
   fallback cru. Mudança de parser exige versão de chunk/reindex explícitos.
3. **P2 — Markdown:** permanece candidato por volume agregado; exige chunker de
   headings/blocos, limites e fixtures de prompt injection.
4. **P3 — JSON seletivo:** continua unsupported. Volume alto não prova segurança
   ou utilidade; exigir allowlist de basename/schema, redaction e novo gate.
5. **P4 — assets/binários:** permanecem não indexáveis.

Sem gold set humano, não há base para habilitar semântica profissional, escolher
ANN, promover híbrido ou afirmar superioridade sobre lexical. Próximo run real
deve usar harness de gold set privado; até lá, baseline lexical e fallback
continuam referência segura.
