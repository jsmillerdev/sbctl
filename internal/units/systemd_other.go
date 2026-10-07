//go:build !linux

package units

import (
	"context"
	"errors"
	"log/slog"

	"github.com/OWNER/sbctl/internal/config"
)

// Systemd is only available on Linux; elsewhere NewSystemd fails and the exec
// backend (supervisor = "exec") is the way to run sbctl for development.
type Systemd struct{}

var _ Supervisor = (*Systemd)(nil)

var errNoSystemd = errors.New("units: the systemd supervisor is only available on Linux; set supervisor = \"exec\"")

func NewSystemd(*config.Config, *slog.Logger) (*Systemd, error) { return nil, errNoSystemd }

func (*Systemd) Close()                                            {}
func (*Systemd) Reload(context.Context) error                      { return errNoSystemd }
func (*Systemd) RenderChanged(context.Context, Spec) (bool, error) { return false, errNoSystemd }
func (*Systemd) Render(context.Context, Spec) error                { return errNoSystemd }
func (*Systemd) Start(context.Context, string) error               { return errNoSystemd }
func (*Systemd) Stop(context.Context, string) error                { return errNoSystemd }
func (*Systemd) Remove(context.Context, string) error              { return errNoSystemd }
func (*Systemd) Status(context.Context, string) (Status, error)    { return Status{}, errNoSystemd }

func (*Systemd) Sandboxed() bool { return true }
