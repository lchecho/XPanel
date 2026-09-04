package web

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"xpanel/internal/config"
)

type Server struct {
	http  *http.Server
	ready atomic.Bool
}

func NewServer(cfg config.Config, deps RouteDependencies) (*Server, error) {
	server := &Server{}
	deps.Ready = server.ready.Load
	handler, err := Routes(deps)
	if err != nil {
		return nil, err
	}
	server.http = &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           handler,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Duration,
		ReadTimeout:       cfg.Server.RequestTimeout.Duration,
		WriteTimeout:      cfg.Server.RequestTimeout.Duration,
		IdleTimeout:       60 * time.Second,
	}
	return server, nil
}

func (s *Server) SetReady(value bool)   { s.ready.Store(value) }
func (s *Server) Handler() http.Handler { return s.http.Handler }

func (s *Server) ListenAndServe() error {
	err := s.http.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.ready.Store(false)
	return s.http.Shutdown(ctx)
}
