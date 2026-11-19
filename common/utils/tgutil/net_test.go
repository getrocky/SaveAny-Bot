package tgutil

import (
	"testing"

	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
)

func TestOnlyIPv6(t *testing.T) {
	list := dcs.List{Options: []tg.DCOption{
		{ID: 5, IPAddress: "91.108.56.130"},
		{ID: 5, Ipv6: true, IPAddress: "2001:b28:f23f:f005::a"},
		{ID: 4, Ipv6: true, IPAddress: "2001:67c:04e8:f004::a"},
	}}
	got, ok := onlyIPv6(list, 5)
	if !ok || len(got.Options) != 1 || got.Options[0].IPAddress != "2001:b28:f23f:f005::a" {
		t.Fatalf("unexpected: %v %v", got.Options, ok)
	}
	if _, ok := onlyIPv6(dcs.List{Options: []tg.DCOption{{ID: 5, IPAddress: "1.2.3.4"}}}, 5); ok {
		t.Fatal("expected no IPv6 candidates")
	}
}
