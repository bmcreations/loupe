package target

import "testing"

func TestSniffMedia(t *testing.T) {
	cases := []struct {
		head  string
		ext   string
		video bool
		ok    bool
	}{
		{"\x89PNG\r\n\x1a\n....", "png", false, true},
		{"\xff\xd8\xff\xe0........", "jpg", false, true},
		{"GIF89a......", "gif", false, true},
		{"RIFF\x00\x00\x00\x00WEBP", "webp", false, true},
		{"\x00\x00\x00\x18ftypheic", "heic", false, true},
		{"\x00\x00\x00\x14ftypqt  ", "mov", true, true},
		{"\x00\x00\x00\x20ftypisom", "mp4", true, true},
		{"PK\x03\x04 an apk", "", false, false},
		{"", "", false, false},
	}
	for _, c := range cases {
		k, ok := SniffMedia([]byte(c.head))
		if ok != c.ok || k.Ext != c.ext || k.Video != c.video {
			t.Errorf("SniffMedia(%q) = %+v, %v; want %s video=%v ok=%v", c.head, k, ok, c.ext, c.video, c.ok)
		}
	}
}
