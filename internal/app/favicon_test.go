package app

import (
	"strings"
	"testing"
)

func TestLocalFaviconUsesPetrolCircleIcon(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	index := getCharacterization(t, h, "/")
	body := index.Body.String()
	for _, want := range []string{
		`<link rel="icon" type="image/svg+xml" href='data:image/svg+xml,`,
		`viewBox="0 0 512 512"`, `%230E7C86`, `<circle cx="256" cy="256"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("local favicon missing %q", want)
		}
	}
	if strings.Contains(body, `M18 12v40M32 12v40M46 12v40`) {
		t.Fatal("local index still carries the old multicolor sliders favicon")
	}
}
