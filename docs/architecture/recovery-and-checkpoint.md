# Checkpoint e recovery

Este é o coração da resistência a crash. O protocolo é desenhado para que, em
**qualquer** ponto de interrupção, o boot encontre um estado consistente.

## Layout de arquivos por geração

Em vez de renomear um arquivo de dados vivo (impossível no Windows quando ele
está mmap-eado), o ZadoDB versiona o arquivo por **geração** e usa um ponteiro
atômico `CURRENT`:

- `data.NNNNNN.zdb` — arquivo de dados de uma geração.
- `CURRENT` — texto minúsculo com o número da geração ativa, atualizado
  atomicamente (escreve `CURRENT.tmp`, fsync, rename por cima). Como o rename é
  atômico, `CURRENT` está sempre totalmente na geração velha ou na nova.
- `wal.log` — o WAL ativo.
- `wal.applying.log` — segmento do WAL sendo dobrado por um checkpoint (só
  existe durante/após um checkpoint interrompido).

## Checkpoint (`checkpoint.Run`)

1. **Rotate**: o sequencer corta o WAL num limite limpo de registro
   (`wal.log` → `wal.applying.log`; um `wal.log` novo e vazio é aberto). Tudo a
   dobrar está agora em `wal.applying.log`.
2. **Build**: copia `data.<G>.zdb` → `data.<G+1>.zdb.tmp`, aplica os registros
   do `wal.applying.log` via B+Tree COW, grava a meta (nova raiz +
   `LastAppliedTxID`), fsync.
3. **Publish**: rename `data.<G+1>.zdb.tmp` → `data.<G+1>.zdb` (nome novo, então
   nada mmap-eado é substituído), fsync do diretório.
4. **Switch**: `WriteCurrent(G+1)` — o ponto atômico em que a nova geração passa
   a ser a verdade.
5. **Swap + limpeza**: troca o snapshot de leitura para a nova geração, apaga o
   `wal.applying.log`, remove gerações antigas (best-effort).

## Recovery (`recovery.Recover`), no boot

Recovery **nunca** muta a geração ativa in-place (evita risco de meta page
torn). Ele mapeia a geração ativa read-only e reconstrói o overlay de escritas
ainda-não-checkpointadas replayando o WAL.

Tratamento dos dois estados pós-crash possíveis:

- **Interrompido antes do switch** (passos 1–3): existe um `data.<G+1>.zdb.tmp`
  órfão e/ou um `wal.applying.log`, mas `CURRENT` ainda aponta para G. O `.tmp` é
  descartado. Se há `wal.applying.log`, o checkpoint é **completado aqui**
  (`BuildGeneration`), dobrando seus registros numa geração nova — assim
  nenhuma escrita confirmada é perdida.
- **Interrompido após o switch** (passo 4 feito): `CURRENT` já nomeia a nova
  geração. Um `wal.applying.log` remanescente é dobrado de novo — mas seus TxIDs
  são ≤ `LastAppliedTxID`, então o replay é um no-op idempotente — e removido.

Depois disso:

1. Abre a geração ativa, lê `LastAppliedTxID` da meta.
2. Reseeda o gerador de IDs varrendo a árvore (ids já armazenados).
3. Replaya `wal.log` para o overlay: registros com TxID > `LastAppliedTxID`
   entram no overlay; para no primeiro registro torn/corrompido.
4. O sequencer retoma com TxID > o maior visto.

## Garantia

Em todos os casos, o pior desfecho é a perda de escritas que nunca chegaram a
ser fsyncadas (torn tail do WAL) — **nunca** corrupção. Isso é exercitado pelo
harness de SIGKILL fuzzing (ver [resilience-testing](../testing/resilience-testing.md)).
