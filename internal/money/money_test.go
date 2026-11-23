package money

import "testing"

func TestParseAndString(t *testing.T) {
	cases := map[string]Paise{"100": 10000, "100.5": 10050, "100.50": 10050, "0.01": 1, "0": 0}
	for in, want := range cases {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Fatalf("Parse(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "-1", "1.234", "1.", "abc", "+5", "1.-5"} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("Parse(%q) should fail", bad)
		}
	}
	if s := Paise(10050).String(); s != "100.50" {
		t.Fatalf("String = %s", s)
	}
	if s := Paise(-5).String(); s != "-0.05" {
		t.Fatalf("String = %s", s)
	}
}
