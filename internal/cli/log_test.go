package cli

import "testing"

func TestLog(t *testing.T) {
	e := newEnv(t)
	e.back.logs = map[string]string{
		"":        "2026/01/02 03:04:05 gst test daemon ready\n",
		"myphone": "2026/01/02 03:04:06 link 0123456789ab: up\n",
	}
	e.mustGst("up")
	if out := e.mustGst("log"); out != e.back.logs[""] {
		t.Fatalf("log %q", out)
	}
	if out := e.mustGst("log", "MyPhone.gst"); out != e.back.logs["myphone"] {
		t.Fatalf("log of myphone %q", out)
	}
	e.expectError(exitFailure, []string{"log", "far"}, "far is offline", "hint: see the machines with `gst status`")
	e.expectError(exitUsage, []string{"log", "a", "b"}, "usage:")
}
