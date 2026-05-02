package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// User-editable defaults — change these and rebuild instead of passing flags every run.
const (
	defaultLogPath        = `C:\Program Files\Roberts Space Industries\StarCitizen\LIVE\Game.log`
	defaultSoundNamedPath = `C:\Sounds\BLACKFLAG\DANGERCLOSE.wav`
	defaultSoundNullPath  = `C:\Sounds\BLACKFLAG\INBOUND.wav`
)

var (
	serverReroutedNamed = regexp.MustCompile(`<Local Route Guard - Server Rerouted>.*NOT AUTH \| ([A-Z][A-Za-z0-9_]+)_(\d+)\[\d+\]`)
	serverReroutedNull  = regexp.MustCompile(`<Local Route Guard - Server Rerouted>.*\| NULL ENTITY\|`)
	vcfLocalDriver      = regexp.MustCompile(`<Vehicle Control Flow>.*\[(\d+)\].*'([A-Z][A-Za-z0-9_]+)_(\d+)'`)
	geidFromPlayerGEID  = regexp.MustCompile(`playerGEID=(\d+)`)
	geidFromSubscribe   = regexp.MustCompile(`<SubscribeToPlayerSocial> Subscribing to player (\d+) social topics`)
	timestampLine       = regexp.MustCompile(`<([0-9T:.Z-]+)>`)
)

type soundEvent struct {
	path  string
	label string
}

type config struct {
	logPath       string
	soundNamed    string
	soundNull     string
	soundGap      time.Duration
	includeNull   bool
	nullRateLimit time.Duration
	quiet         bool
	geidOverride  string
	replay        bool
}

type tracker struct {
	seenNamed     map[string]bool
	localOwnedIDs map[string]bool
	localGEID     string
	lastNullSeen  time.Time
	soundQueue    chan<- soundEvent
	cfg           *config
}

func newTracker(cfg *config, queue chan<- soundEvent) *tracker {
	return &tracker{
		seenNamed:     make(map[string]bool),
		localOwnedIDs: make(map[string]bool),
		soundQueue:    queue,
		cfg:           cfg,
		localGEID:     cfg.geidOverride,
	}
}

func (t *tracker) handleLine(line string) {
	if t.localGEID == "" {
		if m := geidFromPlayerGEID.FindStringSubmatch(line); m != nil {
			t.localGEID = m[1]
			log.Printf("auto-detected local GEID: %s", t.localGEID)
		} else if m := geidFromSubscribe.FindStringSubmatch(line); m != nil {
			t.localGEID = m[1]
			log.Printf("auto-detected local GEID: %s", t.localGEID)
		}
	}

	if m := vcfLocalDriver.FindStringSubmatch(line); m != nil {
		if t.localGEID != "" && m[1] == t.localGEID {
			t.localOwnedIDs[m[3]] = true
		}
	}

	ts := extractTimestamp(line)

	if m := serverReroutedNamed.FindStringSubmatch(line); m != nil {
		class, eid := m[1], m[2]
		full := class + "_" + eid
		if t.localOwnedIDs[eid] {
			fmt.Printf("[%s] LOCAL    %-50s → filtered (own ship)\n", ts, full)
			return
		}
		if t.seenNamed[eid] {
			return
		}
		t.seenNamed[eid] = true
		fmt.Printf("[%s] NAMED    %-50s → queued (%s)\n", ts, full, t.cfg.soundNamed)
		t.enqueue(soundEvent{path: t.cfg.soundNamed, label: "named"})
		return
	}

	if !t.cfg.includeNull {
		return
	}
	if serverReroutedNull.MatchString(line) {
		now := time.Now()
		if !t.lastNullSeen.IsZero() && now.Sub(t.lastNullSeen) < t.cfg.nullRateLimit {
			fmt.Printf("[%s] NULL     %-50s → rate-limited (skipped)\n", ts, "(unattributable arrival)")
			return
		}
		t.lastNullSeen = now
		fmt.Printf("[%s] NULL     %-50s → queued (%s)\n", ts, "(unattributable arrival)", t.cfg.soundNull)
		t.enqueue(soundEvent{path: t.cfg.soundNull, label: "null"})
	}
}

func (t *tracker) enqueue(ev soundEvent) {
	select {
	case t.soundQueue <- ev:
	default:
		log.Printf("sound queue full, dropping %s arrival", ev.label)
	}
}

func extractTimestamp(line string) string {
	if m := timestampLine.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func soundWorker(queue <-chan soundEvent, gap time.Duration, quiet bool) {
	for ev := range queue {
		if quiet {
			continue
		}
		if err := playSoundBlocking(ev.path); err != nil {
			log.Printf("sound playback failed for %s: %v", ev.label, err)
			continue
		}
		time.Sleep(gap)
	}
}

func playSoundBlocking(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		psCmd := fmt.Sprintf(`(New-Object System.Media.SoundPlayer "%s").PlaySync()`, path)
		cmd = exec.Command("powershell", "-NoProfile", "-Command", psCmd)
	case "darwin":
		cmd = exec.Command("afplay", path)
	case "linux":
		cmd = exec.Command("aplay", "-q", path)
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
	return cmd.Run()
}

func tailFile(path string, replay bool, lineHandler func(string)) error {
	var file *os.File
	var err error

	for {
		file, err = os.Open(path)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("open %s: %w", path, err)
		}
		log.Printf("waiting for log file: %s", path)
		time.Sleep(2 * time.Second)
	}
	defer file.Close()

	if !replay {
		if _, err := file.Seek(0, io.SeekEnd); err != nil {
			return fmt.Errorf("seek end: %w", err)
		}
	}

	reader := bufio.NewReader(file)
	var partial strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			partial.WriteString(line)
			if strings.HasSuffix(line, "\n") {
				lineHandler(strings.TrimRight(partial.String(), "\r\n"))
				partial.Reset()
			}
		}
		if err == io.EOF {
			if replay {
				return nil
			}
			currentPos, _ := file.Seek(0, io.SeekCurrent)
			stat, statErr := os.Stat(path)
			if statErr == nil && stat.Size() < currentPos {
				log.Printf("log file truncated, reopening")
				file.Close()
				file, err = os.Open(path)
				if err != nil {
					return fmt.Errorf("reopen: %w", err)
				}
				reader = bufio.NewReader(file)
				partial.Reset()
				continue
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
	}
}

func validateSoundPath(path string, label string) error {
	if path == "" {
		return fmt.Errorf("%s sound path is empty", label)
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s sound file not accessible: %s: %w", label, path, err)
	}
	return nil
}

func main() {
	cfg := &config{}
	flag.StringVar(&cfg.logPath, "log", defaultLogPath, "Path to Game.log")
	flag.StringVar(&cfg.soundNamed, "sound-named", defaultSoundNamedPath, "Sound file for NAMED arrivals")
	flag.StringVar(&cfg.soundNull, "sound-null", defaultSoundNullPath, "Sound file for NULL ENTITY arrivals")
	flag.DurationVar(&cfg.soundGap, "sound-gap", 500*time.Millisecond, "Silence between consecutive queued sounds")
	flag.BoolVar(&cfg.includeNull, "include-null", true, "Fire on NULL ENTITY arrivals")
	flag.DurationVar(&cfg.nullRateLimit, "null-rate-limit", 5*time.Second, "Min interval between NULL-triggered sound enqueues")
	flag.BoolVar(&cfg.replay, "replay", false, "Process log from start to EOF then exit (for testing)")
	flag.BoolVar(&cfg.quiet, "quiet", false, "Suppress sound playback (still log to stdout)")
	flag.StringVar(&cfg.geidOverride, "geid", "", "Override auto-detected local GEID")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	if !cfg.quiet {
		if err := validateSoundPath(cfg.soundNamed, "named"); err != nil {
			log.Fatalf("startup error: %v", err)
		}
		if cfg.includeNull {
			if err := validateSoundPath(cfg.soundNull, "null"); err != nil {
				log.Fatalf("startup error: %v", err)
			}
		}
	}

	log.Printf("BLACKFLAG starting")
	log.Printf("  log:           %s", cfg.logPath)
	log.Printf("  sound-named:   %s", cfg.soundNamed)
	if cfg.includeNull {
		log.Printf("  sound-null:    %s", cfg.soundNull)
	} else {
		log.Printf("  sound-null:    (NULL events disabled)")
	}
	log.Printf("  sound-gap:     %s", cfg.soundGap)
	log.Printf("  null-rate-lim: %s", cfg.nullRateLimit)
	if cfg.geidOverride != "" {
		log.Printf("  geid override: %s", cfg.geidOverride)
	}
	if cfg.replay {
		log.Printf("  REPLAY mode (will exit at EOF)")
	}
	if cfg.quiet {
		log.Printf("  QUIET mode (no sound playback)")
	}

	queue := make(chan soundEvent, 1024)
	workerDone := make(chan struct{})
	go func() {
		soundWorker(queue, cfg.soundGap, cfg.quiet)
		close(workerDone)
	}()

	t := newTracker(cfg, queue)
	if err := tailFile(cfg.logPath, cfg.replay, t.handleLine); err != nil {
		log.Fatalf("tail error: %v", err)
	}

	if cfg.replay {
		close(queue)
		<-workerDone
	}
}
