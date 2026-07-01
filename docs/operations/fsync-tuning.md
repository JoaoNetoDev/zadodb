# Ajuste de fsync (durabilidade vs. throughput)

O ZadoDB só confirma uma escrita ao cliente (HTTP 201) depois que ela está
fisicamente no disco (`fsync` / `FlushFileBuffers`). O **modo de fsync** controla
o trade-off entre durabilidade máxima e throughput.

## `per-commit` (padrão, seguro)

- Um `fsync` por commit, antes de confirmar.
- **Garantia**: um write confirmado **nunca** é perdido, nem sob `kill -9` /
  queda de energia.
- **Custo**: latência de um fsync por escrita. O throughput de escrita fica
  limitado pela taxa de fsync do disco (dezenas de milhares/s em SSD/NVMe,
  centenas/s em HDD).

Use este modo quando não pode perder nenhuma transação confirmada (o padrão).

## `group-commit` (mais throughput)

- Vários commits concorrentes compartilham um único `fsync` (batch por
  `interval_ms` ou `max_batch`).
- **Garantia**: writes confirmados juntos num batch são duráveis juntos após o
  fsync compartilhado. Existe uma janela minúscula: um write cujo fsync do batch
  ainda não retornou não está confirmado (e portanto o cliente ainda não recebeu
  201). Ou seja, **um 201 continua significando durável** — a diferença é a
  latência/agrupamento, não a garantia.
- **Ganho**: sob alta concorrência, o throughput por fsync cresce muito (um
  fsync confirma dezenas de writes).

Use quando tem muitos writers concorrentes e quer maximizar throughput,
mantendo a garantia de que um 201 é durável.

## Como escolher

| Situação | Modo |
|---|---|
| Poucos writers, durabilidade acima de tudo | `per-commit` |
| Muitos writers concorrentes, quer throughput | `group-commit` |
| HDD lento, muitas escritas pequenas | `group-commit` |

Configure via `fsync:` no YAML ou `--fsync` na linha de comando. Parâmetros do
batch em `group_commit.interval_ms` / `group_commit.max_batch`.

> Importante: em ambos os modos, **nunca** há corrupção após um crash. O modo só
> afeta quantas transações *ainda não confirmadas* podem ser perdidas — e por
> construção, uma transação confirmada (201 recebido) já é durável nos dois.
