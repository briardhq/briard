package guestagent

import (
	"context"
	"time"

	"briard.io/agent/files"
)

type driveAccountRequest struct {
	Password string `json:"password"`
}

// setDriveAccount makes the host's password the one that opens the drive (files.SetDriveAccount).
func setDriveAccount(ctx context.Context, x Executor, password string) error {
	return files.SetDriveAccount(ctx, x, password, time.Now())
}

// SetDriveAccount hands the guest the password this host opens the drive with
// (verbSetDriveAccount).
func (g *Client) SetDriveAccount(ctx context.Context, password string) error {
	return g.c.Call(ctx, verbSetDriveAccount, driveAccountRequest{Password: password}, nil)
}

// SupportsDriveAccount reports whether this guest can be handed the drive's password.
func (g *Client) SupportsDriveAccount() bool { return g.Supports(verbSetDriveAccount) }
