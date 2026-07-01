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

## Códigos de erro

| Status | Significado |
|---|---|
| 400 | Corpo inválido / nome de classe inválido / id inválido |
| 404 | Classe ou objeto não existe |
| 409 | Classe já existe / classe não está vazia |
| 500 | Erro interno |

Corpo de erro: `{ "error": "mensagem" }`.

## Collection Postman

Importe [`zadodb.postman_collection.json`](zadodb.postman_collection.json) e o
environment [`zadodb.postman_environment.json`](zadodb.postman_environment.json).
A collection encadeia os requests: criar um objeto captura o `id` em
`{{objectId}}` para os requests seguintes.
