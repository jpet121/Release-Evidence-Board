package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"release-evidence-board/internal/board"
	"release-evidence-board/internal/browser"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) (err error) {
	if len(args) == 0 {
		return errors.New("use init, serve, account, config, backup, restore, revalidate, suspend, restore-drill, expire-backups, or purge")
	}
	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stdout)
	dbFile := flags.String("db", "evidence.db", "protected evidence database path")
	project := flags.String("project", "pilot", "project ID")
	product := flags.String("product", "DEMO", "product ID")
	actor := flags.String("actor", "", "assigned operator or reviewer account")
	readPassword := flags.Bool("password-stdin", false, "read the initial account password from stdin instead of generating it")
	listen := flags.String("listen", "127.0.0.1:8080", "private listen address")
	cert := flags.String("tls-cert", "", "TLS certificate for private HTTPS")
	key := flags.String("tls-key", "", "TLS private key for private HTTPS")
	account := flags.String("account", "", "account to manage")
	roles := flags.String("roles", "", "comma-separated viewer,contributor,reviewer,operator,release-owner roles; empty removes account")
	backupDir := flags.String("backups", "", "protected backup directory")
	backupOperator := flags.String("backup-operator", "", "operator account for server-managed daily backups")
	from := flags.String("from", "", "source backup file or completed drill database")
	candidates := flags.String("candidates", "", "comma-separated retained candidate IDs to purge")
	confirm := flags.String("confirm", "", "exact purge confirmation SHA-256 from preview")
	if command == "config" {
		flags.String("setting", "", "operating setting to change")
		flags.String("duration", "", "new positive duration, such as 48h")
		flags.String("count", "", "positive whole request count for import-rate")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() > 0 {
		return errors.New("positional arguments are unsupported; use command flags")
	}
	settings := board.Settings{ProjectID: *project, ProductID: *product}
	if command == "restore" {
		if *from == "" {
			return errors.New("restore requires -from and a new destination -db path")
		}
		result, err := board.RestoreBackup(*from, *dbFile, settings)
		if err != nil {
			return err
		}
		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			return err
		}
		if !result.IntegrityOK {
			return board.ErrIntegrity
		}
		return nil
	}
	if command != "init" {
		if _, err := os.Stat(*dbFile); err != nil {
			return fmt.Errorf("open initialized evidence database: %w", err)
		}
	}
	app, err := board.Open(*dbFile, settings)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, app.Close()) }()
	switch command {
	case "init":
		return bootstrap(app, *actor, *readPassword, stdin, stdout)
	case "serve":
		return start(app, *listen, *cert, *key, *backupDir, *backupOperator, stdout)
	}
	if *actor == "" {
		return errors.New("provide -actor and its password on stdin")
	}
	secret, err := passwordFrom(stdin)
	if err != nil {
		return err
	}
	if _, err := app.Authenticate(*actor, secret); err != nil {
		return err
	}
	switch command {
	case "account":
		return changeAccount(app, *actor, *account, *roles, stdout)
	case "config":
		return configure(app, *actor, *project, flags, stdout)
	case "backup":
		if *backupDir == "" {
			return errors.New("provide a protected -backups directory")
		}
		result, err := app.Backup(*actor, *project, *backupDir)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(result)
	case "revalidate":
		return app.Revalidate(*actor, *project)
	case "suspend":
		return app.SuspendAssessments(*actor, *project)
	case "restore-drill":
		return app.RecordRestoreDrill(*actor, *project, *from)
	case "expire-backups":
		if *backupDir == "" {
			return errors.New("provide a protected -backups directory")
		}
		count, err := app.ExpireBackups(*actor, *project, *backupDir)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "Removed %d expired backup sets.\n", count)
		return err
	case "purge":
		var ids []string
		if *candidates != "" {
			ids = strings.Split(*candidates, ",")
		}
		return purge(app, *actor, *project, ids, *confirm, stdout)
	default:
		return errors.New("unknown operation")
	}
}

func configure(app *board.Board, actor, project string, flags *flag.FlagSet, stdout io.Writer) error {
	update, countSupplied := false, false
	flags.Visit(func(option *flag.Flag) {
		switch option.Name {
		case "setting", "duration":
			update = true
		case "count":
			update, countSupplied = true, true
		}
	})
	if update {
		setting := flags.Lookup("setting").Value.String()
		duration, err := time.ParseDuration(flags.Lookup("duration").Value.String())
		if setting == "" || err != nil {
			return fmt.Errorf("%w: provide -setting and a positive -duration such as 48h", board.ErrInvalid)
		}
		if setting == "import-rate" {
			count, err := strconv.Atoi(flags.Lookup("count").Value.String())
			if err != nil {
				return fmt.Errorf("%w: import-rate requires a positive integer -count", board.ErrInvalid)
			}
			return app.SetImportRate(actor, project, board.ImportRatePolicy{Count: count, Window: duration})
		}
		if countSupplied {
			return fmt.Errorf("%w: -count applies only to import-rate", board.ErrInvalid)
		}
		return app.SetOperatingDuration(actor, project, setting, duration)
	}
	config, policy, err := app.OperatingConfigSnapshot(actor, project)
	if err != nil {
		return err
	}
	display := make(map[string]string, len(config)+2)
	for setting, duration := range config {
		display[setting] = duration.String()
	}
	display["import-rate-count"], display["import-rate-window"] = strconv.Itoa(policy.Count), policy.Window.String()
	return json.NewEncoder(stdout).Encode(display)
}

func bootstrap(app *board.Board, actor string, readPassword bool, stdin io.Reader, stdout io.Writer) error {
	if actor == "" {
		return errors.New("init requires a named -actor account")
	}
	var password string
	var err error
	if readPassword {
		password, err = passwordFrom(stdin)
	} else {
		password = generatedPassword()
	}
	if err != nil {
		return err
	}
	if err := app.Bootstrap(actor, password); err != nil {
		return err
	}
	if !readPassword {
		_, err = fmt.Fprintf(stdout, "Account %s temporary display of generated password: %s\n", actor, password)
	}
	return err
}

func passwordFrom(source io.Reader) (string, error) {
	reader := bufio.NewReader(io.LimitReader(source, 256))
	text, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimSuffix(text, "\n"), "\r"), nil
}

func generatedPassword() string {
	var secret [24]byte
	// crypto/rand.Read fills the buffer or terminates the process.
	rand.Read(secret[:])
	return base64.RawURLEncoding.EncodeToString(secret[:])
}

func changeAccount(app *board.Board, actor, id, roleSet string, stdout io.Writer) error {
	if roleSet == "" {
		return app.RemoveAccount(actor, id)
	}
	var roles []board.Role
	for _, role := range strings.Split(roleSet, ",") {
		roles = append(roles, board.Role(role))
	}
	password := generatedPassword()
	if err := app.SetAccount(actor, id, password, roles); err != nil {
		return err
	}
	_, err := fmt.Fprintf(stdout, "Account %s temporary display of generated password: %s\n", id, password)
	return err
}

func purge(app *board.Board, actor, project string, ids []string, confirm string, stdout io.Writer) error {
	if confirm != "" {
		removed, err := app.Purge(actor, project, ids, confirm)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(removed)
	}
	plan, err := app.PreviewPurge(actor, project, ids)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(plan)
}

func start(app *board.Board, address, cert, key, backupDir, backupOperator string, stdout io.Writer) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	secure := cert != "" && key != ""
	if (cert != "") != (key != "") {
		return errors.New("provide both TLS certificate and key")
	}
	if !secure && !net.ParseIP(host).IsLoopback() {
		return errors.New("use private HTTPS outside loopback")
	}
	if (backupDir != "") != (backupOperator != "") {
		return errors.New("scheduled backups require both -backups and -backup-operator")
	}
	if backupOperator != "" {
		assigned, err := app.Account(backupOperator)
		if err != nil {
			return err
		}
		if !assigned.CanOperate() {
			return board.ErrForbidden
		}
	}
	scheme := "http"
	if secure {
		scheme = "https"
	}
	if _, err := fmt.Fprintf(stdout, "Release Evidence Board at %s://%s\n", scheme, address); err != nil {
		return err
	}
	handler := browser.New(app, secure)
	defer handler.Close()
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: 60 * time.Second,
		ErrorLog: log.New(io.Discard, "", 0)}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	finished := make(chan error, 1)
	maintained := make(chan struct{})
	go func() {
		defer close(maintained)
		maintenance(ctx, app, backupDir, backupOperator)
	}()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := server.Shutdown(shutdown)
		if err != nil {
			err = errors.Join(err, server.Close())
		}
		finished <- err
	}()
	if secure {
		err = server.ListenAndServeTLS(cert, key)
	} else {
		err = server.ListenAndServe()
	}
	stop()
	<-maintained
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return errors.Join(err, <-finished)
}

func maintenance(ctx context.Context, app *board.Board, backupDir, operator string) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	overdueAnnounced := false
	for {
		check := app.Check()
		if check.ConsecutiveFailures == 3 {
			log.Print("operator alert: three consecutive application health failures")
		}
		if check.BackupOverdue && !overdueAnnounced {
			log.Print("operator alert: " + check.BackupOverdueNotice)
		}
		if check.DatabaseOK {
			overdueAnnounced = check.BackupOverdue
		}
		if operator != "" {
			health, err := app.Health(operator, app.ProjectID())
			if err == nil && health.BackupDue {
				_, err = app.Backup(operator, app.ProjectID(), backupDir)
			}
			if err != nil {
				log.Print("operator alert: scheduled backup check failed")
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
