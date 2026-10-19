// Fundarr: one place that connects your exchanges, brokers and wallets, and
// keeps your portfolio tools fed with their history.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/engine"
	"github.com/Alexis-Marcel/fundarr/internal/secret"
	"github.com/Alexis-Marcel/fundarr/internal/server"
	"github.com/Alexis-Marcel/fundarr/internal/store"
	"github.com/Alexis-Marcel/fundarr/web"
	"golang.org/x/term"

	_ "github.com/Alexis-Marcel/fundarr/internal/dest/wealthfolio"
	_ "github.com/Alexis-Marcel/fundarr/internal/source/binance"
	_ "github.com/Alexis-Marcel/fundarr/internal/source/bitcoin"
	_ "github.com/Alexis-Marcel/fundarr/internal/source/evm"
	_ "github.com/Alexis-Marcel/fundarr/internal/source/okx"
)

var version = "dev"

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func list(k string) []string {
	var out []string
	for _, v := range strings.Split(os.Getenv(k), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "health":
			os.Exit(health())
		case "hash-password":
			hashPassword()
			return
		case "version":
			fmt.Println(version)
			return
		}
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	dataDir := env("FUNDARR_DATA", "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	master, created, err := secret.LoadMaster(os.Getenv("FUNDARR_SECRET_KEY"), dataDir)
	if err != nil {
		return err
	}
	if created {
		log.Warn("created a new master key: back up " + filepath.Join(dataDir, "secret.key") + " with the database, credentials cannot be read without it")
	}
	box, err := secret.New(master)
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(dataDir, "fundarr.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	if n, err := st.CloseInterrupted(context.Background()); err == nil && n > 0 {
		log.Info("marked runs interrupted by the last stop", "count", n)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	eng := engine.New(st, box, log)
	// Like the *arr apps: a username and password created on first launch,
	// unless single sign-on or a proxy is configured.
	mode := env("FUNDARR_AUTH", "")
	if mode == "" {
		mode = "password"
		if os.Getenv("FUNDARR_OIDC_ISSUER") != "" {
			mode = "oidc"
		}
	}
	static, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		return err
	}
	srv, err := server.New(ctx, server.Config{
		Engine:     eng,
		Static:     static,
		Version:    version,
		Log:        log,
		Background: ctx,
		Auth: server.AuthConfig{
			Mode:             mode,
			Username:         env("FUNDARR_USERNAME", "admin"),
			PasswordHash:     os.Getenv("FUNDARR_PASSWORD_HASH"),
			Credentials:      st,
			ProxyHeader:      env("FUNDARR_PROXY_HEADER", "Remote-User"),
			OIDCIssuer:       os.Getenv("FUNDARR_OIDC_ISSUER"),
			OIDCClientID:     os.Getenv("FUNDARR_OIDC_CLIENT_ID"),
			OIDCClientSecret: os.Getenv("FUNDARR_OIDC_CLIENT_SECRET"),
			OIDCAllowedSubs:  list("FUNDARR_OIDC_ALLOWED_SUBS"),
			OIDCAllowedMails: list("FUNDARR_OIDC_ALLOWED_EMAILS"),
			PublicURL:        os.Getenv("FUNDARR_URL"),
			OIDCAutoRedirect: !strings.EqualFold(os.Getenv("FUNDARR_OIDC_AUTO_REDIRECT"), "false"),
			SessionKey:       box.Key("fundarr/session"),
		},
	})
	if err != nil {
		return err
	}
	go eng.Run(ctx)

	addr := env("FUNDARR_ADDR", ":9494")
	hs := &http.Server{Addr: addr, Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hs.Shutdown(sh)
	}()
	log.Info("fundarr listening", "addr", addr, "auth", mode, "version", version)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// health is the container healthcheck (the image has no shell or curl).
func health() int {
	addr := env("FUNDARR_ADDR", ":9494")
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		return 1
	}
	return 0
}

func hashPassword() {
	fmt.Fprint(os.Stderr, "Password: ")
	pw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil || len(pw) < 8 {
		fmt.Fprintln(os.Stderr, "a password of at least 8 characters is required")
		os.Exit(1)
	}
	fmt.Println(server.HashPassword(string(pw)))
}
