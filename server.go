// server.go - Katzenpost server.
// Copyright (C) 2017  Yawning Angel.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

// Package server provides the Katzenpost server.
package server

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"

	"github.com/eapache/channels"
	"github.com/katzenpost/core/crypto/ecdh"
	"github.com/katzenpost/core/crypto/eddsa"
	"github.com/katzenpost/server/config"
	"github.com/op/go-logging"
)

const fileMode = 0600

var (
	// ErrGenerateOnly is the error returned when the server initialization
	// terminates due to the `GenerateOnly` debug config option.
	ErrGenerateOnly = errors.New("server: GenerateOnly set")

	errNotImplemented = errors.New("server: Not implemented yet")
)

// Server is a Katzenpost server instance.
type Server struct {
	cfg *config.Config

	identityKey *eddsa.PrivateKey
	linkKey     *ecdh.PrivateKey

	logBackend logging.LeveledBackend
	log        *logging.Logger

	inboundPackets *channels.InfiniteChannel

	scheduler     *serverScheduler
	cryptoWorkers []*cryptoWorker
	periodic      *periodicTimer
	mixKeys       *mixKeys
	pki           *pki
	listeners     []*listener
	connector     *connector

	haltOnce sync.Once
}

func (s *Server) initDataDir() error {
	const dirMode = os.ModeDir | 0700
	d := s.cfg.Server.DataDir

	// Initialize the data directory, by ensuring that it exists (or can be
	// created), and that it has the appropriate permissions.
	if fi, err := os.Lstat(d); err != nil {
		// Directory doesn't exist, create one.
		if !os.IsNotExist(err) {
			return fmt.Errorf("server: failed to stat() DataDir: %v", err)
		}
		if err = os.Mkdir(d, dirMode); err != nil {
			return fmt.Errorf("server: failed to create DataDir: %v", err)
		}
	} else {
		if !fi.IsDir() {
			return fmt.Errorf("server: DataDir '%v' is not a directory", d)
		}
		if fi.Mode() != dirMode {
			return fmt.Errorf("server: DataDir '%v' has invalid permissions '%v'", d, fi.Mode())
		}
	}

	return nil
}

func (s *Server) initLogging() error {
	// Figure out where the log should go to, creating a log file as needed.
	var f io.Writer
	if s.cfg.Logging.Disable {
		f = ioutil.Discard
	} else if s.cfg.Logging.File == "" {
		f = os.Stdout
	} else {
		p := s.cfg.Logging.File
		if !filepath.IsAbs(p) {
			p = filepath.Join(s.cfg.Server.DataDir, p)
		}

		var err error
		flags := os.O_CREATE | os.O_APPEND | os.O_WRONLY
		f, err = os.OpenFile(p, flags, fileMode)
		if err != nil {
			return fmt.Errorf("server: failed to create log file: %v", err)
		}
	}

	// Create a new log backend, using the configured output, and initialize
	// the server logger.
	//
	// TODO: Maybe use a custom backend to support rotating the log file.
	var b logging.Backend
	b = logging.NewLogBackend(f, "", 0)
	s.logBackend = b.(logging.LeveledBackend)
	s.logBackend.SetLevel(logLevelFromString(s.cfg.Logging.Level), "")
	s.log = s.newLogger("server")

	return nil
}

func (s *Server) newLogger(module string) *logging.Logger {
	l := logging.MustGetLogger(module)
	l.SetBackend(s.logBackend)
	return l
}

func (s *Server) reshadowCryptoWorkers() {
	s.log.Debugf("Calling all crypto workers to re-shadow the mix keys.")
	for _, w := range s.cryptoWorkers {
		w.updateMixKeys()
	}
}

// Shutdown cleanly shuts down a given Server instance.
func (s *Server) Shutdown() {
	s.haltOnce.Do(func() { s.halt() })
}

func (s *Server) halt() {
	// WARNING: The ordering of operations here is deliberate, and should not
	// be altered without a deep understanding of how all the components fit
	// together.

	s.log.Noticef("Starting graceful shutdown.")

	// Stop the 1 Hz periodic utility timer.
	if s.periodic != nil {
		s.periodic.halt()
		s.periodic = nil
	}

	// Stop the listener(s), close all incoming connections.
	for i, l := range s.listeners {
		if l != nil {
			l.halt() // Closes all connections.
			s.listeners[i] = nil
		}
	}

	// Close all outgoing connections.
	if s.connector != nil {
		s.connector.halt()
		// Don't nil this out till after the PKI has been torn down.
	}

	// Stop the Sphinx workers.
	for i, w := range s.cryptoWorkers {
		if w != nil {
			w.halt()
			s.cryptoWorkers[i] = nil
		}
	}

	// Stop the scheduler.
	if s.scheduler != nil {
		s.scheduler.halt()
		s.scheduler = nil
	}

	// Provider specific cleanup.
	if s.cfg.Server.IsProvider {
		// XXX/provider: Implement.
	}

	// Stop the PKI interface.
	if s.pki != nil {
		s.pki.halt()
		s.pki = nil
		s.connector = nil // PKI calls into the connector.
	}

	// Flush and close the mix keys.
	if s.mixKeys != nil {
		s.mixKeys.halt()
		s.mixKeys = nil
	}

	// Clean up the top level components.
	s.inboundPackets.Close()
	s.linkKey.Reset()
	s.identityKey.Reset()

	s.log.Noticef("Shutdown complete.")
}

// New returns a new Server instance parameterized with the specified
// configuration.
func New(cfg *config.Config) (*Server, error) {
	s := new(Server)
	s.cfg = cfg

	// Do the early initialization and bring up logging.
	if err := s.initDataDir(); err != nil {
		return nil, err
	}
	if err := s.initLogging(); err != nil {
		return nil, err
	}

	s.log.Notice("Katzenpost is still pre-alpha.  DO NOT DEPEND ON IT FOR STRONG SECURITY OR ANONYMITY.")
	if s.cfg.Debug.IsUnsafe() {
		s.log.Warning("Unsafe Debug configuration options are set.")
	}
	if s.cfg.Logging.Level == "DEBUG" {
		s.log.Warning("Unsafe Debug logging is enabled.")
	}
	s.log.Notice("Server identifier is: '%v'", s.cfg.Server.Identifier)

	// Initialize the server identity and link keys.
	if err := s.initIdentity(); err != nil {
		s.log.Errorf("Failed to initialize identity: %v", err)
		return nil, err
	}
	s.log.Noticef("Server identity public key is: %s", eddsaToPrintString(s.identityKey.PublicKey()))
	if err := s.initLink(); err != nil {
		s.log.Errorf("Failed to initialize link key: %v", err)
		return nil, err
	}
	s.log.Noticef("Server link public key is: %s", ecdhToPrintString(s.linkKey.PublicKey()))

	// Load and or generate mix keys.
	var err error
	if s.mixKeys, err = newMixKeys(s); err != nil {
		s.log.Errorf("Failed to initialize mix keys: %v", err)
		return nil, err
	}

	// Past this point, failures need to call s.Shutdown() to do cleanup.
	isOk := false
	defer func() {
		// Something failed in bringing the server up, past the point where
		// files are open etc, clean up the partially constructed instance.
		if !isOk {
			s.Shutdown()
		}
	}()

	if s.cfg.Debug.GenerateOnly {
		return nil, ErrGenerateOnly
	}

	// Initialize the PKI interface.
	s.pki = newPKI(s)

	// XXX/provider: Initialize the provider backend.
	if s.cfg.Server.IsProvider {
		return nil, errNotImplemented
	}

	// Initialize and start the the scheduler.
	s.scheduler = newScheduler(s)

	// Initialize and start the Sphinx workers.
	s.inboundPackets = channels.NewInfiniteChannel()
	s.cryptoWorkers = make([]*cryptoWorker, 0, s.cfg.Debug.NumSphinxWorkers)
	for i := 0; i < s.cfg.Debug.NumSphinxWorkers; i++ {
		w := newCryptoWorker(s, i)
		s.cryptoWorkers = append(s.cryptoWorkers, w)
	}

	// Initialize the outgoing connection manager, and then start the PKI
	// worker.
	s.connector = newConnector(s)
	s.pki.startWorker()

	// Bring the listener(s) online.
	s.listeners = make([]*listener, 0, len(s.cfg.Server.Addresses))
	for i, addr := range s.cfg.Server.Addresses {
		l, err := newListener(s, i, addr)
		if err != nil {
			s.log.Errorf("Failed to spawn listener on address: %v (%v).", addr, err)
			return nil, err
		}
		s.listeners = append(s.listeners, l)
	}

	// Start the periodic 1 Hz utility timer.
	s.periodic = newPeriodicTimer(s)

	isOk = true
	return s, nil
}

func logLevelFromString(l string) logging.Level {
	switch l {
	case "ERROR":
		return logging.ERROR
	case "WARNING":
		return logging.WARNING
	case "NOTICE":
		return logging.NOTICE
	case "INFO":
		return logging.INFO
	case "DEBUG":
		return logging.DEBUG
	default:
		panic("BUG: invalid log level (post-validation)")
	}
}
