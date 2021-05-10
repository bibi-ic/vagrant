package runner

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/hashicorp/go-hclog"
	plg "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hashicorp/vagrant-plugin-sdk/component"
	"github.com/hashicorp/vagrant-plugin-sdk/terminal"
	intcfg "github.com/hashicorp/vagrant/internal/config"
	"github.com/hashicorp/vagrant/internal/factory"
	"github.com/hashicorp/vagrant/internal/plugin"

	"github.com/hashicorp/vagrant/internal/server"
	"github.com/hashicorp/vagrant/internal/server/proto/vagrant_server"
	"github.com/hashicorp/vagrant/internal/serverclient"
)

var ErrClosed = errors.New("runner is closed")

// Runners in Vagrant execute operations. These can be local (the CLI)
// or they can be remote (triggered by some webhook). In either case, they
// share this same underlying implementation.
//
// To use a runner:
//
//   1. Initialize it with New. This will setup some initial state but
//      will not register with the server or run jobs.
//
//   2. Start the runner with "Start". This will register the runner and
//      kick off some management goroutines. This will not execute any jobs.
//
//   3. Run a single job with "Accept". This is named to be similar to a
//      network listener "accepting" a connection. This will request a single
//      job from the Vagrant server, block until one is available, and execute
//      it. Repeat this call for however many jobs you want to execute.
//
//   4. Clean up with "Close". This will gracefully exit the runner, waiting
//      for any running jobs to finish.
//
type Runner struct {
	id                 string
	logger             hclog.Logger
	client             *serverclient.VagrantClient
	vagrantRubyRuntime plg.ClientProtocol
	vagrantRubyClient  *serverclient.RubyVagrantClient
	builtinPlugins     *plugin.Builtin
	ctx                context.Context
	cleanupFunc        func()
	runner             *vagrant_server.Runner
	factories          map[component.Type]*factory.Factory
	ui                 terminal.UI
	local              bool
	tempDir            string

	closedVal int32
	acceptWg  sync.WaitGroup

	// config is the current runner config.
	config      *vagrant_server.RunnerConfig
	originalEnv []*vagrant_server.ConfigVar

	// this is used for registering plugins to prevent performing the
	// sequence for every operation
	opConfig *intcfg.Config

	// noopCh is used in tests only. This will cause any noop operations
	// to block until this channel is closed.
	noopCh <-chan struct{}
}

// New initializes a new runner.
//
// You must call Start to start the runner and register with the Vagrant
// server. See the Runner struct docs for more details.
func New(opts ...Option) (*Runner, error) {
	// Create our ID
	id, err := server.Id()
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"failed to generate unique ID: %s", err)
	}

	// Our default runner
	runner := &Runner{
		id:        id,
		logger:    hclog.L(),
		ctx:       context.Background(),
		runner:    &vagrant_server.Runner{Id: id},
		opConfig:  &intcfg.Config{},
		factories: plugin.BaseFactories,
	}

	// Build our config
	var cfg config
	for _, o := range opts {
		err := o(runner, &cfg)
		if err != nil {
			return nil, err
		}
	}

	runner.logger = runner.logger.ResetNamed("vagrant.runner")
	// Setup our runner components list
	for t, f := range runner.factories {
		for _, n := range f.Registered() {
			runner.runner.Components = append(runner.runner.Components, &vagrant_server.Component{
				Type: vagrant_server.Component_Type(t),
				Name: n,
			})
		}
	}

	return runner, nil
}

// Id returns the runner ID.
func (r *Runner) Id() string {
	return r.id
}

// Start starts the runner by registering the runner with the Vagrant
// server. This will spawn goroutines for management. This will return after
// registration so this should not be executed in a goroutine.
func (r *Runner) Start() error {
	if r.closed() {
		return ErrClosed
	}

	log := r.logger

	// Register
	log.Debug("registering runner")
	client, err := r.client.RunnerConfig(r.ctx)
	if err != nil {
		return err
	}
	r.cleanup(func() { client.CloseSend() })

	// Send request
	if err := client.Send(&vagrant_server.RunnerConfigRequest{
		Event: &vagrant_server.RunnerConfigRequest_Open_{
			Open: &vagrant_server.RunnerConfigRequest_Open{
				Runner: r.runner,
			},
		},
	}); err != nil {
		return err
	}

	// Wait for an initial config as confirmation we're registered.
	log.Trace("runner connected, waiting for initial config")
	resp, err := client.Recv()
	if err != nil {
		return err
	}

	// Handle the first config so our initial setup is done
	r.handleConfig(resp.Config)

	// Start the watcher
	ch := make(chan *vagrant_server.RunnerConfig)
	go r.watchConfig(ch)

	// Start the goroutine that waits for all other configs
	go r.recvConfig(r.ctx, client, ch)

	log.Info("runner registered with server")

	if plugin.IN_PROCESS_PLUGINS {
		r.builtinPlugins = plugin.NewBuiltins(context.Background(), log)
	}

	// track plugins
	err = r.LoadPlugins(r.opConfig)
	if err != nil {
		r.logger.Error("failed to load ruby runtime plugins", "error", err)
		return err
	}

	r.factories, err = r.pluginFactories(r.logger, r.opConfig.Plugins(), ".")
	if err != nil {
		r.logger.Error("failed to load plugin factories", "error", err)
		return err
	}

	if r.builtinPlugins != nil {
		r.builtinPlugins.Start()
	}

	return nil
}

// Close gracefully exits the runner. This will wait for any pending
// job executions to complete and then deregister the runner. After
// this is called, Start and Accept will no longer function and will
// return errors immediately.
func (r *Runner) Close() error {
	// If we can't swap, we're already closed.
	if !atomic.CompareAndSwapInt32(&r.closedVal, 0, 1) {
		return nil
	}

	// Wait for our jobs to complete
	r.acceptWg.Wait()

	// Run any cleanup necessary
	if f := r.cleanupFunc; f != nil {
		f()
	}

	if r.builtinPlugins != nil {
		r.builtinPlugins.Close()
	}
	return nil
}

func (r *Runner) closed() bool {
	return atomic.LoadInt32(&r.closedVal) > 0
}

type config struct{}

type Option func(*Runner, *config) error

// WithClient sets the client directly. In this case, the runner won't
// attempt any connection at all regardless of other configuration (env
// vars or vagrant config file). This will be used.
func WithClient(client *serverclient.VagrantClient) Option {
	return func(r *Runner, cfg *config) error {
		r.client = client
		return nil
	}
}

func WithVagrantRubyRuntime(vrr plg.ClientProtocol) Option {
	return func(r *Runner, cfg *config) error {
		r.vagrantRubyRuntime = vrr
		raw, err := vrr.Dispense("vagrantrubyruntime")
		if err != nil {
			return err
		}
		rvc, ok := raw.(serverclient.RubyVagrantClient)
		if !ok {
			panic("failed to dispense RubyVagrantClient")
		}
		r.vagrantRubyClient = &rvc
		return nil
	}
}

// WithComponentFactory sets a factory for a component type. If this isn't set for
// a component type, then the builtins will be used.
func WithComponentFactory(t component.Type, f *factory.Factory) Option {
	return func(r *Runner, cfg *config) error {
		r.factories[t] = f
		return nil
	}
}

// WithLogger sets the logger that the runner will use. If this isn't
// set it uses hclog.L().
func WithLogger(logger hclog.Logger) Option {
	return func(r *Runner, cfg *config) error {
		r.logger = logger
		return nil
	}
}

// WithLocal sets the runner to local mode. This only changes the UI
// behavior to use the given UI. If ui is nil then the normal streamed
// UI will be used.
func WithLocal(ui terminal.UI) Option {
	return func(r *Runner, cfg *config) error {
		r.local = true
		r.ui = ui
		return nil
	}
}

// ByIdOnly sets it so that only jobs that target this runner by specific
// ID may be assigned.
func ByIdOnly() Option {
	return func(r *Runner, cfg *config) error {
		r.runner.ByIdOnly = true
		return nil
	}
}