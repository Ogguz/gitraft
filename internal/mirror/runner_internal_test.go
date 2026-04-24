package mirror

import "testing"

func TestLastLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"empty", "", 3, ""},
		{"single line", "one line", 3, "one line"},
		{"fewer than n", "a\nb", 3, "a | b"},
		{"exactly n", "a\nb\nc", 3, "a | b | c"},
		{"more than n", "a\nb\nc\nd", 3, "b | c | d"},
		{"trailing newline", "a\nb\n", 3, "a | b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := lastLines(tc.in, tc.n); got != tc.want {
				t.Errorf("lastLines(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
		})
	}
}
