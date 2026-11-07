package model

import "testing"

func TestShuffleOnlyForPlayItems(t *testing.T) {
	yes, no := Ptr(true), Ptr(false)
	ok := []Command{
		{Name: CmdPlayItems, ItemIDs: []string{"a1"}, Shuffle: yes},
		{Name: CmdPlayItems, ItemIDs: []string{"a1"}, Shuffle: no, Mode: PlayModeLast},
		{Name: CmdPlayItems, ItemIDs: []string{"a1"}},
	}
	for _, c := range ok {
		if err := c.Validate(); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	bad := []Command{
		{Name: CmdPause, Shuffle: yes},
		{Name: CmdPlay, Shuffle: no},
		{Name: CmdNext, Shuffle: yes},
		{Name: CmdQueueJump, Index: Ptr(1), Shuffle: yes},
		{Name: CmdSeek, PositionMS: Ptr[int64](1), Shuffle: yes},
		{Name: CmdSetVolume, Volume: Ptr(0.5), Shuffle: yes},
	}
	for _, c := range bad {
		if err := c.Validate(); CodeOf(err) != ErrInvalidArgument {
			t.Errorf("%s with shuffle: want invalid_argument, got %v", c.Name, err)
		}
	}
}

func TestPlayItemsRequiredCapability(t *testing.T) {
	cases := []struct {
		shuffle *bool
		want    Capability
	}{{nil, CapPlayItems}, {Ptr(false), CapPlayItems}, {Ptr(true), CapPlayItemsShuffle}}
	for _, c := range cases {
		got, _ := Command{Name: CmdPlayItems, ItemIDs: []string{"a1"}, Shuffle: c.shuffle}.RequiredCapability()
		if got != c.want {
			t.Errorf("shuffle %v: %s, want %s", c.shuffle, got, c.want)
		}
	}
	// The new capability is part of the canonical lists.
	if s := SortCapabilities([]Capability{CapPlayItemsShuffle, CapPlay}); len(s) != 2 || s[1] != CapPlayItemsShuffle {
		t.Errorf("sort dropped or misplaced play_items_shuffle: %v", s)
	}
}
