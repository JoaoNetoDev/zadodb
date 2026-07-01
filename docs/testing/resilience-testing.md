# Testes de resiliência (SIGKILL fuzzing)

A propriedade central do ZadoDB — sobreviver a `kill -9` sem corrupção — é
verificada por um harness que mata o processo de verdade, sob carga de escrita
concorrente, desde o início.

## Rodar

```sh
go test -tags=resilience -timeout=15m ./test/resilience/...
```

Os testes ficam atrás da build tag `resilience` para não pesar no
`go test ./...` do dia a dia. O CI roda os dois (ver `.github/workflows/ci.yml`).

## O que o SIGKILL fuzz faz (`TestSIGKILLFuzz`)

Por 20 ciclos:

1. Sobe o binário `zadodb serve` num subprocesso (fsync `per-commit`).
2. Dispara 16 writers HTTP concorrentes criando objetos; cada objeto carrega um
   marcador único `{"n": N}`.
3. Após um intervalo aleatório (50–400 ms), **mata o processo à força**
   (`os.Process.Kill` → SIGKILL no Linux / TerminateProcess no Windows — sem
   shutdown gracioso).
4. Reinicia o servidor (dispara o recovery).

O harness registra **apenas** as escritas que receberam `201` (ou seja, cujo
fsync já retornou). Ao final, reinicia uma última vez e verifica:

- **Toda** escrita confirmada está presente e com o marcador `n` correto.
- Escritas **não** confirmadas podem faltar — isso é permitido.

Uma escrita confirmada ausente ou com valor trocado seria perda/corrupção de
dados: falha dura.

## Por que isso prova a garantia

O servidor só responde `201` depois que `sequencer.Submit` retorna, o que só
acontece após o `fsync`. Então todo `201` implica durabilidade física. Se o
processo morre logo depois, o dado está no WAL fsyncado; o recovery o reaplica.
Se morre no meio de um checkpoint, o recovery completa ou desfaz o checkpoint
interrompido (ver [recovery-and-checkpoint](../architecture/recovery-and-checkpoint.md)).

Resultado local típico: **10.000+ escritas confirmadas sobrevivem a 20 kills
brutais, nenhuma corrompida**.

## Interpretando falhas

- `N acknowledged writes missing` → uma escrita confirmada sumiu: violação de
  durabilidade. Investigue o caminho de fsync / recovery.
- `mismatched` → um objeto voltou com valor diferente: violação de integridade.
  Investigue serialização / chaves / COW.
- Timeout → possível deadlock ou recovery lento; veja os logs em
  `<data-dir>/server.log`.
