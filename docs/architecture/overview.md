# Visão geral da arquitetura

ZadoDB é um banco de dados orientado a objetos, portável (binário único Go, sem
runtime externo), que expõe uma API REST/JSON. Ele foi construído em torno de
uma única propriedade inegociável: **sobreviver a `kill -9` / queda de energia
em qualquer instante sem corromper dados**, no máximo perdendo transações que
ainda não foram confirmadas ao cliente.

## Fluxo de dados

```
Cliente HTTP (qualquer linguagem)
        │  REST/JSON
        ▼
Servidor ZadoDB (binário único)
   ├── Write path  → sequencer → WAL append (fsync) → overlay em memória
   └── Read path   → overlay → snapshot mmap (MVCC)
        │
        ▼
Disco
   ├── wal.log                (append-only, CRC32C por registro)
   ├── data.NNNNNN.zdb        (B+Tree copy-on-write, mmap para leitura)
   └── CURRENT                (ponteiro atômico para a geração ativa)
```

## Componentes

| Camada | Pacote | Papel |
|---|---|---|
| Página | `internal/storage/page` | Formato fixo 4KB, checksum CRC32C, pager sequencial |
| WAL | `internal/storage/wal` | Registro com checksum, fsync, sequencer (único ponto serial) |
| B+Tree | `internal/storage/btree` | Árvore copy-on-write serializada em páginas, valores overflow |
| Leitura | `internal/storage/mvcc` | Snapshot imutável via mmap, troca atômica |
| Checkpoint | `internal/storage/checkpoint` | Dobra o WAL numa nova geração + publicação atômica |
| Recovery | `internal/storage/recovery` | Reconstrói o estado no boot; completa checkpoint interrompido |
| IDs | `internal/storage/idgen` | Auto-incremento por classe |
| Engine | `internal/storage` | Integra tudo; overlay read-after-write; CRUD |
| Servidor | `internal/server/http` | API REST/JSON |
| Config | `internal/server/config` | YAML + defaults |
| Daemon | `internal/server/daemon` | Windows Service / systemd |

## A regra de ouro

> Nunca mutar o arquivo de dados em uso. Todo write vai primeiro ao WAL
> (append-only, confirmado só após fsync físico). O arquivo de dados só muda por
> checkpoint, que escreve uma **geração nova** via copy-on-write e a publica com
> troca atômica do ponteiro `CURRENT`.

Consequência: o pior caso após um kill é a perda das últimas transações **não
confirmadas** ao cliente — nunca corrupção, nunca estado parcial visível.

Leia em seguida:
[storage-engine](storage-engine.md) ·
[wal-format](wal-format.md) ·
[btree-cow](btree-cow.md) ·
[recovery-and-checkpoint](recovery-and-checkpoint.md) ·
[mvcc](mvcc.md) ·
[concurrency-model](concurrency-model.md)
