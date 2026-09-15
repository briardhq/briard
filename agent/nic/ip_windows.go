package nic

import (
	"context"
	"errors"
)

func ip(context.Context, ...string) ([]byte, error) { return nil, errors.ErrUnsupported }
