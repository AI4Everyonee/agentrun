package collector

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/AI4Everyonee/agentrun/internal/db"
	"github.com/AI4Everyonee/agentrun/internal/recorder"
)

// Server is the long-lived collector process that listens on a Unix domain
// socket and dispatches events into the SQLite database.
type Server struct {
	socketPath string
	dbPath     string
	db         *sql.DB
	listener   net.Listener
	wg         sync.WaitGroup
	stopCh     chan struct{}
	stopOnce   sync.Once
}

// New creates a new Server configured to listen on socketPath and write to dbPath.
// It does NOT start listening yet; call Start for that.
func New(socketPath, dbPath string) (*Server, error) {
	return &Server{
		socketPath: socketPath,
		dbPath:     dbPath,
		stopCh:     make(chan struct{}),
	}, nil
}

// Start opens the database, binds the Unix socket, and begins accepting
// connections in the background. Returns once the listener is bound and ready.
func (s *Server) Start() error {
	// Open or create the database.
	if _, err := os.Stat(s.dbPath); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("collector: stat db: %w", err)
		}
		if mkErr := os.MkdirAll(filepath.Dir(s.dbPath), 0o755); mkErr != nil {
			return fmt.Errorf("collector: mkdir db dir: %w", mkErr)
		}
		d, err := db.Open(s.dbPath)
		if err != nil {
			return fmt.Errorf("collector: open db (new): %w", err)
		}
		s.db = d
	} else {
		d, err := db.OpenReadWrite(s.dbPath)
		if err != nil {
			return fmt.Errorf("collector: open db: %w", err)
		}
		s.db = d
	}

	// Remove stale socket file if present.
	_ = os.Remove(s.socketPath)

	// Ensure the socket directory exists.
	if mkErr := os.MkdirAll(filepath.Dir(s.socketPath), 0o755); mkErr != nil {
		return fmt.Errorf("collector: mkdir socket dir: %w", mkErr)
	}

	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("collector: listen %s: %w", s.socketPath, err)
	}
	s.listener = ln

	s.wg.Add(1)
	go s.acceptLoop()

	return nil
}

// Shutdown stops accepting new connections, waits for in-flight handlers to
// finish (honoring ctx deadline), then closes the database. Idempotent.
func (s *Server) Shutdown(ctx context.Context) error {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		_ = s.listener.Close()
		_ = os.Remove(s.socketPath)
	})

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}

	if s.db != nil {
		err := s.db.Close()
		s.db = nil
		return err
	}
	return nil
}

// acceptLoop is the server's main goroutine: it accepts connections and
// dispatches each to a handler goroutine.
func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.stopCh:
				return
			default:
				// Transient accept error; log and stop the loop.
				fmt.Fprintf(os.Stderr, "agentrun collector: accept: %v\n", err)
				return
			}
		}
		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			s.handle(c)
		}(conn)
	}
}

// handle processes one client connection: read request → ingest → write response.
// Each connection is one-shot; the connection is closed on return.
func (s *Server) handle(conn net.Conn) {
	defer conn.Close()

	// Set an end-to-end deadline for the connection.
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		_, _ = conn.Write([]byte{ResponseErr, '\n'})
		return
	}

	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		_, _ = conn.Write([]byte{ResponseErr, '\n'})
		return
	}

	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		_, _ = conn.Write([]byte{ResponseErr, '\n'})
		return
	}

	ingestReq := recorder.IngestRequest{
		Agent:     req.Agent,
		Event:     req.Event,
		SessionID: req.SessionID,
		Payload:   req.Payload,
		Native:    req.Native,
	}

	if err := recorder.IngestEvent(s.db, ingestReq); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun collector: ingest: %v\n", err)
		_, _ = conn.Write([]byte{ResponseErr, '\n'})
		return
	}

	_, _ = conn.Write([]byte{ResponseOK, '\n'})
}
