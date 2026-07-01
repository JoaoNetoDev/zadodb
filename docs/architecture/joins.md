# JOINs por semi-joins em cadeia

O ZadoDB permite filtrar objetos de uma classe por campos de classes
**relacionadas**, sem materializar produtos cartesianos e sem índice secundário.
Este documento descreve como o resolvedor funciona. Para a interface REST
(endpoints de relação e sintaxe `eq.<rel>.<campo>` / `like.<rel>.<campo>`), ver
[api/rest-api](../api/rest-api.md).

## Grafo de relações

Cada relação registrada numa classe é uma **aresta dirigida** origem → destino,
identificada por um **alias** (`name`, por default o nome da classe destino) e
carregando o par de campos `localField → remoteField`. O conjunto de todas as
relações de um project forma um **grafo dirigido** de classes.

Um filtro de relação `eq.<rel>.<campo>` nomeia, pelo alias `<rel>`, a classe onde
o campo `<campo>` deve ser avaliado. O primeiro passo é achar **como chegar** da
classe base até essa classe.

## BFS do caminho

Para cada alias citado na consulta, o servidor faz uma **busca em largura (BFS)**
no grafo, partindo da classe base, seguindo arestas de relação até alcançar a
classe do alias. O resultado é o **caminho** (sequência de arestas) da base até o
alvo.

- Vários saltos são resolvidos automaticamente: `logradouro → municipio → uf` é
  um caminho de duas arestas encontrado pela BFS.
- Se **não existe caminho** (alias desconhecido ou classe inalcançável), a
  consulta falha com `400 Bad Request`.
- Havendo **múltiplas relações para a mesma classe destino**, a resolução usa a
  **primeira** encontrada.

## Execução: semi-joins do fim para a base

Achado o caminho, o predicado sobre a classe remota é avaliado como uma
**cadeia de semi-joins**, executada **do fim da cadeia para a base**:

1. Aplica-se o predicado (`eq`/`like`, com folding conforme `ci`/`ai`) na
   **última** classe do caminho e coleta-se o conjunto de valores do campo que
   liga essa classe à anterior (o `remoteField` da aresta).
2. Esse conjunto vira um filtro na classe anterior: mantêm-se apenas os objetos
   cujo `localField` está no conjunto; deles coleta-se o campo de ligação para a
   classe anterior a essa.
3. Repete-se até chegar à **classe base**, onde o conjunto resultante restringe
   quais objetos base podem casar.

Ou seja, cada salto reduz um conjunto de chaves candidatas, e nunca se constrói
um produto cartesiano — apenas conjuntos de valores de chave passam de uma classe
para a próxima.

## Custo

Cada classe envolvida é varrida uma vez (**O(n)** por classe), pois **não há
índice secundário** (fase futura, será PR próprio). Na prática, as classes
relacionadas de um caminho costumam ser pequenas (`municipio`, `uf`), então o
custo total é dominado pelo scan da **classe base**. Os semi-joins mantêm o pico
de memória proporcional aos conjuntos de chaves candidatas, não ao produto das
classes.

O scan da classe base reusa o mesmo caminho de consulta em **streaming**
(merge do snapshot mmap com o overlay em memória, ordenado por id) descrito em
[storage-engine](storage-engine.md#consulta--paginação) — por isso joins e
classes grandes não estouram a RAM.
