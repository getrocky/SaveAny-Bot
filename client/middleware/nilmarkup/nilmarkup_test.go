package nilmarkup

import (
	"context"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

type encodeInvoker struct{ err error }

func (e *encodeInvoker) Invoke(_ context.Context, input bin.Encoder, _ bin.Decoder) error {
	var buf bin.Buffer
	e.err = input.Encode(&buf)
	return e.err
}

func typedNil() tg.ReplyMarkupClass {
	var m *tg.ReplyInlineMarkup
	return m
}

// Reproduces the production error: a typed nil inline keyboard makes gotd fail
// to encode messages.sendMessage.
func TestTypedNilMarkupFailsToEncode(t *testing.T) {
	req := &tg.MessagesSendMessageRequest{Peer: &tg.InputPeerSelf{}, Message: "x", ReplyMarkup: typedNil()}
	var buf bin.Buffer
	err := req.Encode(&buf)
	if err == nil || !strings.Contains(err.Error(), "can't encode replyInlineMarkup#b2b15770 as nil") {
		t.Fatalf("expected typed-nil encode error, got %v", err)
	}
}

func TestMiddlewareClearsTypedNilMarkup(t *testing.T) {
	inv := &encodeInvoker{}
	h := New().Handle(inv)
	reqs := []bin.Encoder{
		&tg.MessagesSendMessageRequest{Peer: &tg.InputPeerSelf{}, Message: "x", ReplyMarkup: typedNil()},
		&tg.MessagesEditMessageRequest{Peer: &tg.InputPeerSelf{}, ID: 1, ReplyMarkup: typedNil()},
		&tg.MessagesSendMediaRequest{Peer: &tg.InputPeerSelf{}, Media: &tg.InputMediaEmpty{}, ReplyMarkup: typedNil()},
	}
	for _, r := range reqs {
		if err := h(context.Background(), r, nil); err != nil {
			t.Fatalf("%T: unexpected encode error after sanitize: %v", r, err)
		}
	}
}

func TestMiddlewareKeepsRealMarkup(t *testing.T) {
	mk := &tg.ReplyInlineMarkup{}
	req := &tg.MessagesSendMessageRequest{Peer: &tg.InputPeerSelf{}, Message: "x", ReplyMarkup: mk}
	Sanitize(req)
	if req.ReplyMarkup != mk {
		t.Fatal("non-nil markup must be kept")
	}
}

func TestIsNil(t *testing.T) {
	if !IsNil(nil) || !IsNil(typedNil()) || IsNil(&tg.ReplyInlineMarkup{}) {
		t.Fatal("IsNil mismatch")
	}
}
