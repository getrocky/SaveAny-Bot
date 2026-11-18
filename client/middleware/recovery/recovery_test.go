package recovery

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

type countingInvoker struct{ calls int }

func (c *countingInvoker) Invoke(_ context.Context, input bin.Encoder, _ bin.Decoder) error {
	c.calls++
	var buf bin.Buffer
	if err := input.Encode(&buf); err != nil {
		return err
	}
	return errors.New("network down")
}

func newTestBackoff() backoff.BackOff {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = time.Millisecond
	b.MaxElapsedTime = 200 * time.Millisecond
	return b
}

// An unencodable request must fail at once instead of being retried for the
// whole backoff window (which froze bot menus for ~5 minutes in production).
func TestUnencodableRequestIsNotRetried(t *testing.T) {
	var nilMarkup *tg.ReplyInlineMarkup
	req := &tg.MessagesSendMessageRequest{Peer: &tg.InputPeerSelf{}, Message: "x", ReplyMarkup: nilMarkup}
	inv := &countingInvoker{}
	h := New(context.Background(), newTestBackoff).Handle(inv)
	if err := h(context.Background(), req, nil); err == nil {
		t.Fatal("expected error")
	}
	if inv.calls != 1 {
		t.Fatalf("expected 1 call, got %d", inv.calls)
	}
}

func TestNetworkErrorIsStillRetried(t *testing.T) {
	req := &tg.MessagesSendMessageRequest{Peer: &tg.InputPeerSelf{}, Message: "x"}
	inv := &countingInvoker{}
	h := New(context.Background(), newTestBackoff).Handle(inv)
	_ = h(context.Background(), req, nil)
	if inv.calls < 2 {
		t.Fatalf("expected retries, got %d call(s)", inv.calls)
	}
}
