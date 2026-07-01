# CLAUDE.md — orientação para agentes de código no ZadoDB

Este arquivo orienta agentes (Claude Code e afins) que trabalham neste repositório.
Leia-o antes de modificar qualquer coisa no storage engine.

## O que é o ZadoDB

Banco de dados orientado a objetos, portável, escrito em Go, com binário único
(sem runtime externo). Requisitos centrais que guiam **toda** decisão de design:

1. Escritas concorrentes reais (não serializa a aplicação como o SQLite).
2. Portabilidade: um binário estático, idêntico em Windows e Linux.
3. Execução em background como serviço (Windows Service / systemd).
4. **Resistência a `kill -9` / queda de energia em qualquer ponto, sem corromper dados.**
5. Atomicidade: toda escrita é tudo-ou-nada.
6. Leitura rápida via `mmap` (snapshot MVCC), sem tocar o write path.
7. Modelo orientado a objeto exposto por API REST (JSON no wire).

## Regra de ouro (NUNCA quebre)

> Nunca mutar o arquivo de dados em uso. Todo write vai primeiro para o WAL
> (append-only, com checksum, confirmado só após `fsync` físico). O arquivo de
> dados só é atualizado por checkpoint, que escreve um **arquivo novo** via
> copy-on-write e troca pelo antigo com **rename atômico**.

Consequência: o pior caso possível após um kill é a perda das últimas
transações que ainda não foram confirmadas ao cliente — **nunca** corrupção nem
estado parcial visível.

## Mapa dos pacotes

| Pacote | Responsabilidade |
|---|---|
| `cmd/zadodb` | Entrypoint único: `serve`, `service`, `version`. |
| `internal/storage/page` | Formato de página 4KB, header, checksum CRC32C, free list. |
| `internal/storage/wal` | Registro do WAL, fsync, sequencer (único ponto de serialização). |
| `internal/storage/btree` | B+Tree copy-on-write serializada em páginas. |
| `internal/storage/mvcc` | Snapshot read-only via mmap; troca atômica pós-checkpoint. |
| `internal/storage/checkpoint` | Aplica WAL num arquivo novo (COW) + rename atômico. |
| `internal/storage/recovery` | Reconstrói o estado no boot; descarta transações não confirmadas. |
| `internal/storage/idgen` | Contador auto-incremento por classe. |
| `internal/storage/engine.go` | Junta tudo; API interna CRUD; overlay read-after-write. |
| `internal/server/http` | API REST (net/http, sem framework). |
| `internal/server/daemon` | Integração Windows Service / systemd. |
| `internal/server/config` | Configuração (YAML + flags + defaults). |
| `internal/testutil` | Gerador de carga + killer de processo + harness de crash. |
| `test/resilience` | Testes de SIGKILL fuzzing e concorrência (build tag `resilience`). |

## Comandos

```sh
go build ./...                                   # compilar tudo
go vet ./...                                      # análise estática
go test -race ./...                               # testes unitários + integração
go test -tags=resilience -timeout=15m ./test/resilience/...  # harness de kill/concorrência

# build cross-platform (binário único, sem deps de runtime):
GOOS=linux   GOARCH=amd64 go build -o dist/zadodb        ./cmd/zadodb
GOOS=windows GOARCH=amd64 go build -o dist/zadodb.exe    ./cmd/zadodb
```

## Estilo e invariantes

- Erros sempre wrapeados: `fmt.Errorf("contexto: %w", err)`.
- Nenhum `panic` no caminho de request HTTP; recover no middleware.
- Todo write path passa pelo sequencer; nenhum outro código escreve no arquivo WAL.
- O read path (`mvcc`) nunca toca o WAL nem o pager de escrita.
- Qualquer mudança no storage engine deve preservar a garantia de corrupção-zero
  sob kill: rode o harness de resiliência antes de considerar a mudança pronta.

## Fluxo de contribuição

- Branch a partir de `main`; **nunca** commitar direto em `main`.
- Commits pequenos e coesos, mensagem no padrão convencional
  (`feat(storage/page): ...`, `test(wal): ...`, `docs: ...`).
- Abrir PR para revisão; não fazer push/PR sem aprovação explícita.
