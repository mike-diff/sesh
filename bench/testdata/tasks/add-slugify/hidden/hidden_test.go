package slug

import "testing"

func TestSlugifyHidden(t *testing.T) {
	cases := map[string]string{
		"Hello, World!":       "hello-world",
		"  --Go 1.22 rocks--": "go-1-22-rocks",
		"already-a-slug":      "already-a-slug",
		"Ünïcode & more":      "n-code-more",
		"":                    "",
		"!!!":                 "",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
