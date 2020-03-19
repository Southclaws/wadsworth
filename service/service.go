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
	"github.com/picostack/pico/service/reconfigurer"
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
	config       Config
	reconfigurer reconfigurer.Provider
	watcher      watcher.Watcher
	secrets      secret.Store
	bus          chan task.ExecutionTask
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

	// reconfigurer
	app.reconfigurer = reconfigurer.New(
		c.Directory,
		c.Hostname,
		c.Target,
		c.CheckInterval,
		authMethod,
	)

	// target watcher
	app.watcher = watcher.NewGitWatcher(
		app.config.Directory,
		app.bus,
		app.config.CheckInterval,
		authMethod,
	)

	return
}

// Start launches the app and blocks until fatal error
func (app *App) Start(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)

	zap.L().Debug("starting service daemon")

	// TODO: Replace this errgroup with a more resilient solution.
	// Not all of these tasks fail in the same way. Some don't fail at all.
	// This needs to be rewritten to be more considerate of different failure
	// states and potentially retry in some circumstances. Pico should be the
	// kind of service that barely goes down, only when absolutely necessary.

	ce := executor.NewCommandExecutor(app.secrets)
	g.Go(func() error {
		ce.Subscribe(app.bus)
		return nil
	})

	// TODO: gw can fail when setting up the gitwatch instance, it should retry.
	gw := app.watcher.(*watcher.GitWatcher)
	g.Go(gw.Start)

	// TODO: reconfigurer can also fail when setting up gitwatch.
	g.Go(func() error {
		return app.reconfigurer.Configure(app.watcher)
	})

	if s, ok := app.secrets.(*vault.VaultSecrets); ok {
		g.Go(func() error {
			return retrier.New(retrier.ConstantBackoff(3, 100*time.Millisecond), nil).
				RunCtx(ctx, s.Renew)
		})
	}

	return g.Wait()
}
