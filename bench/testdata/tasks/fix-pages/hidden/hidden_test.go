package pages

import "testing"

func TestCountHidden(t *testing.T) {
	cases := []struct{ total, size, want int }{{29, 10, 3}, {30, 10, 3}, {5, 0, 0}, {1000, 1, 1000}}
	for _, c := range cases {
		if got := Count(c.total, c.size); got != c.want {
			t.Errorf("Count(%d, %d) = %d, want %d", c.total, c.size, got, c.want)
		}
	}
}
