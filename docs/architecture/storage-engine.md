# Storage engine e formato de página

## Página (4KB)

Pacote `internal/storage/page`. Toda a persistência é feita em páginas fixas de
**4096 bytes**, escolhido para casar com o tamanho de página do SO/disco e
favorecer leitura via mmap.

Cada página começa com um header de 32 bytes:

| Offset | Campo | Tipo | Descrição |
|---|---|---|---|
| 0 | Checksum | uint32 | CRC32C (Castagnoli) sobre os bytes `[4:4096]` |
| 4 | Magic | uint32 | `ZDB1` |
| 8 | Type | uint8 | free / meta / btree-leaf / btree-internal / overflow |
| 9 | Flags | uint8 | reservado |
| 12 | PayloadLen | uint32 | bytes úteis no corpo |
| 16 | PageID | uint64 | auto-referência (sanidade) |
| 24 | LSN | uint64 | número de sequência que escreveu a página (debug) |

O checksum é o **primeiro** campo e cobre todo o resto da página. Assim,
qualquer alteração (bit-flip no corpo, adulteração de tipo, página torn) é
detectada por `Page.Verify()`, que confere magic + recomputa o CRC32C.

## Pager

`page.Manager` é dono de um arquivo de dados e distribui páginas
**sequencialmente** — não há free list. Isso é uma decisão deliberada: o
copy-on-write gera páginas órfãs, mas elas são recuperadas na próxima geração,
que reescreve o arquivo do zero (via checkpoint). Manter o pager sem free list o
torna simples e correto.

- `Allocate()` — devolve o próximo PageID (página 0 é sempre a meta page).
- `WritePage(p)` — finaliza (checksum) e grava na posição `id * PageSize`.
- `ReadPage(id)` — lê e verifica; usado só por checkpoint/recovery, **nunca** no
  caminho de leitura online (que usa mmap).
- `Sync()` — fsync físico.

## Meta page (página 0)

Guarda o registro raiz da geração:

- `Root` — PageID da raiz da B+Tree (`InvalidPageID` quando vazia).
- `LastAppliedTxID` — maior TxID do WAL já dobrado nesta geração.
- `NumPages` — total de páginas alocadas.

Os contadores de ID por classe **não** são persistidos aqui — são reconstruídos
no boot varrendo a árvore e o WAL (ver [idgen](../../internal/storage/idgen)),
evitando uma segunda fonte de verdade que poderia divergir após um crash.

## Chaves

Objetos e definições de classe coexistem num único espaço de chaves ordenado.
Cada classe vive dentro de um **project** (namespace virtual; ver
[api/rest-api](../api/rest-api.md)). O project padrão é a string vazia `""` e usa
o layout **legado**, sem prefixo — assim um banco criado antes dos projects não
precisa de migração alguma:

| | Project padrão (`""`) | Project nomeado |
|---|---|---|
| Definição de classe | `0x01` + classe | `0x01` + project + `0x00` + classe |
| Objeto | `0x02` + classe + `0x00` + id8 | `0x02` + project + `0x00` + classe + `0x00` + id8 |

(id = uint64 big-endian.)

Nomes de classe e de project são validados (sem `0x00`) na borda, então os
separadores `0x00` são inequívocos: uma chave pertence ao project padrão sse seu
corpo não tem `0x00` antes do id, e a um project nomeado caso contrário (a
decodificação divide no primeiro `0x00`). IDs são inteiros positivos crescentes,
então big-endian ordena ascendente — objetos da mesma classe ficam fisicamente
contíguos, o que torna a listagem por classe um scan de prefixo eficiente.

O project é puramente um prefixo de chave: o write path (WAL→COW→rename), a
compactação (`btree.Builder`, key-agnostic), o snapshot mmap e o recovery são
todos idênticos — a garantia de corrupção-zero sob kill não é afetada. Em
memória, o conjunto de classes e o gerador de ids são chaveados por
`project + 0x00 + classe` (`wal.ScopeKey`), mantendo a mesma classe independente
entre projects.

## Consulta / paginação

A listagem/consulta de objetos (ver [api/rest-api](../api/rest-api.md)) roda em
**streaming**, sem materializar a classe inteira. A visão consistente de uma
classe é a fusão de duas fontes ordenadas por id:

- o **snapshot mmap** da geração ativa (dados já checkpointados);
- o **overlay** em memória (escritas confirmadas ainda não dobradas no arquivo de
  dados).

A consulta faz o **merge** dessas duas sequências como um *merge-join* por id (o
overlay vence quando os ids coincidem; deletes suprimem o objeto), emitindo
objetos em ordem crescente de id. Só os que passam pelos filtros e cabem na
página são retornados. O **pico de memória** é, portanto, *página retornada +
overlay* — não cresce com o tamanho da classe. É isso que mantém classes grandes
e joins sem estourar a RAM.

### Keyset (cursor) vs. offset

Duas formas de paginar sobre essa sequência ordenada:

- **`offset`/`limit`** (legado): descarta as primeiras `offset` linhas casadas —
  **O(offset)**, degrada em paginação profunda.
- **`after=<id>`** (keyset): faz um **seek** na B+Tree para a primeira chave com
  id > `<id>` e segue dali — **O(tamanho-da-página)**, independente da
  profundidade. A resposta devolve `next_after` (id do último objeto) quando a
  página vem cheia, para o cliente encadear a próxima com `?after=<next_after>`.

O seek keyset se aplica ao snapshot mmap; o overlay, muito menor, é posicionado
por busca na estrutura ordenada em memória. Prefira `after` para varredura
sequencial de classes grandes.

### Resolvedor de joins (semi-joins)

Filtros por campos de classes relacionadas (`eq.<rel>.<campo>`) são resolvidos
como uma **cadeia de semi-joins** sobre o mesmo caminho de consulta em streaming,
usando BFS no grafo de relações para achar o caminho da classe base até a classe
do alias. Detalhes em [joins](joins.md).
