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

## Limitações conhecidas (fase futura)

- **Delete não faz merge/rebalance** de nós: a entrada é removida (COW), nós
  podem ficar sub-ocupados, e o espaço é recuperado na próxima geração (que é
  reescrita). Isso mantém o delete simples e as buscas sempre corretas.
- **Sem compactação incremental**: um checkpoint copia a geração base e aplica
  os deltas; páginas órfãs acumulam até... na prática cada geração é um arquivo
  novo, então o crescimento é limitado, mas não há compactação que elimine nós
  vazios de dentro de uma árvore existente. Índices secundários e compactação
  são itens de fase futura.
