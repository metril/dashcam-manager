package media

import (
	"strings"
	"testing"
)

func TestThumbPathShards(t *testing.T) {
	got := ThumbPath("/cache", 1234)
	want := "/cache/thumbs/234/1234.jpg"
	if got != want {
		t.Errorf("ThumbPath = %q, want %q", got, want)
	}
}

func TestSpritePathsShards(t *testing.T) {
	jpg, vtt := SpritePaths("/cache", 5)
	if jpg != "/cache/sprites/5/5.jpg" {
		t.Errorf("jpg = %q", jpg)
	}
	if vtt != "/cache/sprites/5/5.vtt" {
		t.Errorf("vtt = %q", vtt)
	}
	if !strings.HasSuffix(jpg, ".jpg") || !strings.HasSuffix(vtt, ".vtt") {
		t.Errorf("unexpected extensions: %q %q", jpg, vtt)
	}
}
