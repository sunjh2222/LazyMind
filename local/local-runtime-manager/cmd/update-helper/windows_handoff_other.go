//go:build !windows

package main

import (
	"context"
	"io"

	"github.com/mesotron7x/LazyMind/local/local-runtime-manager/internal/desktopupdate"
)

func runWindowsHandoff(context.Context, []string, io.Writer, desktopupdate.Config, string) error {
	return &desktopupdate.Failure{Code: "native_install_unavailable"}
}
