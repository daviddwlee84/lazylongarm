package colab

import "testing"

func TestParseUsage(t *testing.T) {
	u, err := ParseUsage("Current balance: 571.68 compute units\nUsage rate: 0.08/hr\nActive assignments: 1\n")
	if err != nil {
		t.Fatal(err)
	}
	if u.Balance != 571.68 || u.RatePerHour != 0.08 || u.Active != 1 {
		t.Fatalf("%+v", u)
	}
	if _, err := ParseUsage("Please sign in"); err == nil {
		t.Fatal("garbage accepted")
	}
	u, err = ParseUsage("\x1b[1mCurrent balance:\x1b[0m 0 compute units")
	if err != nil || u.Balance != 0 {
		t.Fatalf("%+v %v", u, err)
	}
}

func TestParseSessions(t *testing.T) {
	out := "[colab] Listing...\n[lw] m-s-example-abc | Hardware: CPU | Shape: Standard | Variant: DEFAULT\n[gpu-1] m-s-def | Hardware: T4 | Shape: High-RAM | Variant: GPU\nnoise\n"
	got := ParseSessions(out)
	if len(got) != 2 {
		t.Fatalf("CLI chatter parsed as a session: %+v", got)
	}
	s, ok := FindSession(got, "lw")
	if !ok || s.ID != "m-s-example-abc" || s.Hardware != "CPU" || s.Shape != "Standard" {
		t.Fatalf("%+v", got)
	}
	if s, ok := FindSession(got, "gpu-1"); !ok || s.Hardware != "T4" || s.Shape != "High-RAM" {
		t.Fatalf("%+v", s)
	}
	if _, ok := FindSession(got, "nope"); ok {
		t.Fatal("found a missing session")
	}
}

func TestResult(t *testing.T) {
	if r, ok := Result("noise\n  __LAZYLONGARM_OK__ x86_64 abc\nmore"); !ok || r != "x86_64 abc" {
		t.Fatalf("%q %v", r, ok)
	}
	if _, ok := Result("Traceback ...\nValueError"); ok {
		t.Fatal("failure reported as success")
	}
}
