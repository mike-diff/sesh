package pages

import "testing"

func TestCount(t *testing.T) {
	cases := []struct{ total, size, want int }{{0, 10, 0}, {10, 10, 1}, {11, 10, 2}, {1, 10, 1}}
	for _, c := range cases {
		if got := Count(c.total, c.size); got != c.want {
			t.Errorf("Count(%d, %d) = %d, want %d", c.total, c.size, got, c.want)
		}
	}
}
