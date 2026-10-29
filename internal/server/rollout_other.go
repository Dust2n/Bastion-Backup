//go:build !windows

package server

import (
	"context"
	"errors"
)

const smbPushSupported = false

func (s *Server) pushSMB(ctx context.Context, target string, c *cred, tok string, progress func(string)) error {
	return errors.New("SMB-Verteilung ist nur möglich, wenn der Server unter Windows läuft; bitte WinRM verwenden")
}

func (s *Server) checkSMB(target string, c *cred, add func(name string, err error, detail string)) bool {
	add("SMB", errors.New("nur mit Windows-Server möglich"), "")
	return false
}
