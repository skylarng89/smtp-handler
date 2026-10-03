// Command smtp-handler is a service that accepts email-send requests from
// frontend applications so SMTP credentials never live in the browser.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/skylarng89/smtp-handler/internal/app"
	"github.com/skylarng89/smtp-handler/internal/auth"
	"github.com/skylarng89/smtp-handler/internal/compose"
	"github.com/skylarng89/smtp-handler/internal/config"
	"github.com/skylarng89/smtp-handler/internal/logging"
	"github.com/skylarng89/smtp-handler/internal/render"
	"github.com/skylarng89/smtp-handler/internal/store/sqlstore"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `smtp-handler - send email on behalf of frontend applications

Usage:
  smtp-handler [serve] [-config FILE]      run the service (default command)
  smtp-handler migrate [-config FILE]      apply database migrations and exit
  smtp-handler config validate [-config FILE]
                                           load and validate the configuration
  smtp-handler keys generate [-type publishable|secret] [-count N]
                                           create API keys and their config hashes
  smtp-handler hash-password               create an admin password hash (reads stdin)
  smtp-handler healthcheck [-addr HOST:PORT]
                                           probe /readyz (for container health checks)
  smtp-handler version

Configuration comes from the YAML file (-config or SMTPH_CONFIG) and SMTPH_*
environment variables. See README.md.
`

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	switch cmd {
	case "serve":
		return serve(args, stderr)
	case "migrate":
		return migrate(args, stdout, stderr)
	case "config":
		if len(args) == 0 || args[0] != "validate" {
			return errors.New("usage: smtp-handler config validate [-config FILE]")
		}
		return validateConfig(args[1:], stdout, stderr)
	case "keys":
		if len(args) == 0 || args[0] != "generate" {
			return errors.New("usage: smtp-handler keys generate [-type publishable|secret] [-count N]")
		}
		return generateKeys(args[1:], stdout, stderr)
	case "hash-password":
		return hashPassword(stdin, stdout, stderr)
	case "healthcheck":
		return healthcheck(args, stderr)
	case "version", "-v", "--version":
		fmt.Fprintln(stdout, "smtp-handler", version)
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	default:
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func loadConfig(name string, args []string, stderr io.Writer) (*config.Config, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "path to the YAML configuration file (default: $SMTPH_CONFIG)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return config.Load(*path, nil)
}

func serve(args []string, stderr io.Writer) error {
	cfg, err := loadConfig("serve", args, stderr)
	if err != nil {
		return err
	}
	log := logging.New(stderr, cfg.Logging)
	log.Info("starting", "version", version)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop() // a second signal terminates immediately
	}()

	a, err := app.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	return a.Run(ctx)
}

func migrate(args []string, stdout, stderr io.Writer) error {
	cfg, err := loadConfig("migrate", args, stderr)
	if err != nil {
		return err
	}
	// Open without auto-migration so the outcome below is explicit.
	cfg.Store.AutoMigrate = new(bool)
	ctx := context.Background()
	s, err := sqlstore.Open(ctx, cfg.Store)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "database schema is up to date (%s)\n", s.Driver())
	return nil
}

func validateConfig(args []string, stdout, stderr io.Writer) error {
	cfg, err := loadConfig("config validate", args, stderr)
	if err != nil {
		return err
	}
	reg, err := render.NewRegistry(cfg)
	if err != nil {
		return fmt.Errorf("templates: %w", err)
	}
	if _, err := compose.New(cfg, reg); err != nil {
		return err
	}
	for _, w := range cfg.Warnings {
		fmt.Fprintln(stdout, "warning:", w)
	}
	templates := 0
	for _, p := range cfg.Projects {
		templates += len(p.Templates)
	}
	fmt.Fprintf(stdout, "configuration is valid: %d project(s), %d template(s), store=%s\n",
		len(cfg.Projects), templates, cfg.Store.Driver)
	return nil
}

func generateKeys(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("keys generate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	typ := fs.String("type", "publishable", "key type: publishable or secret")
	count := fs.Int("count", 1, "number of keys to generate")
	if err := fs.Parse(args); err != nil {
		return err
	}
	kt := auth.KeyType(*typ)
	if kt != auth.Publishable && kt != auth.Secret {
		return errors.New("-type must be publishable or secret")
	}
	if *count < 1 || *count > 100 {
		return errors.New("-count must be between 1 and 100")
	}

	for range *count {
		key, hash, err := auth.GenerateKey(kt)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "key:  %s\nhash: %s\n", key, hash)
		fmt.Fprintf(stdout, "\n# add to the project's keys:\n- type: %s\n  hash: %s\n", kt, hash)
		if kt == auth.Publishable {
			fmt.Fprintln(stdout, "  allowed_origins: [\"https://your-app.example\"]")
		}
		fmt.Fprintln(stdout)
	}
	fmt.Fprintln(stderr, "Store the key itself securely now: only its hash is kept in the configuration and it cannot be recovered.")
	return nil
}

func hashPassword(stdin io.Reader, stdout, stderr io.Writer) error {
	var password string
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(stderr, "Password: ")
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(stderr)
		if err != nil {
			return err
		}
		password = string(b)
	} else {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		password = strings.TrimRight(line, "\r\n")
	}
	if len(password) < 12 {
		return errors.New("password must be at least 12 characters")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, hash)
	return nil
}

// healthcheck probes the readiness endpoint so distroless images, which have
// no shell or curl, can still define a container HEALTHCHECK.
func healthcheck(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	def := os.Getenv("SMTPH_ADMIN_ADDR")
	if def == "" {
		def = "127.0.0.1:9090"
	}
	addr := fs.String("addr", def, "address of the operations listener")
	if err := fs.Parse(args); err != nil {
		return err
	}
	host := *addr
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// The address is the operator's own flag/env for the local listener.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+host+"/readyz", nil) //nolint:gosec // G704: operator-supplied local address
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req) //nolint:gosec // G704: see above
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("not ready: HTTP %d", res.StatusCode)
	}
	return nil
}
