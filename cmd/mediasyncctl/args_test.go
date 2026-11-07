package main

import (
	"slices"
	"testing"
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
