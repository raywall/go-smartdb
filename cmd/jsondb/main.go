// jsondb expõe arquivos JSON como um banco de dados MySQL.
//
// Qualquer cliente compatível com MySQL (DBeaver, DataGrip, mysql CLI,
// drivers JDBC/ODBC, Go, Python...) pode conectar e consultar os dados com SQL.
//
//	jsondb -path ./dados                  # diretório: cada .json vira uma tabela
//	jsondb -path agentes.json -port 3307  # arquivo único
//	jsondb -path ./dados -watch -persist  # recarrega e grava alterações no disco
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	sqle "github.com/dolthub/go-mysql-server"
	"github.com/dolthub/go-mysql-server/memory"
	"github.com/dolthub/go-mysql-server/server"
	"github.com/dolthub/go-mysql-server/sql"
	"github.com/sirupsen/logrus"

	"github.com/raywall/go-dbserver-plugin/internal/catalog"
)

// multiFlag permite repetir -path várias vezes.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*m = append(*m, p)
		}
	}
	return nil
}

var version = "dev"

func main() {
	var paths multiFlag
	flag.Var(&paths, "path", "arquivo .json/.ndjson/.jsonl ou diretório (pode repetir ou separar por vírgula)")
	host := flag.String("host", "127.0.0.1", "endereço de escuta (use 0.0.0.0 para aceitar conexões externas)")
	port := flag.Int("port", 3306, "porta TCP do protocolo MySQL")
	dbName := flag.String("db", "jsondb", "nome do banco que recebe os arquivos do nível raiz")
	user := flag.String("user", "root", "usuário aceito na conexão")
	password := flag.String("password", "", "senha do usuário (vazio = sem senha)")
	caseSensitive := flag.Bool("case-sensitive", false, "comparações de texto sensíveis a maiúsculas/acentos (collation binária)")
	pk := flag.String("pk", "id", "coluna usada como chave primária quando única e não nula (vazio desativa)")
	readOnly := flag.Bool("readonly", false, "rejeita INSERT/UPDATE/DELETE/DDL")
	persist := flag.Bool("persist", false, "grava de volta nos arquivos JSON as alterações feitas via SQL")
	watch := flag.Bool("watch", false, "recarrega tabelas quando os arquivos mudam no disco")
	interval := flag.Duration("interval", 2*time.Second, "intervalo de verificação para -watch e -persist")
	verbose := flag.Bool("v", false, "log detalhado (inclui consultas do motor SQL)")
	showVersion := flag.Bool("version", false, "mostra a versão e sai")
	flag.Usage = usage
	flag.Parse()

	if *showVersion {
		fmt.Println("jsondb", version)
		return
	}
	if len(paths) == 0 {
		paths = append(paths, flag.Args()...)
	}
	if len(paths) == 0 {
		usage()
		os.Exit(2)
	}
	if *persist && *readOnly {
		fail("as flags -persist e -readonly são mutuamente exclusivas")
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
		logrus.SetLevel(logrus.DebugLevel)
	} else {
		logrus.SetLevel(logrus.WarnLevel) // o motor SQL é bem verboso em INFO
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cat, err := catalog.New(catalog.Options{
		Paths:         paths,
		DefaultDB:     *dbName,
		PKColumn:      *pk,
		CaseSensitive: *caseSensitive,
	}, log)
	if err != nil {
		fail(err.Error())
	}

	// LOAD DATA / SELECT INTO OUTFILE ficam restritos a um diretório inexistente.
	_ = sql.SystemVariables.AssignValues(map[string]interface{}{"secure_file_priv": os.DevNull + "/jsondb-disabled"})
	engine := sqle.NewDefault(cat.Provider)
	engine.ReadOnly.Store(*readOnly)

	// Autenticação: cria o usuário informado com acesso total, via qualquer host.
	mysqlDb := engine.Analyzer.Catalog.MySQLDb
	ed := mysqlDb.Editor()
	mysqlDb.AddSuperUser(ed, *user, "%", *password)
	mysqlDb.AddSuperUser(ed, *user, "localhost", *password)
	ed.Close()

	addr := fmt.Sprintf("%s:%d", *host, *port)
	srv, err := server.NewServer(server.Config{
		Protocol: "tcp",
		Address:  addr,
		Version:  "8.0.33-jsondb", // drivers JDBC usam a versão para escolher recursos
	}, engine, sql.NewContext, memory.NewSessionBuilder(cat.Provider), nil)
	if err != nil {
		fail(err.Error())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	syncDone := make(chan struct{})
	go func() {
		cat.Run(ctx, *interval, *watch, *persist)
		close(syncDone)
	}()

	go func() {
		<-ctx.Done()
		log.Info("encerrando...")
		_ = srv.Close()
	}()

	printBanner(cat, addr, *dbName, *user, *password, *readOnly, *persist, *watch)
	if err := srv.Start(); err != nil && ctx.Err() == nil {
		fail(err.Error())
	}
	stop()
	<-syncDone
}

func printBanner(cat *catalog.Catalog, addr, db, user, pass string, ro, persist, watch bool) {
	tables := cat.Describe()
	sort.Strings(tables)
	mode := "leitura e escrita em memória"
	switch {
	case ro:
		mode = "somente leitura"
	case persist:
		mode = "leitura e escrita, persistindo nos arquivos"
	}
	pw := "(sem senha)"
	if pass != "" {
		pw = "(com senha)"
	}
	fmt.Fprintf(os.Stderr, "\njsondb %s ouvindo em %s  [protocolo MySQL]\n", version, addr)
	fmt.Fprintf(os.Stderr, "  usuário: %s %s | banco padrão: %s | modo: %s | watch: %v\n", user, pw, db, mode, watch)
	fmt.Fprintf(os.Stderr, "  tabelas (%d):\n", len(tables))
	for _, t := range tables {
		fmt.Fprintf(os.Stderr, "    - %s\n", t)
	}
	fmt.Fprintf(os.Stderr, "\n  JDBC: jdbc:mysql://%s/%s?useSSL=false&allowPublicKeyRetrieval=true\n\n", addr, db)
}

func usage() {
	fmt.Fprintf(os.Stderr, `jsondb - servidor MySQL sobre arquivos JSON

Uso:
  jsondb -path <arquivo|diretório> [opções]

Cada arquivo vira uma tabela (nome do arquivo sem extensão). Subdiretórios
viram bancos separados. Formatos aceitos:
  [ {...}, {...} ]            array de objetos
  { "itens": [ {...} ] }      objeto com um único array
  {...}\n{...}                NDJSON (.ndjson / .jsonl)

Opções:
`)
	flag.PrintDefaults()
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "erro:", msg)
	os.Exit(1)
}
