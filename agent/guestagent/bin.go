package guestagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"

	"briard.io/agent/guestfirmware"
)

// The HOST end of the push protocol ([B.86j], re-cut by [B.138]): the three verbs the host
// dresses a guest through. The guest end -- staging, the cheap gate, the trial and its aftermath
// -- is the FIRMWARE's ([B.139], agent/guestfirmware/bin.go), because it is the one part of the
// protocol the image bakes and the pushed agent must keep speaking unchanged.

// BinStage streams one binary to the guest in BinChunkSize chunks and has the guest verify the
// whole file's sha256 before it becomes <name>.next.
func (g *Client) BinStage(ctx context.Context, name string, r io.Reader) error {
	h := sha256.New()
	buf := make([]byte, guestfirmware.BinChunkSize)
	seq := 0
	for {
		n, err := io.ReadFull(r, buf)
		last := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
		if err != nil && !last {
			return err
		}
		h.Write(buf[:n])
		c := guestfirmware.BinChunk{Name: name, Seq: seq, Data: buf[:n], Last: last}
		if last {
			c.SHA256 = hex.EncodeToString(h.Sum(nil))
		}
		if err := g.c.Call(ctx, guestfirmware.VerbBinStage, c, nil); err != nil {
			return err
		}
		if last {
			return nil
		}
		seq++
	}
}

// BinTest has the guest run every staged binary's --test-launch. An error names the binary
// that failed; the guest has discarded the whole staged set by then.
func (g *Client) BinTest(ctx context.Context, names []string) error {
	return g.c.Call(ctx, guestfirmware.VerbBinTest, guestfirmware.BinTest{Names: names}, nil)
}

// BinActivate arms the staged set and restarts the guest agent's unit, whose start is the
// trial. The call returns once the guest has accepted the restart; the channel then drops, and
// the caller learns the outcome from the next handshake's Bundle.
func (g *Client) BinActivate(ctx context.Context, release string, names []string) error {
	return g.c.Call(ctx, guestfirmware.VerbBinActivate, guestfirmware.BinActivation{Release: release, Names: names}, nil)
}

func (g *Client) SupportsBinPush() bool {
	return g.Supports(guestfirmware.VerbBinStage) && g.Supports(guestfirmware.VerbBinTest) && g.Supports(guestfirmware.VerbBinActivate)
}

// Bundle is the guest bundle the guest reported in its last handshake: the host release id its
// pushed binaries came from, "" while it runs the firmware baked into the image.
func (g *Client) Bundle() string { return g.bundle }
