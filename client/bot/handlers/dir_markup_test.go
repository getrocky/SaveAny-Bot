package handlers

import (
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

func TestDirMenuMarkupEmptyIsUntypedNil(t *testing.T) {
	m := dirMenuMarkup(nil)
	if m != nil {
		t.Fatalf("expected untyped nil, got %#v", m)
	}
	req := &tg.MessagesSendMessageRequest{Peer: &tg.InputPeerSelf{}, Message: "x", ReplyMarkup: m}
	var buf bin.Buffer
	if err := req.Encode(&buf); err != nil {
		t.Fatalf("encode: %v", err)
	}
}
