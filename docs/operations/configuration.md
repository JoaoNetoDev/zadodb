# Configuração

A configuração vem de um arquivo YAML (opcional) sobreposto por defaults e por
flags de linha de comando. Precedência: **flag > arquivo > default**.

## Arquivo YAML

```yaml
# zadodb.yaml
data_dir: /var/lib/zadodb
http_addr: 127.0.0.1:7373

# per-commit (padrão, mais seguro) ou group-commit (mais throughput)
fsync: per-commit

group_commit:
  interval_ms: 2       # janela máxima para acumular um batch
  max_batch: 256       # registros máximos por batch

checkpoint:
  wal_bytes: 67108864  # dispara checkpoint quando o WAL passa disso (64 MiB)
  interval_sec: 300    # checkpoint periódico se há escritas pendentes (5 min)
```

Rodar com o arquivo:

```sh
zadodb serve --config /etc/zadodb/zadodb.yaml
```

## Defaults

| Chave | Default |
|---|---|
| `data_dir` | `./data` |
| `http_addr` | `127.0.0.1:7373` |
| `fsync` | `per-commit` |
| `group_commit.interval_ms` | `2` |
| `group_commit.max_batch` | `256` |
| `checkpoint.wal_bytes` | `67108864` (64 MiB) |
| `checkpoint.interval_sec` | `300` |

## Overrides por flag

```sh
zadodb serve --data-dir ./data --http-addr 0.0.0.0:9000 --fsync group-commit
```

Um arquivo ausente não é erro: os defaults são usados. Um `fsync` inválido é
rejeitado na validação.

Sobre o trade-off de fsync, ver [fsync-tuning](fsync-tuning.md).
