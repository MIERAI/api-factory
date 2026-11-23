package perf

import "testing"

func TestParseSkew(t *testing.T) {
	if p, err := ParseSkew("uniform"); err != nil || p != nil {
		t.Fatalf("uniform: %v %v", p != nil, err)
	}
	hot, err := ParseSkew("hot:3")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 1000 {
		if k := hot(100, i); k < 0 || k >= 3 {
			t.Fatalf("hot:3 picked %d", k)
		}
	}
	z, err := ParseSkew("zipf:1.3")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[int]int{}
	for i := range 10000 {
		k := z(1000, i)
		if k < 0 || k >= 1000 {
			t.Fatalf("zipf picked %d", k)
		}
		counts[k]++
	}
	if counts[0] < 2000 || len(counts) < 50 { // a hot head and a long tail
		t.Fatalf("zipf:1.3 head %d, distinct %d", counts[0], len(counts))
	}
	for _, bad := range []string{"hot:0", "zipf:1", "zipf:x", "pareto"} {
		if _, err := ParseSkew(bad); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
}
