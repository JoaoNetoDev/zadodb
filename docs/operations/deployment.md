# Deployment

O ZadoDB é um binário único, estático, sem dependências de runtime externo.

## Build

```sh
# Para a plataforma atual
go build -o dist/zadodb ./cmd/zadodb        # (dist/zadodb.exe no Windows)

# Cross-compile (o mesmo código roda idêntico nos dois SOs)
GOOS=linux   GOARCH=amd64 go build -o dist/zadodb     ./cmd/zadodb
GOOS=windows GOARCH=amd64 go build -o dist/zadodb.exe ./cmd/zadodb
```

Injetar metadados de versão (opcional):

```sh
go build -ldflags "-X main.version=v0.1.0 -X main.commit=$(git rev-parse --short HEAD)" ./cmd/zadodb
```

## Executar

```sh
zadodb serve --data-dir /var/lib/zadodb --http-addr 127.0.0.1:7373
```

Flags de `serve`:

| Flag | Descrição |
|---|---|
| `--config` | Caminho do YAML de configuração |
| `--data-dir` | Diretório de dados (sobrepõe o config) |
| `--http-addr` | Endereço de escuta HTTP (sobrepõe o config) |
| `--fsync` | `per-commit` ou `group-commit` (sobrepõe o config) |

O diretório de dados é criado se não existir. Na primeira execução, o ZadoDB
inicializa a geração 0 vazia.

## Rodar em background / como serviço

- Linux: [systemd-service](systemd-service.md).
- Windows: [windows-service](windows-service.md).

## Portas e segurança

Por padrão o servidor escuta em `127.0.0.1` (loopback). A API **não** tem
autenticação nesta versão — não a exponha diretamente à internet. Coloque-a
atrás de um proxy/rede confiável se precisar de acesso remoto.
