//go:build !linux

package egress

import (
	"context"
	"errors"
	"net"
)

// ServeTransparent needs Linux's SO_ORIGINAL_DST.
func (p *Proxy) ServeTransparent(ctx context.Context, l net.Listener) error {
	return errors.New("the transparent egress proxy needs Linux")
}
