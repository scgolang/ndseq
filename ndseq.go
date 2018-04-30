package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/briansorahan/death"
	"github.com/pkg/errors"
	flag "github.com/spf13/pflag"
	"github.com/xthexder/go-jack"
)

const (
	clientName = "ndseq" // JACK client name.
)

// Error codes.
const (
	DivideByZero = 50
)

var (
	bufferSize uint32

	client *jack.Client

	launchpadInput  *jack.Port // JACK port for sending MIDI data to the Launchpad.
	launchpadOutput *jack.Port // JACK port for receiving MIDI data from the Launchpad.

	nd       string     // Name of the MIDI interface to use for communicating with the Nord Drum 3p.
	ndInput  *jack.Port // JACK port for sending MIDI data to the Nord Drum 3p.
	ndOutput *jack.Port // JACK port for receiving MIDI data from the Nord Drum 3p.

	beat            int    // 64 steps
	firstNotePlayed bool   // Flag telling us if we've ever played a note.
	sampleCount     uint32 // Current sample count. This gets reset everytime we trigger a sequencer step.
	samplesPerBeat  uint32 // Samples per beat. Gets updated if the sample rate or the tempo changes.
	tempo           uint32 // Tempo in BPM.

	trigs [8][64]uint8 // Launchpad grid data.
)

func main() {
	// Parse the command line flags.
	// I use a Focusrite Scarlett 6i6 to communicate with the Nord Drum.
	flag.StringVar(&nd, "nd", "Scarlett", "JACK port for the Nord Drum 3p.")
	flag.Uint32Var(&tempo, "t", 120, "Tempo in BPM.")
	flag.Parse()

	var code int

	// Open the JACK client.
	client, code = jack.ClientOpen(clientName, jack.NoStartServer)
	death.Main(wrapCode(code, "opening JACK client"))

	// Set the callbacks.
	death.Main(wrapCode(client.SetSampleRateCallback(setSamplesPerBeat), "setting sample rate callback"))
	death.Main(wrapCode(client.SetProcessCallback(Process), "setting process callback"))

	// Register the JACK ports.
	death.Main(errors.Wrap(registerPorts(), "registering ports"))

	// Activate the client.
	death.Main(wrapCode(client.Activate(), "activating JACK client"))

	// Set the buffer size.
	bufferSize = client.GetBufferSize()

	// Wait for a signal or context done.
	var (
		ctx = context.Background()
		sc  = make(chan os.Signal, 1)
	)
	signal.Notify(sc, os.Interrupt, syscall.SIGQUIT, syscall.SIGINT)

	select {
	case <-ctx.Done():
		os.Exit(0)
	case sig := <-sc:
		fmt.Printf("received %s, exiting\n", sig)
		os.Exit(0)
	}
}

// Process is the JACK process callback.
func Process(nframes uint32) int {
	var (
		launchpadEvents = launchpadInput.GetMidiEvents(bufferSize)
		outBuffer       = ndOutput.MidiClearBuffer(nframes)
	)
	if len(launchpadEvents) > 0 {
		for _, event := range launchpadEvents {
			if code := processMidi(nframes, event, outBuffer); code != 0 {
				return code
			}
		}
	}
	return tick(nframes, outBuffer)
}

func advanceStepLight(outBuffer jack.MidiBuffer) int {
	if beat == 0 && !firstNotePlayed {
		// First note ever: light step 0.
		beat++
		return launchpadOutput.MidiEventWrite(&jack.MidiData{Buffer: []byte{0x90, 0x10, 63}}, outBuffer)
	}
	beat++
	return 0
}

func cc(nframes uint32, in []byte, outBuffer jack.MidiBuffer) int {
	var (
		event = jack.MidiData{
			Buffer: []byte{0x90, 0x36, in[2]}, // Note On C3
		}
	)
	// Set the output channel.
	switch in[1] {
	case 0x68: // Button 1
	case 0x69: // Button 2
		event.Buffer[0] |= 0x01
	case 0x6A: // Button 3
		event.Buffer[0] |= 0x02
	case 0x6B: // Button 4
		event.Buffer[0] |= 0x03
	case 0x6C: // Button 5
		event.Buffer[0] |= 0x04
	case 0x6D: // Button 6
		event.Buffer[0] |= 0x05
	}
	return ndOutput.MidiEventWrite(&event, outBuffer)
}

func contains(sub string) func(string) bool {
	return func(s string) bool {
		return strings.Contains(s, sub)
	}
}

func isFailure(code int) bool {
	return code == jack.Failure || code == jack.InvalidOption || code == jack.NameNotUnique ||
		code == jack.ServerError || code == jack.NoSuchClient || code == jack.LoadFailure ||
		code == jack.InitFailure || code == jack.ShmFailure || code == jack.VersionError ||
		code == jack.BackendError || code == jack.ClientZombie || code == DivideByZero
}

func light(x, y, g, r int, outBuffer jack.MidiBuffer) int {
	var (
		note     = byte(x + (16 * y))
		velocity = byte((16 * g) + r + 8 + 4)
	)
	return ndOutput.MidiEventWrite(&jack.MidiData{Buffer: []byte{0x90, note, velocity}}, outBuffer)

}

func note(nframes uint32, in []byte, out jack.MidiBuffer) int {
	return 0
}

type Port struct {
	*jack.Port

	Matches    func(string) bool
	Flags      uint64
	BufferSize uint64
}

var Ports = struct {
	Inputs  map[string]*Port
	Outputs map[string]*Port
}{
	Inputs: map[string]*Port{
		"LaunchpadRecv": {
			Matches: contains("Launchpad"),
		},
		"NordDrumRecv": {
			Matches: contains("Scarlett"),
		},
	},
	Outputs: map[string]*Port{
		"LaunchpadSend": {
			Matches: contains("Launchpad"),
		},
		"NordDrumSend": {
			Matches: contains("Scarlett"),
		},
	},
}

func registerPorts() error {
	for name, input := range Ports.Inputs {
		input.Port = client.PortRegister(name, jack.DEFAULT_MIDI_TYPE, input.Flags|jack.PortIsInput, input.BufferSize)
	}
	for name, output := range Ports.Outputs {
		output.Port = client.PortRegister(name, jack.DEFAULT_MIDI_TYPE, output.Flags|jack.PortIsOutput, output.BufferSize)
	}
	for _, in := range client.GetPorts("", jack.DEFAULT_MIDI_TYPE, jack.PortIsInput) {
		for name, out := range Ports.Outputs {
			if out.Matches(in) {
				rc := client.ConnectPorts(out.Port, client.GetPortByName(in))
				if err := wrapCodef(rc, "connecting %s to %s", name, in); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func processMidi(nframes uint32, event *jack.MidiData, outBuffer jack.MidiBuffer) int {
	switch event.Buffer[0] {
	case 0xB0: // CC
		return cc(nframes, event.Buffer, outBuffer)
	case 0x80, 0x90: // Note
		return note(nframes, event.Buffer, outBuffer)
	}
	return 0
}

func setSamplesPerBeat(sr uint32) int {
	if tempo == 0 {
		return DivideByZero
	}
	samplesPerBeat = (60 * sr) / tempo
	return 0
}

func stepLightMidiData(beat int) *jack.MidiData {
	var (
		note = byte((beat / 8) + (16 * (beat % 8)))
	)
	return &jack.MidiData{Buffer: []byte{0x90, note, 63}}
}

func tick(nframes uint32, outBuffer jack.MidiBuffer) int {
	if !firstNotePlayed {
		if code := advanceStepLight(outBuffer); isFailure(code) {
			return code
		}
		firstNotePlayed = true

		return trigger(nframes, outBuffer)
	}
	if sampleCount+nframes < samplesPerBeat {
		sampleCount += nframes
		return 0
	}
	return trigger(nframes, outBuffer)
}

func trigger(nframes uint32, outBuffer jack.MidiBuffer) int {
	for track, trackTrigs := range trigs {
		for _, trig := range trackTrigs {
			// TODO: trigger the notes.
			if code := triggerTrack(track, trig, outBuffer); isFailure(code) {
				return code
			}
		}
	}
	return 0
}

func triggerTrack(track int, trig uint8, outBuffer jack.MidiBuffer) int {
	return 0
}

func wrapCode(code int, msg string) error {
	return errors.Wrap(jack.Strerror(code), msg)
}

func wrapCodef(code int, format string, args ...interface{}) error {
	return errors.Wrapf(jack.Strerror(code), format, args...)
}
