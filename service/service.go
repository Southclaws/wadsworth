package service

import (
	"context"
	"time"

	"github.com/eapache/go-resiliency/retrier"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"gopkg.in/src-d/go-git.v4/plumbing/transport"
	"gopkg.in/src-d/go-git.v4/plumbing/transport/ssh"

	"github.com/picostack/pico/service/executor"
	"github.com/picostack/pico/service/secret"
	"github.com/picostack/pico/service/secret/memory"
	"github.com/picostack/pico/service/secret/vault"
	"github.com/picostack/pico/service/task"
	"github.com/picostack/pico/service/watcher"
)

// Config specifies static configuration parameters (from CLI or environment)
type Config struct {
	Target        string
	Hostname      string
	NoSSH         bool
	Directory     string
	CheckInterval time.Duration
	VaultAddress  string
	VaultToken    string
	VaultPath     string
	VaultRenewal  time.Duration
}

// App stores application state
type App struct {
	config  Config
	watcher *watcher.Watcher
	secrets secret.Store
	bus     chan task.ExecutionTask
}

// Initialise prepares an instance of the app to run
func Initialise(c Config) (app *App, err error) {
	app = new(App)

	app.config = c

	var authMethod transport.AuthMethod
	if !c.NoSSH {
		authMethod, err = ssh.NewSSHAgentAuth("git")
		if err != nil {
			return nil, errors.Wrap(err, "failed to set up SSH authentication")
		}
	}

	var secretStore secret.Store
	if c.VaultAddress != "" {
		zap.L().Debug("connecting to vault",
			zap.String("address", c.VaultAddress),
			zap.String("path", c.VaultPath),
			zap.String("token", c.VaultToken),
			zap.Duration("renewal", c.VaultRenewal))

		secretStore, err = vault.New(c.VaultAddress, c.VaultPath, c.VaultToken, c.VaultRenewal)
		if err != nil {
			return nil, err
		}
	} else {
		secretStore = &memory.MemorySecrets{
			// TODO: pull env vars with PICO_SECRET_* or something and shove em here
		}
	}

	app.secrets = secretStore

	app.bus = make(chan task.ExecutionTask, 100)

	app.watcher = watcher.New(
		app.bus,
		c.Hostname,
		c.Directory,
		c.Target,
		c.CheckInterval,
		authMethod,
	)

	return
}

// Start launches the app and blocks until fatal error
func (app *App) Start(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)

	zap.L().Debug("starting service daemon")

	ce := executor.NewCommandExecutor(app.secrets)
	g.Go(func() error {
		return ce.Subscribe(app.bus)
	})

	g.Go(app.watcher.Start)

	if s, ok := app.secrets.(*vault.VaultSecrets); ok {
		g.Go(func() error {
			return retrier.New(retrier.ConstantBackoff(3, 100*time.Millisecond), nil).
				RunCtx(ctx, s.Renew)
		})
	}

	return g.Wait()
}
