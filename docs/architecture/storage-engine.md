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

Objetos e definições de classe coexistem num único espaço de chaves ordenado:

- Definição de classe: `0x01` + nome da classe.
- Objeto: `0x02` + nome da classe + `0x00` + id (uint64 big-endian).

Nomes de classe são validados (sem `0x00`) na borda, então o separador `0x00` é
inequívoco. IDs são inteiros positivos crescentes, então big-endian ordena
ascendente — objetos da mesma classe ficam fisicamente contíguos, o que torna a
listagem por classe um scan de prefixo eficiente.
