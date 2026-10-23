# jsondb

Servidor que expõe arquivos JSON como um **banco de dados MySQL**. Aponte para um arquivo ou diretório e conecte com DBeaver, DataGrip, `mysql` CLI ou qualquer driver MySQL — cada arquivo vira uma tabela consultável com SQL completo (JOIN, GROUP BY, subconsultas, funções JSON, views…).

```
agentes.json ─┐                        ┌─ DBeaver
missoes.json ─┼─► jsondb (protocolo ───┼─ mysql CLI
rh/*.json ────┘     MySQL :3306)       └─ JDBC / Go / Python / BI
```

O motor SQL é o [go-mysql-server](https://github.com/dolthub/go-mysql-server) (o mesmo do Dolt), executando em memória.

## Instalação

```bash
go mod tidy
go build -o bin/jsondb ./cmd/jsondb
# ou: make build
```

Requer Go 1.24+.

## Uso

```bash
# um arquivo
./bin/jsondb -path agentes.json

# um diretório (cada .json/.ndjson/.jsonl vira tabela; subdiretórios viram bancos)
./bin/jsondb -path ./exemplos/dados

# vários caminhos, porta e senha
./bin/jsondb -path ./dados,./outros.json -port 3307 -user admin -password s3nh4

# recarregar quando o arquivo mudar e gravar de volta o que for alterado via SQL
./bin/jsondb -path ./dados -watch -persist
```

| Flag | Padrão | Descrição |
|---|---|---|
| `-path` | — | Arquivo ou diretório. Pode repetir ou separar por vírgula. |
| `-host` | `127.0.0.1` | Endereço de escuta. Use `0.0.0.0` para acesso pela rede. |
| `-port` | `3306` | Porta do protocolo MySQL. |
| `-db` | `jsondb` | Banco que recebe os arquivos do nível raiz. |
| `-user` / `-password` | `root` / vazio | Credenciais aceitas. |
| `-pk` | `id` | Coluna usada como chave primária, se for única e não nula. Vazio desativa. |
| `-readonly` | `false` | Rejeita INSERT/UPDATE/DELETE/DDL. |
| `-persist` | `false` | Grava as alterações feitas via SQL de volta nos arquivos. |
| `-watch` | `false` | Recarrega tabelas quando os arquivos mudam e carrega arquivos novos. |
| `-interval` | `2s` | Frequência de verificação para `-watch`/`-persist`. |
| `-case-sensitive` | `false` | Comparações de texto binárias. O padrão ignora maiúsculas e acentos, como o MySQL 8. |
| `-v` | `false` | Log detalhado. |

## Conectando pelo DBeaver

1. **Nova conexão → MySQL**.
2. Host `127.0.0.1`, porta `3306` (ou a da flag), banco `jsondb`, usuário `root`, senha conforme `-password`.
3. Em **Driver properties**: `useSSL=false` e `allowPublicKeyRetrieval=true`.
4. Conectar. As tabelas aparecem em `jsondb` e cada subdiretório aparece como outro banco.

URL JDBC equivalente: `jdbc:mysql://127.0.0.1:3306/jsondb?useSSL=false&allowPublicKeyRetrieval=true`

Com `-persist`, editar células no grid do DBeaver e salvar grava direto no arquivo JSON (tabelas com PK são editáveis no grid).

## Mapeamento JSON → SQL

**Formatos aceitos**

| Arquivo | Resultado |
|---|---|
| `[ {...}, {...} ]` | uma linha por objeto |
| `{ "agentes": [ {...} ] }` | objeto com um único array: o array vira a tabela |
| `{...}` | tabela de uma linha |
| `.ndjson` / `.jsonl` | uma linha por linha do arquivo |
| `["a", "b"]` | coluna única `value` |

**Tipos inferidos** (considerando todas as linhas)

| Valores no JSON | Tipo SQL |
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

-- campos aninhados como colunas via view
CREATE VIEW agentes_flat AS
SELECT id, nome, base->>'$.cidade' AS cidade, base->>'$.uf' AS uf FROM agentes;
```

## Persistência (`-persist`)

- Tabelas alteradas são reescritas no **formato original** (array, objeto envelope ou NDJSON), com indentação de 2 espaços. Arquivos sem alteração nunca são reescritos.
- A gravação é atômica (arquivo temporário + rename).
- `CREATE TABLE` em um banco ligado a um diretório cria `<tabela>.json` nesse diretório.
- `DROP TABLE` **não apaga** o arquivo; apenas desvincula a tabela.
- Valores `NULL` são gravados como chave ausente.
- Sem `-persist`, as alterações ficam só em memória e somem ao reiniciar.

## Limitações

- Os dados ficam inteiros em memória: indicado para arquivos de até algumas centenas de MB.
- O motor em memória não foi feito para muitas escritas concorrentes; para uso de consulta e edição pontual funciona bem.
- `-watch` usa verificação periódica de data de modificação (sem inotify), o que funciona igual em Linux, macOS, Windows e volumes de rede.
- Se uma edição externa mudar a estrutura de um arquivo (nova chave, tipo diferente), a tabela é recriada com o novo schema; reconecte ou atualize o navegador do DBeaver (F5).

## Estrutura

```
cmd/jsondb/          flags, autenticação e servidor MySQL
internal/source/     descoberta de arquivos, parsing e inferência de schema
internal/catalog/    criação das tabelas, watch e persistência
exemplos/dados/      arquivos de exemplo
```
