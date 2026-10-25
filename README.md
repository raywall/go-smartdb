# go-smartdb

Conector local que expõe arquivos **JSON, CSV e YAML como tabelas de um servidor MySQL**. Aponte para um arquivo ou diretório e conecte com DBeaver, Beekeeper Studio, DataGrip, `mysql` CLI ou qualquer driver MySQL — cada arquivo vira uma tabela consultável com SQL completo (JOIN, GROUP BY, subconsultas, funções JSON, views…).

```
agentes.json ──┐                      ┌─ DBeaver / Beekeeper Studio
vendas.csv ────┼─► smartdb (protocolo ───┼─ mysql CLI
rh/*.yaml ─────┘     MySQL :3306)      └─ JDBC / Go / Python / BI
```

O motor SQL é o [go-mysql-server](https://github.com/dolthub/go-mysql-server) (o mesmo do Dolt), executando em memória.

## Instalação

```bash
go mod tidy
go build -o bin/smartdb ./cmd/smartdb
# ou: make build
```

Requer Go 1.24+.

## Releases automáticas

O workflow [Release](.github/workflows/release.yml) executa a cada push na branch
`main`. Ele verifica as dependências, executa `go vet` e os testes com detector de
concorrência e compila os seis destinos antes de publicar uma GitHub Release.

As versões seguem `v0.1.<número da execução>`, começando em `v0.1.1`. Execuções
que falham podem deixar lacunas na numeração. Reexecutar uma execução mantém a
mesma versão; uma release já publicada é preservada.

| Sistema | Arquiteturas | Pacote | Executável |
|---|---|---|---|
| Linux | `amd64`, `arm64` | `.tar.gz` | `smartdb` |
| macOS (`darwin`) | `amd64` (Intel), `arm64` (Apple Silicon) | `.tar.gz` | `smartdb` |
| Windows | `amd64`, `arm64` | `.zip` | `smartdb.exe` |

Cada pacote inclui o README e os dados de exemplo. A release também inclui
`checksums.txt`, com os hashes SHA-256 dos seis pacotes, e notas geradas a partir
do histórico do GitHub. Os executáveis recebem a versão da release em `-version`.
Os builds usam compilação cruzada sem CGO; os testes executam em Linux amd64.

Para instalar sem Go, baixe o pacote da sua plataforma na aba **Releases** do
repositório, extraia e execute:

```bash
./smartdb -path ./exemplos/dados -watch
# Windows (PowerShell): .\smartdb.exe -path .\exemplos\dados -watch
```

O workflow usa apenas o `GITHUB_TOKEN` automático, com escrita em conteúdo
restrita ao job de publicação. Não exige um token pessoal ou secrets adicionais.
As políticas do repositório devem permitir ao GitHub Actions criar releases e
tags. Os pacotes não incluem assinatura de código ou notarização do macOS.

## Uso

```bash
# um arquivo
./bin/smartdb -path agentes.json

# um diretório (cada .json/.ndjson/.jsonl/.csv/.yaml/.yml vira tabela; subdiretórios viram bancos)
./bin/smartdb -path ./exemplos/dados

# vários caminhos, porta e senha
./bin/smartdb -path ./dados,./outros.json -port 3307 -user admin -password s3nh4

# recarregar quando o arquivo mudar e gravar de volta o que for alterado via SQL
./bin/smartdb -path ./dados -watch -persist
```

| Flag | Padrão | Descrição |
|---|---|---|
| `-path` | — | Arquivo ou diretório. Pode repetir ou separar por vírgula. |
| `-host` | `127.0.0.1` | Endereço de escuta. Use `0.0.0.0` para acesso pela rede. |
| `-port` | `3306` | Porta do protocolo MySQL. |
| `-db` | `smartdb` | Banco que recebe os arquivos do nível raiz. |
| `-user` / `-password` | `root` / vazio | Credenciais aceitas. |
| `-pk` | `id` | Coluna usada como chave primária, se for única e não nula. Vazio desativa. |
| `-readonly` | `false` | Rejeita INSERT/UPDATE/DELETE/DDL. |
| `-persist` | `false` | Grava as alterações feitas via SQL de volta nos arquivos. |
| `-watch` | `false` | Recarrega tabelas quando os arquivos mudam e carrega arquivos novos. |
| `-interval` | `2s` | Frequência de verificação para `-watch`/`-persist`. |
| `-case-sensitive` | `false` | Comparações de texto binárias. O padrão ignora maiúsculas e acentos, como o MySQL 8. |
| `-csv-delimiter` | `auto` | Detecta `,`, `;`, tabulação ou pipe. Use `tab` ou um caractere para fixar o separador. |
| `-csv-no-header` | `false` | CSV sem cabeçalho: gera `column_1`, `column_2`, etc. |
| `-v` | `false` | Log detalhado. |

## Conectando pelo DBeaver ou Beekeeper Studio

1. Crie uma conexão **MySQL** no DBeaver ou Beekeeper Studio.
2. Host `127.0.0.1`, porta `3306` (ou a da flag), banco `smartdb`, usuário `root`, senha conforme `-password`.
3. No DBeaver, em **Driver properties**: `useSSL=false` e `allowPublicKeyRetrieval=true`. No Beekeeper, desative SSL para o servidor local.
4. Conectar. As tabelas aparecem em `smartdb` e cada subdiretório aparece como outro banco.

URL JDBC equivalente: `jdbc:mysql://127.0.0.1:3306/smartdb?useSSL=false&allowPublicKeyRetrieval=true`

Com `-persist`, editar células no grid do DBeaver e salvar grava no arquivo de origem na próxima sincronização (tabelas com PK são editáveis no grid).

## Mapeamento dos arquivos → SQL

**Formatos aceitos**

| Arquivo | Resultado |
|---|---|
| `[ {...}, {...} ]` | uma linha por objeto |
| `{ "agentes": [ {...} ] }` | objeto com um único array: o array vira a tabela |
| `{...}` | tabela de uma linha |
| `.ndjson` / `.jsonl` | uma linha por linha do arquivo |
| `["a", "b"]` | coluna única `value` |

**CSV** (`.csv`, UTF-8, com ou sem BOM)

A primeira linha fornece os nomes das colunas. Campos entre aspas podem conter separadores, aspas escapadas e quebras de linha. Cada registro deve ter a mesma quantidade de campos. Cabeçalhos vazios ou duplicados (ignorando maiúsculas) são rejeitados com erro.

O separador é detectado entre vírgula, ponto e vírgula, tabulação e pipe. Para outros dialetos ou arquivos sem cabeçalho:

```bash
./bin/smartdb -path ./dados.csv -csv-delimiter ';'
./bin/smartdb -path ./dados.csv -csv-delimiter tab -csv-no-header
```

As opções CSV se aplicam a todos os CSVs da execução. Campos vazios viram `NULL`. A inferência considera a coluna inteira: números usam ponto decimal; `true`/`false` viram booleanos; códigos como `00123` e colunas que misturam números com texto permanecem texto. Datas permanecem texto. Um CSV somente com cabeçalho mantém as colunas sem linhas.

**YAML** (`.yaml` / `.yml`)

Sequências, mapas e envelopes seguem o mesmo mapeamento do JSON. Um escalar vira uma linha com a coluna `value`. Múltiplos documentos separados por `---` são concatenados na mesma tabela, aplicando o mapeamento a cada documento. Âncoras, aliases e merges são resolvidos; objetos e listas aninhados ficam em colunas JSON. Mapas precisam de chaves textuais; números não finitos (`.nan` / `.inf`) são rejeitados. Timestamps YAML são expostos como texto RFC 3339; strings entre aspas preservam seu texto.

**Tipos inferidos** (considerando todas as linhas)

| Valores no JSON/YAML | Tipo SQL |
|---|---|
| inteiros | `BIGINT` |
| inteiros e decimais | `DOUBLE` |
| `true`/`false` | `BOOLEAN` (`TINYINT(1)`) |
| strings | `LONGTEXT` (`VARCHAR(255)` se for PK) |
| objetos, arrays ou tipos misturados | `JSON` |

Chaves ausentes em algumas linhas ou com `null` geram colunas anuláveis. As colunas seguem a ordem em que aparecem no arquivo.

**Nomes**: `vendas 2024.json` → tabela `vendas_2024`; subdiretório `rh/` → banco `rh`.

## Exemplos de consultas

```sql
SELECT nome, base->>'$.cidade' AS cidade
FROM agentes
WHERE ativo AND JSON_CONTAINS(skills, '"idiomas"');

SELECT a.nome, COUNT(m.id) AS missoes
FROM agentes a LEFT JOIN missoes m ON m.agente_id = a.id
GROUP BY a.nome;

SELECT * FROM rh.departamentos;

-- JOIN entre JSON (agentes), CSV (vendas) e YAML (rh.lotacoes)
SELECT a.nome, l.departamento_id, SUM(v.valor) AS total
FROM smartdb.agentes a
JOIN smartdb.vendas v ON v.agente_id = a.id
JOIN rh.lotacoes l ON l.agente_id = a.id
GROUP BY a.nome, l.departamento_id
ORDER BY total DESC;

-- campos aninhados como colunas via view
CREATE VIEW agentes_flat AS
SELECT id, nome, base->>'$.cidade' AS cidade, base->>'$.uf' AS uf FROM agentes;
```

## Persistência (`-persist`)

- Tabelas alteradas são reescritas no **formato de origem**: JSON, NDJSON, CSV ou YAML. Arquivos sem alteração nunca são reescritos.
- CSV preserva separador, presença de cabeçalho, ordem original das colunas existentes e quebras CRLF. Novas colunas são acrescentadas ao final. `NULL` é escrito como campo vazio.
- YAML preserva sequência, envelope, objeto ou fluxo de documentos. A regravação normaliza a apresentação com indentação de 2 espaços; comentários, âncoras e estilo de aspas não são preservados. Em fluxos, cada linha SQL passa a ser um documento. Se um objeto único passar a ter zero ou várias linhas, será gravado como sequência.
- JSON usa indentação de 2 espaços.
- A gravação é atômica (arquivo temporário + rename).
- `CREATE TABLE` em um banco ligado a um diretório cria `<tabela>.json` nesse diretório.
- `DROP TABLE` **não apaga** o arquivo; apenas desvincula a tabela.
- Em JSON/YAML, valores `NULL` são gravados como chave ausente.
- Sem `-persist`, as alterações ficam só em memória e somem ao reiniciar.

## Limitações

- A descoberta percorre o diretório informado e seus subdiretórios imediatos. Arquivos na raiz usam o banco `-db`; por exemplo, `rh/lotacoes.yaml` vira `rh.lotacoes`.
- Arquivos de formatos diferentes com o mesmo nome no mesmo banco (como `vendas.json` e `vendas.csv`) causam erro de colisão.
- Os dados ficam inteiros em memória: indicado para arquivos de até algumas centenas de MB.
- O motor em memória não foi feito para muitas escritas concorrentes; para uso de consulta e edição pontual funciona bem.
- `-watch` usa verificação periódica de data de modificação (sem inotify), o que funciona igual em Linux, macOS, Windows e volumes de rede.
- Se uma edição externa mudar a estrutura de um arquivo (nova coluna, tipo diferente), a tabela é recriada com o novo schema; reconecte ou atualize o navegador do DBeaver (F5).

## Estrutura

```
cmd/smartdb/          flags, autenticação e servidor MySQL
internal/source/     descoberta de arquivos, parsing e inferência de schema
internal/catalog/    criação das tabelas, watch e persistência
exemplos/dados/      arquivos de exemplo
```
