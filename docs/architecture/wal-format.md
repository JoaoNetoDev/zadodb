# WAL (Write-Ahead Log)

Pacote `internal/storage/wal`. O WAL é a única fonte de durabilidade: toda
mutação é anexada aqui como um registro com checksum e só é confirmada ao
cliente após um `fsync` físico.

## Formato do registro

```
[0:4]   Magic uint32        ZWAL
[4:5]   Version uint8
[5:8]   reservado (zero)
[8:16]  TxID uint64         monotônico, atribuído pelo sequencer
[16:20] PayloadLen uint32
[20:24] CRC32C uint32       sobre TxID || PayloadLen || Payload
[24:..] Payload             msgpack de WALEntry
```

`WALEntry` = `{Op, Class, ObjectID, Data, Timestamp}`, onde `Op` é um de
`OpPut`, `OpDelete`, `OpCreateClass`, `OpDropClass`. O payload é MessagePack
(compacto). O checksum cobre TxID + tamanho + payload; se qualquer um for
corrompido, a leitura detecta e para.

## Torn tail é esperado

Ao ler o WAL (recovery/checkpoint), o `Reader` para no **primeiro** registro
truncado ou com checksum inválido, retornando `ErrCorrupt`. Isso é o resultado
**normal** de um crash no meio de uma escrita: os registros anteriores já foram
fsyncados e são válidos; o registro incompleto (e o que viesse depois) é
descartado. Não é um erro fatal — é exatamente "perder as últimas transações não
confirmadas".

## Sequencer: o único ponto de serialização

Uma **única goroutine** (`Sequencer`) é a única que escreve no WAL. Os callers
(handlers HTTP) fazem em paralelo, sem lock, o trabalho caro: validação e
serialização MessagePack. Só o append físico + fsync é serializado, na goroutine
do sequencer. Como um único goroutine é dono do arquivo, o append não precisa de
lock e a atribuição de TxID é naturalmente livre de corrida.

- `Submit(payload)` — bloqueia só até o fsync **daquele** commit (ou do batch,
  em group-commit), devolvendo o TxID atribuído.
- `Rotate(retiredPath)` — corta o log num limite limpo de registro (usado pelo
  checkpoint). Ver [recovery-and-checkpoint](recovery-and-checkpoint.md).

## Modos de fsync

- **per-commit** (padrão): fsync após cada registro, antes de confirmar. Um
  write confirmado nunca é perdido; custo de um fsync por commit.
- **group-commit**: coalesce vários commits concorrentes num único fsync,
  trocando uma janela minúscula de durabilidade por muito mais throughput sob
  carga concorrente. Todos os writers do batch são confirmados juntos, só após o
  fsync compartilhado.

Ver [fsync-tuning](../operations/fsync-tuning.md).
