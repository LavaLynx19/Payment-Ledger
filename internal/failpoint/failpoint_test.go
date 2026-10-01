package failpoint

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		spec    string
		wantErr bool
		want    string
	}{
		{"", false, "none"},
		{"accept.after_commit", false, "accept.after_commit=1"},
		{"capture.after_entries=0.25, purge.mid_batch", false, "capture.after_entries=0.25,purge.mid_batch=1"},
		{"accept.after_comit", true, ""},
		{"accept.after_commit=0", true, ""},
		{"accept.after_commit=1.5", true, ""},
		{"accept.after_commit=x", true, ""},
	}
	for _, c := range cases {
		s, err := Parse(c.spec)
		if (err != nil) != c.wantErr {
			t.Errorf("Parse(%q) err = %v, wantErr %v", c.spec, err, c.wantErr)
			continue
		}
		if err == nil && s.String() != c.want {
			t.Errorf("Parse(%q) = %s, want %s", c.spec, s, c.want)
		}
	}
}

func TestInject(t *testing.T) {
	var nilSet *Set
	nilSet.Inject("accept.after_commit") // must not panic

	s, err := Parse("accept.after_commit")
	if err != nil {
		t.Fatal(err)
	}
	var fired []string
	s.WithCrash(func(name string) { fired = append(fired, name) })

	s.Inject("accept.after_commit")
	s.Inject("capture.after_claim") // not enabled
	if len(fired) != 1 || fired[0] != "accept.after_commit" {
		t.Errorf("fired = %v, want [accept.after_commit]", fired)
	}
}
