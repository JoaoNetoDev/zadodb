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
2. **Build (compactante)**: constrói `data.<G+1>.zdb.tmp` por **compactação** —
   faz o streaming da árvore de `data.<G>.zdb` em ordem de chave, faz o **merge**
   com os deltas líquidos do `wal.applying.log` (deltas vencem; deletes removem)
   e faz **bulk-load** de uma B+Tree nova e compacta (folhas empacotadas, sem
   órfãos). Grava a meta (nova raiz + `LastAppliedTxID`), fsync.

   > Isso é crucial: aplicar o WAL incrementalmente via COW numa árvore grande
   > amplifica o tamanho sem limite (cada inserção reescreve o caminho e deixa
   > páginas órfãs). A compactação mantém o arquivo proporcional aos **dados
   > vivos**. A memória é limitada aos deltas do WAL — a árvore base é
   > transmitida, nunca materializada por inteiro.
3. **Publish**: rename `data.<G+1>.zdb.tmp` → `data.<G+1>.zdb` (nome novo, então
   nada mmap-eado é substituído), fsync do diretório.
4. **Switch**: `WriteCurrent(G+1)` — o ponto atômico em que a nova geração passa
   a ser a verdade.
5. **Swap + limpeza**: troca o snapshot de leitura para a nova geração, apaga o
   `wal.applying.log`, remove gerações antigas (best-effort).

## Gatilhos: automático, manual e a válvula `max_overlay`

Por default o checkpoint é **automático**, disparado quando o WAL cresce além de
`checkpoint.wal_bytes` ou pelo timer `checkpoint.interval_sec`.

Dois controles ajustam esse comportamento (config YAML em `checkpoint:` ou flags
equivalentes no `serve`):

- **`manual: true`** (`--checkpoint-manual`): desabilita o checkpoint
  **automático**. A consolidação do WAL só ocorre quando disparada
  explicitamente pelo endpoint `POST /v1/checkpoint` (ver
  [api/rest-api](../api/rest-api.md)).
- **`max_overlay: <N>`** (`--checkpoint-max-overlay=<N>`): **válvula anti-OOM**.
  Força um checkpoint quando o overlay em memória passa de `N` entradas,
  **mesmo em modo manual**. `0` desliga a válvula.

O overlay guarda todas as escritas confirmadas ainda não dobradas no arquivo de
dados; sem checkpoint ele cresce indefinidamente. A válvula garante um teto de
memória mesmo quando o operador esqueceu (ou adiou de propósito) o checkpoint
manual.

### Por que o modo manual acelera o import em massa

O checkpoint compactante **transmite a árvore base inteira** para construir a
geração nova (ver passo 2). Sob checkpoint automático durante uma carga pesada,
isso se repete a **cada** rodada disparada pelo crescimento do WAL — e cada
rodada re-lê e re-escreve toda a base acumulada até ali. Num HD USB lento, esse
custo somado chegava a ~48min.

Com `--checkpoint-manual`, o import roda **sem nenhum checkpoint**: cada lote vai
só para o WAL (append + fsync). Ao final, um único `POST /v1/checkpoint`
consolida tudo numa **única** compactação. Uma passada pela base em vez de
várias — o import termina muito mais rápido. (Deixe a válvula `max_overlay`
ligada como rede de segurança se o volume importado puder exceder a RAM
disponível para o overlay.)

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
