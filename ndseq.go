package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/briansorahan/death"
	"github.com/pkg/errors"
	"github.com/xthexder/go-jack"
)

var (
	bufferSize uint32
	client     *jack.Client
	in         *jack.Port
	out        *jack.Port
)

func main() {
	config, err := NewConfig()
	death.Main(errors.Wrap(err, "parsing config"))

	app, err := NewApp(config)
	death.Main(errors.Wrap(err, "creating app"))
	death.Main(errors.Wrap(app.Run(context.Background()), "running app"))
}

// App defines the behavior of the application.
type App struct {
	Config
}

// NewApp creates a new app.
func NewApp(config Config) (*App, error) {
	a := &App{Config: config}

	jc, code := jack.ClientOpen(a.ClientName, jack.NoStartServer)
	if isFailure(code) {
		return nil, jack.Strerror(code)
	}
	client = jc

	if code := client.SetProcessCallback(Process); isFailure(code) {
		return nil, jack.Strerror(code)
	}
	in = client.PortRegister("in", jack.DEFAULT_MIDI_TYPE, jack.PortIsInput, 0)
	out = client.PortRegister("out", jack.DEFAULT_MIDI_TYPE, jack.PortIsOutput, 0)

	if in == nil {
		return nil, errors.New("registering input port")
	}
	if code := client.Activate(); isFailure(code) {
		return nil, errors.New("activating client")
	}
	bufferSize = client.GetBufferSize()

	return a, nil
}

// Run runs the application.
func (a *App) Run(ctx context.Context) error {
	sc := make(chan os.Signal, 1)

	signal.Notify(sc, os.Interrupt, syscall.SIGQUIT, syscall.SIGINT)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case sig := <-sc:
		fmt.Printf("received %s, exiting\n", sig)
		return nil
	}
	return nil
}

// Config defines the application's configuration.
type Config struct {
	ClientName string `json:"clientName"`
}

// NewConfig creates a new config.
func NewConfig() (Config, error) {
	var c Config

	flag.StringVar(&c.ClientName, "c", "ndseq", "JACK client name.")
	flag.Parse()

	return c, nil
}

// Process is the JACK process callback.
func Process(nframes uint32) int {
	events := in.GetMidiEvents(bufferSize)

	for _, event := range events {
		fmt.Printf("midi event %X\n", event.Buffer)
	}
	return 0
}

func isFailure(code int) bool {
	return code == jack.Failure || code == jack.InvalidOption || code == jack.NameNotUnique ||
		code == jack.ServerError || code == jack.NoSuchClient || code == jack.LoadFailure ||
		code == jack.InitFailure || code == jack.ShmFailure || code == jack.VersionError ||
		code == jack.BackendError || code == jack.ClientZombie
}
