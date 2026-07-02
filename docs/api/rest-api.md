# API REST

O servidor expõe uma API REST/JSON. **JSON no wire** (universal, consumível por
qualquer linguagem sem lib extra); internamente os objetos são armazenados em
**MessagePack** (compacto). A conversão acontece só na borda.

Base URL padrão: `http://127.0.0.1:7373`

> Nota sobre números: valores JSON numéricos são decodificados como ponto
> flutuante e re-serializados; inteiros voltam como inteiros quando possível.
> Isso é adequado ao MVP.

## Projects (namespace virtual)

Um **project** é um banco de dados virtual: um namespace que agrupa um conjunto
de classes, permitindo trabalhar com conjuntos separados sem misturá-los. A
mesma classe (ex.: `Rua`) pode existir de forma independente em vários projects,
cada um com seus próprios objetos e sua própria sequência de ids.

O project é selecionado pelo header **`X-Zado-Project`** em **qualquer** rota de
classes/objetos. As rotas não mudam — só o header:

```
X-Zado-Project: censo2022
```

- **Header ausente ou vazio ⇒ project padrão** (`""`). O project padrão usa o
  layout de chave legado, então um banco criado antes deste recurso continua
  funcionando sem migração e sem nenhum header.
- Nomes de project seguem a mesma regra de nomes de classe
  (`[A-Za-z0-9_.-]`, 1–128 chars).
- O isolamento vale para todas as operações: criar/listar/remover classe,
  CRUD de objeto, bulk e filtros de consulta.

### `GET /v1/projects` — lista os projects que possuem classes
```json
200 { "projects": ["", "censo2020", "censo2022"] }
```
O project padrão aparece como `""` quando tem ao menos uma classe.

Exemplo — criar a classe `Rua` no project `censo2022` e inserir um objeto:
```sh
curl -X POST http://127.0.0.1:7373/v1/classes \
  -H 'X-Zado-Project: censo2022' -H 'Content-Type: application/json' \
  -d '{"name":"Rua"}'

curl -X POST http://127.0.0.1:7373/v1/classes/Rua/objects \
  -H 'X-Zado-Project: censo2022' -H 'Content-Type: application/json' \
  -d '{"nome":"Rua das Flores"}'
```

## Saúde e métricas

### `GET /v1/health`
```json
200 { "status": "ok" }
```

### `GET /v1/stats`
```json
200 {
  "last_tx_id": 42, "wal_bytes": 3512, "active_gen": 3,
  "num_classes": 2, "overlay_size": 5, "checkpoints": 3,
  "last_checkpoint": "2026-06-30T21:00:00Z"
}
```

## Classes

### `POST /v1/classes`  — cria uma classe
```json
// request
{ "name": "Pessoa" }
// 201
{ "name": "Pessoa" }
```
Erros: `400` nome inválido/ausente, `409` já existe.

Nomes válidos: letras, dígitos, `_`, `-`, `.` (até 128 chars).

### `GET /v1/classes` — lista classes
```json
200 { "classes": ["Filial", "Pessoa"] }
```

### `GET /v1/classes/{class}` — detalhe
```json
200 { "name": "Pessoa" }      // 404 se não existe
```

### `DELETE /v1/classes/{class}` — remove classe vazia
`204` em sucesso. Erros: `404` não existe, `409` não está vazia.

## Objetos

### `POST /v1/classes/{class}/objects` — cria objeto
O corpo é um objeto JSON arbitrário. O `id` é atribuído pelo servidor
(auto-incremento por classe) e devolvido junto do objeto.
```json
// request
{ "nome": "João", "idade": 30 }
// 201
{ "id": 1, "nome": "João", "idade": 30 }
```
Erros: `404` classe não existe, `400` corpo não é objeto JSON.

### `GET /v1/classes/{class}/objects/{id}` — obtém objeto
```json
200 { "id": 1, "nome": "João", "idade": 30 }   // 404 se não existe
```

### `PUT /v1/classes/{class}/objects/{id}` — substitui objeto
Substitui um objeto **existente** (404 se não existe).
```json
// request
{ "nome": "João Neto", "idade": 31 }
// 200
{ "id": 1, "nome": "João Neto", "idade": 31 }
```

### `DELETE /v1/classes/{class}/objects/{id}` — remove objeto
`204` em sucesso. `404` se não existe.

### `POST /v1/classes/{class}/objects/bulk` — cria objetos em lote (atômico)
Recebe um **array JSON** de objetos e os grava numa **única transação atômica**
(um só registro no WAL, um só `fsync`). É a forma recomendada para ingestão
pesada: elimina o round-trip HTTP por objeto e amortiza o fsync.
```json
// request (array)
[ { "nome": "A" }, { "nome": "B" }, { "nome": "C" } ]
// 201
{ "ids": [1, 2, 3], "count": 3 }
```
Garantia de atomicidade: um `201` significa que **todos** os objetos estão
duráveis. Se ocorrer erro ou crash sem `201`, o lote inteiro fica ausente
(nunca parcial) — reenvie. Limite: 10.000 objetos por request (corpo até
256 MiB). Erros: `404` classe não existe, `400` corpo não é array de objetos ou
lote grande demais.

### `GET /v1/classes/{class}/objects?limit=&offset=` — lista/consulta objetos
Ordenado por `id` crescente. `limit` padrão 100, `offset` padrão 0.
```json
200 {
  "objects": [ {"id":1,"nome":"João"}, {"id":2,"nome":"Maria"} ],
  "count": 2, "limit": 100, "offset": 0
}
```
Erro: `404` classe não existe.

**Filtros (WHERE).** Combináveis por `AND`, aplicados por campo do objeto:

| Param | Significado |
|---|---|
| `eq.<campo>=valor` | Igualdade exata (comparação como string) |
| `like.<campo>=padrão` | SQL LIKE: `%` = qualquer sequência, `_` = um caractere |
| `ci=false` | Opta por case-**sensitive** (o padrão é case-insensitive) |
| `ai=false` | Opta por acento-**sensível** (o padrão é acento-insensível) |

`limit`/`offset` paginam sobre os **resultados** que casaram. Lembre de
URL-encodar o `%` como `%25`.

Exemplos:
```
# nome contendo "nio" e depois "ivo" (ex.: "Antonio Nascivo"):
GET /v1/classes/logradouro/objects?like.nome=%25nio%25ivo%25&limit=100

# UF exata E nome começando por "Rua":
GET /v1/classes/logradouro/objects?eq.uf=SP&like.nome=Rua%25

# case-sensitive:
GET /v1/classes/logradouro/objects?eq.uf=SP&ci=false
```

> **Custo**: não há índice secundário — cada consulta é um *full scan* da
> classe, O(n). Ordem de ~200ms para 100k objetos, ~alguns segundos para
> milhões. Ótimo para buscas ocasionais; para busca rápida e repetida em
> escala, índices secundários são o próximo passo (fase futura).

### Ignorar acento e caixa (folding)

Por **default**, tanto `eq.` quanto `like.` comparam ignorando **caixa** e
**acento** — inclusive nos filtros de relação (ver [JOINs](#joins-consulta-por-campos-de-classes-relacionadas)).
Dois parâmetros de query controlam isso:

| Param | Default | Efeito |
|---|---|---|
| `ci` | `true` | `ci=false` torna a comparação case-**sensitive** |
| `ai` | `true` | `ai=false` torna a comparação acento-**sensível** |

A dobragem (*fold*) é aplicada dos **dois lados** da comparação: faz lowercase e
remove diacríticos latinos (`á à â ã ä ç é ê í ó ô õ ú ü ñ ý` e maiúsculas
correspondentes). Assim `mossoro` casa `Mossoró`, `MoSsoRó`, etc.

```
# padrão: ignora acento e caixa
GET /v1/classes/municipio/objects?eq.nome=mossoro

# exige acento e caixa exatos:
GET /v1/classes/municipio/objects?eq.nome=Mossor%C3%B3&ci=false&ai=false
```

### Paginação keyset (`after` / `next_after`)

Além de `offset`/`limit`, a listagem aceita **`after=<id>`**: retorna os objetos
com id **estritamente maior** que `<id>`, em ordem crescente de id, até `limit`.
Internamente isso é um *seek* na B+Tree (custo O(tamanho-da-página)), em vez de
descartar as primeiras `offset` linhas (O(offset)) — **prefira `after` para
paginação profunda em classes grandes.**

Quando a página vem **cheia** (`count == limit`), a resposta inclui
**`next_after`**, o id do último objeto retornado; passe-o como `?after=` para
pedir a próxima página. Quando a página vem incompleta, `next_after` é omitido
(fim da sequência).

```json
200 {
  "objects": [ {"id":101,"nome":"..."}, {"id":102,"nome":"..."} ],
  "count": 2, "limit": 2, "after": 100, "next_after": 102
}
```

```
# primeira página:
GET /v1/classes/logradouro/objects?limit=1000
# próxima, a partir do next_after devolvido:
GET /v1/classes/logradouro/objects?limit=1000&after=1000
```

`offset`/`limit` continuam funcionando (caminho legado, O(n)). `after` combina
com filtros e joins.

## Relacionamentos (foreign keys)

Uma **relação** liga um campo de uma classe (origem) a um campo de outra classe
(destino). Registra-se a relação **uma vez por classe**; depois as consultas
podem filtrar por campos das classes relacionadas (ver [JOINs](#joins-consulta-por-campos-de-classes-relacionadas)).
Todas as rotas respeitam o header `X-Zado-Project`.

### `POST /v1/classes/{class}/relationships` — cria uma relação
```json
// request
{
  "name": "municipio",
  "localField": "municipioCodigo",
  "toClass": "municipio",
  "remoteField": "codigoIbge"
}
// 201
{
  "name": "municipio",
  "localField": "municipioCodigo",
  "toClass": "municipio",
  "remoteField": "codigoIbge"
}
```

- `name` é **opcional** (default = `toClass`) e é como as consultas referenciam a
  relação (o *alias*).
- `localField` é o campo na classe origem; `remoteField` é o campo na classe
  destino (`toClass`).

Erros: `409` se a relação já existe, `404` se a classe origem **ou** a classe
destino (`toClass`) não existe, `400` corpo inválido.

### `GET /v1/classes/{class}/relationships` — lista relações da classe
```json
200 { "relationships": [ {"name":"municipio","localField":"municipioCodigo","toClass":"municipio","remoteField":"codigoIbge"} ] }
```

### `DELETE /v1/classes/{class}/relationships/{name}` — remove uma relação
`204` em sucesso. `404` se não existe.

Exemplo — modelar o dataset `logradouro → municipio → uf`:
```sh
# logradouro.municipioCodigo -> municipio.codigoIbge
curl -X POST http://127.0.0.1:7373/v1/classes/logradouro/relationships \
  -H 'Content-Type: application/json' \
  -d '{"localField":"municipioCodigo","toClass":"municipio","remoteField":"codigoIbge"}'

# municipio.codigoUf -> uf.codigoUf
curl -X POST http://127.0.0.1:7373/v1/classes/municipio/relationships \
  -H 'Content-Type: application/json' \
  -d '{"localField":"codigoUf","toClass":"uf","remoteField":"codigoUf"}'
```
(Sem `name`, o alias de cada relação vira o nome da classe destino: `municipio` e
`uf`.)

## JOINs: consulta por campos de classes relacionadas

Com relações registradas, a listagem aceita filtros **com ponto** que apontam
para campos de uma classe relacionada, usando o **alias** da relação:

| Param | Significado |
|---|---|
| `eq.<rel>.<campo>=valor` | Igualdade no campo de uma classe relacionada |
| `like.<rel>.<campo>=padrão` | LIKE no campo de uma classe relacionada |

- `<rel>` é o **nome (alias)** da relação — por default o nome da classe destino.
- O servidor faz **BFS no grafo de relações** para achar o caminho da classe base
  até a classe do alias, então **múltiplos saltos** são resolvidos
  automaticamente.
- Filtros **sem ponto** no campo continuam aplicando à **classe base** e podem
  combinar com os de relação.
- Os parâmetros `ci`/`ai` (folding) valem também para os filtros de relação.

Exemplo real (join de 2 saltos): logradouros cujo município começa com "mossor"
no estado (uf.sigla) `RN`:
```
GET /v1/classes/logradouro/objects?eq.uf.sigla=RN&like.municipio.nome=mossor%25
```
Aqui `logradouro` tem `municipioCodigo → municipio.codigoIbge` e `municipio` tem
`codigoUf → uf.codigoUf`; o servidor encadeia os dois saltos sozinho. Como o
folding é o default, "mossor" casa "Mossor..." acento/caixa-insensível.

Erros/limitações:
- **Alias desconhecido ou inalcançável** (não há caminho no grafo) → `400 Bad
  Request`.
- Sem índice secundário: cada salto é um *semi-join* O(n) sobre a classe
  relacionada (fase futura; será PR próprio). Classes relacionadas costumam ser
  pequenas (`municipio`, `uf`), então o custo é dominado pelo scan da classe base.
- Se houver **múltiplas relações para a mesma classe destino**, a resolução usa a
  **primeira** encontrada.

### Trazer os campos do pai (`include`)

Os filtros de relação **filtram**, mas por padrão não trazem os dados do pai. Para
**embutir** o objeto relacionado em cada linha do resultado, use
`include=<rel>[,<rel>...]` (os mesmos nomes de relação dos filtros). Cada relação
listada vira uma chave no objeto retornado, com o **objeto pai completo** (segue
o caminho no grafo, então saltos múltiplos como `uf` via `municipio` também
funcionam):

```
GET /v1/classes/logradouro/objects?eq.uf.sigla=RN&eq.municipio.nome=mossoro&include=municipio,uf&limit=1
```
```json
{
  "objects": [{
    "id": 3234386, "nome": "ANTONIO IVO MARINHO", "municipioCodigo": 2408003,
    "municipio": { "id": 5163, "nome": "Mossoró", "codigoIbge": 2408003, "codigoUf": 24 },
    "uf":        { "id": 11, "nome": "Rio Grande do Norte", "sigla": "RN", "codigoUf": 24 }
  }],
  "count": 1
}
```

As classes relacionadas são carregadas uma vez num mapa por chave (são pequenas);
cada linha resolve o pai por lookup O(1). Se a cadeia de um registro não resolver
(FK órfã), a chave do alias vem como `null`.

Ver [architecture/joins](../architecture/joins.md) para o algoritmo.

## Checkpoint manual

### `POST /v1/checkpoint` — dispara um checkpoint síncrono
Consolida o WAL num arquivo de dados novo (compactação) de forma **síncrona** e
retorna quando concluído:
```json
200 { "checkpoints": 4, "active_gen": 4, "last_checkpoint": "2026-07-01T12:00:00Z" }
```

É a peça complementar do **modo manual** (`checkpoint.manual: true` /
`--checkpoint-manual`), que desabilita o checkpoint automático. O uso ideal é ao
**fim de uma carga em massa**: importe com `--checkpoint-manual` e chame este
endpoint **uma vez** no final. Isso evita re-transmitir a árvore base a cada
rodada de checkpoint durante o import — o gargalo que fazia o checkpoint demorar
~48min num HD USB. Ver
[architecture/recovery-and-checkpoint](../architecture/recovery-and-checkpoint.md).

## Códigos de erro

| Status | Significado |
|---|---|
| 400 | Corpo inválido / nome de classe inválido / id inválido / alias de relação desconhecido ou inalcançável |
| 404 | Classe, objeto ou relação não existe / classe destino inexistente |
| 409 | Classe já existe / classe não está vazia / relação já existe |
| 500 | Erro interno |

Corpo de erro: `{ "error": "mensagem" }`.

## Collection Postman

Importe [`zadodb.postman_collection.json`](zadodb.postman_collection.json) e o
environment [`zadodb.postman_environment.json`](zadodb.postman_environment.json).
A collection encadeia os requests: criar um objeto captura o `id` em
`{{objectId}}` para os requests seguintes.
