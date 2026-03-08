//go:build !darwin || !arm64
// +build !darwin !arm64

package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/fang"
	"github.com/gopxl/beep/v2"
	"github.com/gopxl/beep/v2/mp3"
	"github.com/gopxl/beep/v2/speaker"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var version = "dev"

//go:embed audio/pain/*.mp3
var painAudio embed.FS

//go:embed audio/sexy/*.mp3
var sexyAudio embed.FS

//go:embed audio/halo/*.mp3
var haloAudio embed.FS

var (
	sexyMode     bool
	haloMode     bool
	customPath   string
	minAmplitude float64
	cooldownMs   int
	triggerMode  string
	testAudio    bool
)

type playMode int

const (
	modeRandom playMode = iota
	modeEscalation
)

const (
	decayHalfLife     = 30.0
	defaultCooldownMs = 750
)

type soundPack struct {
	name   string
	fs     embed.FS
	dir    string
	mode   playMode
	files  []string
	custom bool
}

func (sound *soundPack) loadFiles() error {
	if sound.custom {
		entries, err := os.ReadDir(sound.dir)
		if err != nil {
			return err
		}
		sound.files = make([]string, 0, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() {
				sound.files = append(sound.files, filepath.Join(sound.dir, entry.Name()))
			}
		}
	} else {
		entries, err := sound.fs.ReadDir(sound.dir)
		if err != nil {
			return err
		}
		sound.files = make([]string, 0, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() {
				sound.files = append(sound.files, sound.dir+"/"+entry.Name())
			}
		}
	}
	sort.Strings(sound.files)
	if len(sound.files) == 0 {
		return fmt.Errorf("no audio files found in %s", sound.dir)
	}
	return nil
}

type slapTracker struct {
	mu       sync.Mutex
	score    float64
	lastTime time.Time
	total    int
	halfLife float64
	scale    float64
	pack     *soundPack
}

func newSlapTracker(pack *soundPack, cooldown time.Duration) *slapTracker {
	cooldownSec := cooldown.Seconds()
	ssMax := 1.0 / (1.0 - math.Pow(0.5, cooldownSec/decayHalfLife))
	scale := (ssMax - 1) / math.Log(float64(len(pack.files)+1))
	return &slapTracker{
		halfLife: decayHalfLife,
		scale:    scale,
		pack:     pack,
	}
}

func (tracker *slapTracker) record(now time.Time) (int, float64) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if !tracker.lastTime.IsZero() {
		elapsed := now.Sub(tracker.lastTime).Seconds()
		tracker.score *= math.Pow(0.5, elapsed/tracker.halfLife)
	}
	tracker.score += 1.0
	tracker.lastTime = now
	tracker.total++
	return tracker.total, tracker.score
}

func (tracker *slapTracker) getFile(score float64) string {
	if tracker.pack.mode == modeRandom {
		return tracker.pack.files[rand.Intn(len(tracker.pack.files))]
	}

	maxIndex := len(tracker.pack.files) - 1
	index := int(float64(len(tracker.pack.files)) * (1.0 - math.Exp(-(score-1)/tracker.scale)))
	if index > maxIndex {
		index = maxIndex
	}
	return tracker.pack.files[index]
}

func main() {
	cmd := &cobra.Command{
		Use:   "spank",
		Short: "Plays responses when triggered",
		Long: `spank on non-Apple-Silicon platforms runs in manual trigger mode.

Press Enter to simulate a slap. Type q and press Enter to quit.

On Apple Silicon macOS builds, spank uses the accelerometer for automatic slap detection.`,
		Version: version,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd.Context())
		},
		SilenceUsage: true,
	}

	cmd.Flags().BoolVarP(&sexyMode, "sexy", "s", false, "Enable sexy mode")
	cmd.Flags().BoolVarP(&haloMode, "halo", "H", false, "Enable halo mode")
	cmd.Flags().StringVarP(&customPath, "custom", "c", "", "Path to custom MP3 audio directory")
	cmd.Flags().Float64Var(&minAmplitude, "min-amplitude", 0.3, "Minimum amplitude threshold (accepted for compatibility)")
	cmd.Flags().IntVar(&cooldownMs, "cooldown", defaultCooldownMs, "Cooldown between responses in milliseconds")
	cmd.Flags().StringVar(&triggerMode, "trigger", "both", "Trigger mode on non-Apple-Silicon platforms: enter|key|both")
	cmd.Flags().BoolVar(&testAudio, "test-audio", false, "Play one test sound at startup and report errors")

	if err := fang.Execute(context.Background(), cmd); err != nil {
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	modeCount := 0
	if sexyMode {
		modeCount++
	}
	if haloMode {
		modeCount++
	}
	if customPath != "" {
		modeCount++
	}
	if modeCount > 1 {
		return fmt.Errorf("--sexy, --halo, and --custom are mutually exclusive; pick one")
	}

	if minAmplitude < 0 || minAmplitude > 1 {
		return fmt.Errorf("--min-amplitude must be between 0.0 and 1.0")
	}

	if triggerMode != "enter" && triggerMode != "key" && triggerMode != "both" {
		return fmt.Errorf("--trigger must be one of: enter, key, both")
	}

	var pack *soundPack
	switch {
	case customPath != "":
		pack = &soundPack{name: "custom", dir: customPath, mode: modeRandom, custom: true}
	case sexyMode:
		pack = &soundPack{name: "sexy", fs: sexyAudio, dir: "audio/sexy", mode: modeEscalation}
	case haloMode:
		pack = &soundPack{name: "halo", fs: haloAudio, dir: "audio/halo", mode: modeRandom}
	default:
		pack = &soundPack{name: "pain", fs: painAudio, dir: "audio/pain", mode: modeRandom}
	}

	if err := pack.loadFiles(); err != nil {
		return fmt.Errorf("loading %s audio: %w", pack.name, err)
	}

	if testAudio {
		speakerInit := false
		testFile := pack.files[0]
		fmt.Printf("spank: startup audio test -> %s\n", testFile)
		if err := playAudio(pack, testFile, &speakerInit); err != nil {
			return fmt.Errorf("startup audio test failed: %w", err)
		}
		fmt.Println("spank: startup audio test OK")
	}

	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
	defer cancel()

	cooldown := time.Duration(cooldownMs) * time.Millisecond
	return listenForManualTriggers(ctx, pack, cooldown)
}

func listenForManualTriggers(ctx context.Context, pack *soundPack, cooldown time.Duration) error {
	tracker := newSlapTracker(pack, cooldown)
	speakerInit := false
	var lastYell time.Time

	fmt.Printf("spank: %s mode active (manual trigger on %s)\n", pack.name, runtimeName())
	fmt.Printf("Trigger mode: %s\n", triggerMode)
	fmt.Println("Controls: q to quit. Enter triggers in enter/both modes; any key triggers in key/both modes.")

	inputChannel := make(chan string)
	if triggerMode == "enter" || triggerMode == "both" {
		go func() {
			scanner := bufio.NewScanner(os.Stdin)
			for scanner.Scan() {
				inputChannel <- strings.TrimSpace(scanner.Text())
			}
			close(inputChannel)
		}()
	}

	var keyChannel <-chan byte
	restoreRaw := func() {}
	if triggerMode == "key" || triggerMode == "both" {
		channel, restore, err := startKeypressReader()
		if err != nil {
			if triggerMode == "key" {
				return err
			}
			fmt.Fprintf(os.Stderr, "spank: key trigger unavailable, continuing with enter mode only: %v\n", err)
		} else {
			keyChannel = channel
			restoreRaw = restore
		}
	}
	defer restoreRaw()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nbye!")
			return nil
		case key, ok := <-keyChannel:
			if !ok {
				keyChannel = nil
				continue
			}

			if key == 'q' || key == 'Q' || key == 3 {
				fmt.Println("bye!")
				return nil
			}

			if key == '\r' || key == '\n' {
				continue
			}

			if time.Since(lastYell) <= cooldown {
				continue
			}

			now := time.Now()
			lastYell = now
			number, score := tracker.record(now)
			file := tracker.getFile(score)
			fmt.Printf("trigger #%d [key amp=1.00000g] -> %s\n", number, file)
			if err := playAudio(pack, file, &speakerInit); err != nil {
				fmt.Fprintf(os.Stderr, "spank: playback error: %v\n", err)
			}
		case line, ok := <-inputChannel:
			if !ok {
				inputChannel = nil
				if keyChannel == nil {
					return nil
				}
				continue
			}
			if strings.EqualFold(line, "q") || strings.EqualFold(line, "quit") || strings.EqualFold(line, "exit") {
				fmt.Println("bye!")
				return nil
			}
			if time.Since(lastYell) <= cooldown {
				continue
			}

			now := time.Now()
			lastYell = now
			number, score := tracker.record(now)
			file := tracker.getFile(score)
			fmt.Printf("trigger #%d [manual amp=1.00000g] -> %s\n", number, file)
			if err := playAudio(pack, file, &speakerInit); err != nil {
				fmt.Fprintf(os.Stderr, "spank: playback error: %v\n", err)
			}
		}
	}
}

func startKeypressReader() (<-chan byte, func(), error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, nil, errors.New("stdin is not a terminal; key mode needs an interactive console")
	}

	state, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to enable raw key mode: %w", err)
	}

	channel := make(chan byte, 16)
	go func() {
		defer close(channel)
		buffer := make([]byte, 1)
		for {
			count, readErr := os.Stdin.Read(buffer)
			if readErr != nil {
				return
			}
			if count == 1 {
				channel <- buffer[0]
			}
		}
	}()

	restore := func() {
		_ = term.Restore(int(os.Stdin.Fd()), state)
	}

	return channel, restore, nil
}

func runtimeName() string {
	return fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)
}

var speakerMu sync.Mutex

func playAudio(pack *soundPack, path string, speakerInit *bool) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("audio panic: %v", recovered)
		}
	}()

	var streamer beep.StreamSeekCloser
	var format beep.Format

	if pack.custom {
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		defer file.Close()
		streamer, format, err = mp3.Decode(file)
		if err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
	} else {
		data, err := pack.fs.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		streamer, format, err = mp3.Decode(io.NopCloser(bytes.NewReader(data)))
		if err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
	}
	defer streamer.Close()

	speakerMu.Lock()
	if !*speakerInit {
		speaker.Init(format.SampleRate, format.SampleRate.N(time.Second/10))
		*speakerInit = true
	}
	speakerMu.Unlock()

	done := make(chan bool)
	speaker.Play(beep.Seq(streamer, beep.Callback(func() {
		done <- true
	})))
	<-done
	return nil
}
