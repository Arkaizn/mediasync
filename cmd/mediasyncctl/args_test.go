package main

import (
	"slices"
	"testing"

	"github.com/arkaizn/mediasync/internal/model"
)

func TestSplitPlayItemsArgs(t *testing.T) {
	cases := []struct {
		args, ids, flags []string
	}{
		{[]string{"a", "b"}, []string{"a", "b"}, nil},
		{[]string{"a", "--shuffle", "b"}, []string{"a", "b"}, []string{"--shuffle"}},
		{[]string{"--shuffle", "a", "--mode", "next", "b"}, []string{"a", "b"}, []string{"--shuffle", "--mode", "next"}},
		{[]string{"a", "--session=x", "--shuffle=true", "b"}, []string{"a", "b"}, []string{"--session=x", "--shuffle=true"}},
		{[]string{"a", "--session", "s1", "--shuffle"}, []string{"a"}, []string{"--session", "s1", "--shuffle"}},
	}
	for _, c := range cases {
		ids, flags := splitPlayItemsArgs(c.args)
		if !slices.Equal(ids, c.ids) || !slices.Equal(flags, c.flags) {
			t.Errorf("%v: ids %v flags %v, want %v %v", c.args, ids, flags, c.ids, c.flags)
		}
	}
}

func TestParseOnOff(t *testing.T) {
	for in, want := range map[string]bool{"on": true, "ON": true, "true": true, "1": true, "off": false, "no": false, "0": false} {
		if got, err := parseOnOff(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "maybe", "2"} {
		if _, err := parseOnOff(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestContextLine(t *testing.T) {
	if got := contextLine(&model.QueueContext{Type: "playlist", ID: "x", Name: "Road Trip"}); got != "Playing from: Road Trip (playlist)" {
		t.Error(got)
	}
	if got := contextLine(&model.QueueContext{Type: "album", ID: "a1", Shuffled: true}); got != "Playing from: a1 (album, shuffled)" {
		t.Error(got)
	}
}
