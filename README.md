# ZadoDB

**Banco de dados orientado a objetos, portátil, rápido, atômico e resistente a
kill bruto.**

[![License: AGPL v3](https://img.shields.io/badge/License-AGPL_v3-blue.svg)](LICENSE)
![status](https://img.shields.io/badge/status-MVP-orange)
![go](https://img.shields.io/badge/go-1.25-00ADD8)

ZadoDB é um motor de banco de dados escrito em Go, distribuído como **binário
único** (sem runtime externo, sem JVM/.NET, sem instalador), idêntico em Windows
e Linux. Ele expõe uma **API REST/JSON**, então qualquer linguagem consome sem
driver dedicado.

## Por que existe

Nenhuma opção de mercado atendia simultaneamente a estes requisitos:

- **Escritas concorrentes reais** — múltiplos clientes escrevendo ao mesmo tempo
  (diferente do SQLite, que serializa writers).
- **Portabilidade real** — um binário estático, sem dependências de runtime.
- **Execução em background** — roda como serviço (Windows Service / systemd).
- **Resistência a `kill -9` / queda de energia** — sobrevive a interrupção
  brutal em qualquer ponto **sem corromper dados**; no pior caso perde apenas
  transações ainda não confirmadas.
- **Atomicidade** — toda escrita é tudo-ou-nada.
- **Leitura rápida** — via `mmap`, próxima de velocidade de memória.
- **Modelo orientado a objeto** — trabalhe com objetos, não SQL linha a linha.

Como isso é alcançado: WAL com fsync + B+Tree copy-on-write + publicação por
troca atômica de geração + MVCC via mmap. Ver [docs/architecture](docs/architecture/overview.md).

## Quickstart

```sh
# Build (binário único)
go build -o zadodb ./cmd/zadodb          # zadodb.exe no Windows

# Rodar
./zadodb serve --data-dir ./data --http-addr 127.0.0.1:7373
```

Em outro terminal:

```sh
# Criar uma classe
curl -X POST http://127.0.0.1:7373/v1/classes -d '{"name":"Pessoa"}'

# Criar um objeto (id é atribuído pelo servidor)
curl -X POST http://127.0.0.1:7373/v1/classes/Pessoa/objects \
     -d '{"nome":"João","idade":30}'
# -> {"id":1,"nome":"João","idade":30}

# Ler
curl http://127.0.0.1:7373/v1/classes/Pessoa/objects/1

# Listar
curl "http://127.0.0.1:7373/v1/classes/Pessoa/objects?limit=100"
```

Prove a resistência a crash: mate o processo à força (`kill -9` / Stop-Process
-Force) durante escritas e reinicie — todos os dados confirmados continuam lá.

## Cliente

Não há driver dedicado por design: a API é REST/JSON. Use a
[collection Postman](docs/api/zadodb.postman_collection.json) como cliente de
referência (importe também o
[environment](docs/api/zadodb.postman_environment.json)), ou qualquer cliente
HTTP. Referência completa dos endpoints em [docs/api/rest-api.md](docs/api/rest-api.md).

## Rodar como serviço

- **Windows**: `zadodb.exe service install` — ver [docs/operations/windows-service.md](docs/operations/windows-service.md)
- **Linux (systemd)**: `sudo zadodb service install` — ver [docs/operations/systemd-service.md](docs/operations/systemd-service.md)

## Configuração

Arquivo YAML opcional + flags. Padrão seguro (`fsync: per-commit`). Ver
[docs/operations/configuration.md](docs/operations/configuration.md) e
[fsync-tuning](docs/operations/fsync-tuning.md).

## Documentação

- Arquitetura: [overview](docs/architecture/overview.md) ·
  [storage-engine](docs/architecture/storage-engine.md) ·
  [WAL](docs/architecture/wal-format.md) ·
  [B+Tree COW](docs/architecture/btree-cow.md) ·
  [recovery & checkpoint](docs/architecture/recovery-and-checkpoint.md) ·
  [MVCC](docs/architecture/mvcc.md) ·
  [concorrência](docs/architecture/concurrency-model.md)
- API: [rest-api](docs/api/rest-api.md)
- Operação: [deployment](docs/operations/deployment.md) ·
  [configuração](docs/operations/configuration.md) ·
  [fsync](docs/operations/fsync-tuning.md)
- Testes: [resiliência](docs/testing/resilience-testing.md) ·
  [concorrência](docs/testing/concurrency-testing.md)

## Desenvolvimento

```sh
go build ./...                                              # compilar
go test -race ./...                                         # testes (com detector de corrida)
go test -tags=resilience -timeout=15m ./test/resilience/... # harness de SIGKILL + concorrência
```

Convenções e invariantes para contribuidores/agentes em [CLAUDE.md](CLAUDE.md).

## Estado do projeto

MVP funcional e usável. **Fora do escopo desta versão** (fase futura):
particionamento por classe (WAL único por ora), índices secundários, merge de
nós B+Tree / compactação incremental, autenticação, logging estruturado.

## Licença

[GNU AGPL-3.0](LICENSE). Uso irrestrito (estudo, dev, produção) sempre gratuito;
quem modificar e oferecer como serviço de rede deve publicar as modificações.
