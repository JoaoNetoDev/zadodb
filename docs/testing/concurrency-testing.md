# Testes de concorrência

## Rodar

```sh
# Teste rápido incluído na suíte padrão (com detector de corrida)
go test -race -run TestEngineConcurrentWriters ./internal/storage/

# Teste de throughput pesado (build tag resilience)
go test -tags=resilience -run TestConcurrentWritersThroughput -timeout=3m ./test/resilience/
```

## O que é validado

### Ausência de corrida
Toda a suíte roda sob `go test -race`. O engine, o overlay, o snapshot mmap e o
sequencer são exercitados por múltiplas goroutines simultâneas.

### Ausência de deadlock
Os testes de concorrência usam uma guarda de timeout: se os writers não terminam
em N segundos, o teste falha com "deadlock". Isso pega qualquer travamento no
caminho de escrita.

### TxIDs únicos e monotônicos
`TestSequencerConcurrent*` (no pacote `wal`) sobe 50 goroutines submetendo ao
sequencer simultaneamente e confirma que os TxIDs formam exatamente `1..N` — sem
duplicata nem buraco. Isso prova que o único ponto de serialização (o append ao
WAL) é correto sob concorrência.

### Throughput
`TestConcurrentWritersThroughput` roda 64 writers × 500 escritas = 32.000
escritas concorrentes em group-commit, distribuídas por várias classes, medindo
ops/s e confirmando que a contagem final bate. Localmente: **~20k ops/s**.

## Modelo por trás

O único ponto serializado no caminho de escrita é o append ao WAL, feito por uma
goroutine sequencer dedicada. Validação e serialização acontecem em paralelo nas
goroutines dos handlers. Em group-commit, vários commits compartilham um fsync,
então a concorrência **aumenta** o throughput por fsync. Ver
[concurrency-model](../architecture/concurrency-model.md).
