//go:build !linux

package importer

import (
	"context"
	"errors"
	"log/slog"

	"github.com/motwok/npm-switchyard/internal/config"
	"github.com/motwok/npm-switchyard/internal/docker"
	"github.com/motwok/npm-switchyard/internal/metrics"
)

// Importer is Linux only in 1.0 (SC-3).
type Importer struct{}

func Setup(context.Context, *docker.Client, string, *config.Config, Opener, *metrics.Registry,
	*slog.Logger) (*Importer, error) {
	return nil, errors.New("import is only supported on Linux")
}

func (*Importer) Serve(context.Context, chan<- struct{}) error { return nil }
