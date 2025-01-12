package hls

import (
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/m1k1o/go-transcode/internal/utils"
)

// how often should be cleanup called
const cleanupPeriod = 4 * time.Second

// timeot for first playlist, when it waits for new data
const playlistTimeout = 20 * time.Second

// minimum segments available to consider stream as active
const hlsMinimumSegments = 2

// how long must be active stream idle to be considered as dead
const activeIdleTimeout = 12 * time.Second

// how long must be iactive stream idle to be considered as dead
const inactiveIdleTimeout = 24 * time.Second

type ManagerCtx struct {
	logger     zerolog.Logger
	mu         sync.Mutex
	cmdFactory func() *exec.Cmd
	active     bool
	events     struct {
		onStart  func()
		onCmdLog func(message string)
		onStop   func()
	}

	cmd         *exec.Cmd
	tempdir     string
	lastRequest time.Time

	sequence int
	playlist string

	playlistLoad chan string
	shutdown     chan interface{}
}

func New(cmdFactory func() *exec.Cmd) *ManagerCtx {
	return &ManagerCtx{
		logger:     log.With().Str("module", "hls").Str("submodule", "manager").Logger(),
		cmdFactory: cmdFactory,

		playlistLoad: make(chan string),
		shutdown:     make(chan interface{}),
	}
}

func (m *ManagerCtx) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cmd != nil {
		return errors.New("has already started")
	}

	m.logger.Debug().Msg("performing start")

	var err error
	m.tempdir, err = os.MkdirTemp("", "go-transcode-hls")
	if err != nil {
		return err
	}

	m.cmd = m.cmdFactory()
	m.cmd.Dir = m.tempdir

	if m.events.onCmdLog != nil {
		m.cmd.Stderr = utils.LogEvent(m.events.onCmdLog)
	} else {
		m.cmd.Stderr = utils.LogWriter(m.logger)
	}

	read, write := io.Pipe()
	m.cmd.Stdout = write

	//create a new process group
	m.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	m.active = false
	m.lastRequest = time.Now()

	m.sequence = 0
	m.playlist = ""

	m.playlistLoad = make(chan string)
	m.shutdown = make(chan interface{})

	go func() {
		buf := make([]byte, 1024)

		for {
			n, err := read.Read(buf)
			if n != 0 {
				m.playlist = string(buf[:n])
				m.sequence = m.sequence + 1

				m.logger.Info().
					Int("sequence", m.sequence).
					Str("playlist", m.playlist).
					Msg("received playlist")

				if m.sequence == hlsMinimumSegments {
					m.active = true
					m.playlistLoad <- m.playlist
					close(m.playlistLoad)
				}
			}

			if err != nil {
				m.logger.Err(err).Msg("cmd read failed")
				return
			}
		}
	}()

	go func() {
		ticker := time.NewTicker(cleanupPeriod)
		defer ticker.Stop()

		for {
			select {
			case <-m.shutdown:
				write.Close()
				return
			case <-ticker.C:
				m.Cleanup()
			}
		}
	}()

	if m.events.onStart != nil {
		m.events.onStart()
	}

	return m.cmd.Start()
}

func (m *ManagerCtx) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cmd == nil {
		return
	}

	m.logger.Debug().Msg("performing stop")
	close(m.shutdown)

	if m.cmd.Process != nil {
		pgid, err := syscall.Getpgid(m.cmd.Process.Pid)
		if err == nil {
			err := syscall.Kill(-pgid, syscall.SIGKILL)
			m.logger.Err(err).Msg("killing proccess group")
		} else {
			m.logger.Err(err).Msg("could not get proccess group id")
			err := m.cmd.Process.Kill()
			m.logger.Err(err).Msg("killing proccess")
		}
		m.cmd = nil
	}

	time.AfterFunc(2*time.Second, func() {
		err := os.RemoveAll(m.tempdir)
		m.logger.Err(err).Msg("removing tempdir")
	})

	if m.events.onStop != nil {
		m.events.onStop()
	}
}

func (m *ManagerCtx) Cleanup() {
	m.mu.Lock()
	diff := time.Since(m.lastRequest)
	stop := m.active && diff > activeIdleTimeout || !m.active && diff > inactiveIdleTimeout
	m.mu.Unlock()

	m.logger.Debug().
		Time("last_request", m.lastRequest).
		Dur("diff", diff).
		Bool("active", m.active).
		Bool("stop", stop).
		Msg("performing cleanup")

	if stop {
		m.Stop()
	}
}

func (m *ManagerCtx) ServePlaylist(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.lastRequest = time.Now()
	m.mu.Unlock()

	playlist := m.playlist

	if m.cmd == nil {
		err := m.Start()
		if err != nil {
			m.logger.Warn().Err(err).Msg("transcode could not be started")
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
			return
		}
	}

	if !m.active {
		select {
		case playlist = <-m.playlistLoad:
		case <-m.shutdown:
			m.logger.Warn().Msg("playlist load failed because of shutdown")
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("404 playlist not found"))
			return
		case <-time.After(playlistTimeout):
			m.logger.Warn().Msg("playlist load channel timeouted")
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("500 not available"))
			return
		}
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write([]byte(playlist))
}

func (m *ManagerCtx) ServeMedia(w http.ResponseWriter, r *http.Request) {
	fileName := path.Base(r.URL.RequestURI())
	path := path.Join(m.tempdir, fileName)

	if _, err := os.Stat(path); os.IsNotExist(err) {
		m.logger.Warn().Str("path", path).Msg("media file not found")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("404 media not found"))
		return
	}

	m.mu.Lock()
	m.lastRequest = time.Now()
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, path)
}

func (m *ManagerCtx) OnStart(event func()) {
	m.events.onStart = event
}

func (m *ManagerCtx) OnCmdLog(event func(message string)) {
	m.events.onCmdLog = event
}

func (m *ManagerCtx) OnStop(event func()) {
	m.events.onStop = event
}
