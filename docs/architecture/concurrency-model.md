# Modelo de concorrência

O objetivo é **escritas concorrentes reais** (não serializar a aplicação como o
SQLite faz), mantendo um único ponto de serialização mínimo.

## Write path

```
handler HTTP (goroutine A) ─┐
handler HTTP (goroutine B) ─┼─ validação + msgpack (em paralelo, sem lock)
handler HTTP (goroutine C) ─┘        │
                                     ▼
                             Sequencer (1 goroutine)
                                append no WAL + fsync
                                     │
                                     ▼
                             overlay (map protegido por RWMutex)
```

- O trabalho caro (validação, serialização MessagePack, checksum do payload)
  acontece **na goroutine chamadora**, em paralelo, sem lock.
- O **único** ponto serializado é o consumo do canal pelo sequencer, que faz o
  append físico e o fsync. É uma operação de microssegundos por commit (ou
  amortizada por batch em group-commit).
- Em **group-commit**, vários commits concorrentes compartilham um único fsync —
  quanto maior a concorrência, maior o throughput por fsync.

## Read path

Leituras não passam pelo sequencer nem por locks de escrita:

- O overlay é protegido por um `sync.RWMutex` — leituras usam `RLock`.
- O snapshot mmap é obtido por um ponteiro atômico com refcount; leituras
  concorrentes nunca bloqueiam escritas e vice-versa.

## Locks no engine

- `mu sync.RWMutex` — protege o overlay e o conjunto de classes. Escritas usam
  `Lock` por um instante (só para gravar no map); leituras usam `RLock`.
- `ckMu sync.Mutex` — serializa checkpoints (só um por vez); o checkpoint em
  background e o `Checkpoint()` manual competem por ele.
- O gerador de IDs tem seu próprio mutex (contenção baixa: só incrementa um
  contador em memória).

## Garantias testadas

- `TestEngineConcurrentWriters` (`-race`): 20 writers × 50, sem corrida, contagem
  correta.
- `TestConcurrentWritersThroughput` (tag `resilience`): 64 writers × 500 = 32k
  escritas, guarda de deadlock por timeout, ~20k ops/s localmente.
- `TestSequencerConcurrent*`: 50 goroutines submetendo, TxIDs únicos e
  monotônicos (1..N sem duplicata nem buraco).

Ver [concurrency-testing](../testing/concurrency-testing.md).
