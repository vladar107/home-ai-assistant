package eval

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// terminalDeliverer reports the ceremony's outcome to the terminal and signals that it is over.
type terminalDeliverer struct {
	out  io.Writer
	done chan struct{}
}

func (d terminalDeliverer) SendMessage(_ context.Context, _ int64, text string) error {
	_, _ = fmt.Fprintln(d.out, text)
	close(d.done)
	return nil
}

// Login runs the device-code flow for the eval principal, printing the verification address and
// code to out, and binds the account in the eval store. The credential is written to the eval
// profile with the application's own private permissions.
func Login(ctx context.Context, profiles Profiles, endpoints Endpoints, out io.Writer) error {
	store, err := profiles.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()

	deliverer := terminalDeliverer{out: out, done: make(chan struct{})}
	service, err := newService(profiles, store, deliverer, endpoints)
	if err != nil {
		return err
	}
	if err := service.Start(ctx); err != nil {
		return err
	}
	defer service.Shutdown(context.WithoutCancel(ctx))

	ceremony, err := service.Connect(ctx, PrincipalID, 0)
	if err != nil {
		return fmt.Errorf("start login: %w", err)
	}
	_, _ = fmt.Fprintf(out, "Open %s and enter the code %s (valid until %s).\n",
		ceremony.VerificationURL, ceremony.UserCode, ceremony.ExpiresAt.Format("15:04:05"))

	select {
	case <-deliverer.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	status, err := service.Status(ctx, PrincipalID)
	if err != nil {
		return err
	}
	if !status.Connected {
		return errors.New("login did not complete")
	}
	return nil
}
