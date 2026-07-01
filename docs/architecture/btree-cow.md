# B+Tree copy-on-write

Pacote `internal/storage/btree`. A árvore é serializada em páginas de 4KB e é
**copy-on-write**: toda mutação escreve páginas novas ao longo do caminho da
folha alterada até uma nova raiz, e nunca sobrescreve uma página viva.

## Por que copy-on-write

É o que permite duas garantias centrais:

1. **Publicação atômica**: um checkpoint constrói uma nova geração inteira sem
   tocar a que está em uso, e a publica com uma troca atômica de ponteiro.
2. **Leitura sem lock**: um leitor que capturou uma raiz vê um snapshot imutável
   — nenhuma escrita concorrente altera as páginas que ele está navegando.

`TestCopyOnWriteImmutability` prova isso: após capturar uma raiz antiga e depois
inserir/apagar muito, a raiz antiga ainda devolve exatamente o estado original.

## Nós

- **Folha**: entradas ordenadas `(chave, valor)`. O valor é inline se ≤ 512
  bytes, senão referencia uma **cadeia de overflow**.
- **Interno**: `n` separadores + `n+1` ponteiros de filho.

Ambos cabem numa página de 4KB. Como cada entrada de folha é limitada (chave +
até 512 bytes de valor, ou uma referência de overflow de 16 bytes), um split ao
meio de uma folha cheia sempre produz duas metades que cabem.

## Overflow

Valores maiores que 512 bytes são gravados numa cadeia de páginas
`PageTypeOverflow` (cada uma: `next PageID` + `chunkLen` + bytes). A folha guarda
só uma referência de 16 bytes. Como tudo é COW, substituir um valor grande aloca
páginas de overflow novas; as antigas viram órfãs, recuperadas no próximo
checkpoint. `TestOverflowLargeValue` cobre valores multi-página (20KB).

## Operações

- `Insert` / `Delete` propagam o COW até uma nova raiz e a devolvem.
- `Get` navega da raiz à folha (busca linear dentro do nó — nós são pequenos).
- `Scan(prefix, fn)` visita em ordem crescente as chaves com o prefixo, podando
  subárvores que não se sobrepõem ao intervalo.

`Get` e `Scan` são funções de pacote que operam sobre qualquer `PageSource`
(read-only), o que permite reaproveitá-las tanto no lado de escrita quanto no
snapshot mmap.

## Bulk builder (compactação)

Além do caminho COW (usado nas escritas online), o pacote expõe um **bulk
builder** (`btree.Builder`) que carrega uma B+Tree compacta a partir de chaves
em ordem crescente: folhas empacotadas quase cheias e níveis internos montados
de baixo para cima. O resultado **não tem órfãos** — seu tamanho é proporcional
aos dados vivos. O checkpoint usa isso para compactar (ver
[recovery-and-checkpoint](recovery-and-checkpoint.md)); medido em ~70× menor que
a sequência COW equivalente. A memória é limitada a uma folha + o índice por
nível (`firstKey`, `pageID`), não ao dataset inteiro.

## Limitações conhecidas (fase futura)

- **Delete online não faz merge/rebalance** de nós: a entrada é removida (COW),
  nós podem ficar sub-ocupados até o próximo checkpoint, que **reconstrói a
  árvore compacta** e elimina o espaço morto. Mantém o delete simples e as
  buscas sempre corretas.
- **Sem índices secundários**: consultas por campo são full scan (fase futura).
