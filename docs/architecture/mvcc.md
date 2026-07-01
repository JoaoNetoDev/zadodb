# MVCC e caminho de leitura

Pacote `internal/storage/mvcc` + o overlay do engine (`internal/storage`).

## Snapshot via mmap

Um `Snapshot` é uma visão **imutável** de uma geração de dados, mapeada em
memória read-only (`mmap`). Ler = navegar a B+Tree diretamente sobre o slice
mmap (dereferências na page cache do SO, sem parse de disco, sem lock). O
snapshot **nunca** toca o WAL nem o caminho de escrita.

Valores são copiados para fora do mapeamento ao serem devolvidos, então nunca
apontam para dentro do mmap depois que o handler termina.

## Troca atômica com contagem de referência

`MappedFile` guarda o snapshot ativo atrás de um ponteiro atômico. Depois de um
checkpoint publicar uma nova geração, `SwapTo` instala um snapshot novo; o antigo
só é desmapeado quando seu **último leitor em voo** o libera (contagem de
referência + `sync.Once` no unmap). Isso evita desmapear a memória sob os pés de
um leitor. `TestConcurrentReadsDuringSwap` exercita 16 leitores contra 500
trocas sob `-race`.

## Overlay: consistência leitura-após-escrita

Escritas confirmadas no WAL mas ainda não dobradas por um checkpoint vivem num
**overlay em memória** no engine: `chave → {txID, dados, deleted}`.

- **Leitura** consulta o overlay primeiro; se não achar, cai para o snapshot
  mmap. Assim, um objeto recém-criado é imediatamente visível (read-after-write)
  mesmo antes do próximo checkpoint.
- **Escrita** faz `sequencer.Submit` (bloqueia só até o fsync daquele commit) e,
  na mesma seção crítica, grava no overlay — então leituras nunca veem um "vão"
  onde a chave não está nem no overlay nem no snapshot.
- **Checkpoint** poda o overlay **depois** da troca do snapshot, removendo as
  entradas com `txID ≤ foldedTxID` (agora presentes na nova geração). Fazer a
  poda depois da troca garante que a chave nunca fique ausente de ambos.

No boot, o overlay é reconstruído do zero replayando o WAL a partir do
`LastAppliedTxID` da geração ativa — nunca precisa ser persistido.

## Listagem

`ListObjects` faz um scan de prefixo no snapshot e depois aplica os deltas do
overlay (updates/deletes/inserts) para a classe, ordena por id e pagina. É
`O(overlay + tamanho da classe)` por chamada — adequado ao MVP.
