package handlers

import (
	"strings"
	"testing"
	"time"

	storconfig "github.com/krau/SaveAny-Bot/config/storage"
)

func TestParseVideoSplitValue(t *testing.T) {
	cases := []struct {
		mode, in string
		want     int64
		ok       bool
	}{
		{storconfig.SplitModeDuration, "119", 119, true},
		{storconfig.SplitModeDuration, " 120秒 ", 120, true},
		{storconfig.SplitModeDuration, "90s", 90, true},
		{storconfig.SplitModeDuration, "5", 0, false},
		{storconfig.SplitModeDuration, "3601", 0, false},
		{storconfig.SplitModeDuration, "abc", 0, false},
		{storconfig.SplitModeSize, "500", 500, true},
		{storconfig.SplitModeSize, "500MB", 500, true},
		{storconfig.SplitModeSize, "0", 0, false},
		{storconfig.SplitModeSize, "2001", 0, false},
	}
	for _, c := range cases {
		got, err := parseVideoSplitValue(c.mode, c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("parseVideoSplitValue(%q,%q) = %d,%v; want %d ok=%v", c.mode, c.in, got, err, c.want, c.ok)
		}
	}
}

func TestVideoSplitInputStoreExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	s := &videoSplitInputStore{pending: map[string]videoSplitPending{}, now: func() time.Time { return now }}
	s.set(1, 2, videoSplitPending{storageID: 7, mode: storconfig.SplitModeSize})
	if _, ok := s.take(1, 3); ok {
		t.Fatal("pending input leaked to another chat")
	}
	p, ok := s.take(1, 2)
	if !ok || p.storageID != 7 {
		t.Fatalf("take() = %+v,%v", p, ok)
	}
	if _, ok := s.take(1, 2); ok {
		t.Fatal("pending input should be consumed once")
	}
	s.set(1, 2, videoSplitPending{storageID: 7})
	now = now.Add(videoSplitInputTTL + time.Second)
	if _, ok := s.take(1, 2); ok {
		t.Fatal("expired input should be ignored")
	}
}

func TestVideoSplitCallbackDataFits(t *testing.T) {
	data := vsplitData("m", uint(4294967295), storconfig.SplitModeDuration)
	if len(data) > 64 {
		t.Fatalf("callback data too long: %d", len(data))
	}
}

func TestParseVideoRecompressMB(t *testing.T) {
	if v, err := parseVideoRecompressMB(" 300MB "); err != nil || v != 300 {
		t.Fatalf("parse 300MB = %d, %v", v, err)
	}
	for _, bad := range []string{"abc", "5", "5000"} {
		if _, err := parseVideoRecompressMB(bad); err == nil {
			t.Fatalf("%q should be rejected", bad)
		}
	}
}

func TestVideoRecompressMenu(t *testing.T) {
	if data := vrecompData("q", uint(4294967295), 26); len(data) > 64 {
		t.Fatalf("callback data too long: %d", len(data))
	}
	st := &storconfig.TelegramStorageConfig{}
	mk := videoRecompressDetailMarkup(st)
	if mk == nil || len(mk.Rows) != 4 || len(mk.Rows[1].Buttons) != 3 {
		t.Fatalf("unexpected recompress markup: %+v", mk)
	}
	text := videoRecompressDetailText(st, "")
	for _, want := range []string{"已关闭", "CRF 20", "200 MB"} {
		if !strings.Contains(text, want) {
			t.Fatalf("detail text %q missing %q", text, want)
		}
	}
}
