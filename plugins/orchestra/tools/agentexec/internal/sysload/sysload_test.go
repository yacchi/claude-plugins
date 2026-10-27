package sysload

import (
	"runtime"
	"strings"
	"testing"
)

func TestMemoryPressureLevels(t *testing.T) {
	for _, c := range []struct {
		name         string
		psi, meminfo []byte
		want         string
	}{
		{"nothing readable", nil, nil, Unknown},
		{"garbage", []byte("nope\n"), []byte("junk"), Unknown},
		{"psi calm", []byte("some avg10=3.10 avg60=0.00 avg300=0.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"), nil, Normal},
		{"psi high", []byte("some avg10=25.00 avg60=0.00 avg300=0.00 total=1\n"), nil, Warn},
		{"psi critical", []byte("some avg10=60.00\n"), nil, Critical},
		{"psi wins over meminfo", []byte("some avg10=0.00\n"), []byte("MemTotal: 1000 kB\nMemAvailable: 10 kB\n"), Normal},
		{"meminfo low", nil, []byte("MemTotal: 1000 kB\nMemFree: 5 kB\nMemAvailable: 50 kB\n"), Warn},
		{"meminfo critical", nil, []byte("MemTotal: 1000 kB\nMemAvailable: 40 kB\n"), Critical},
		{"meminfo fine", nil, []byte("MemTotal: 1000 kB\nMemAvailable: 500 kB\n"), Normal},
		{"meminfo without total", nil, []byte("MemAvailable: 1 kB\n"), Unknown},
	} {
		if got := linuxPressure(c.psi, c.meminfo); got.Level != c.want || got.High() != (got.Reason != "") {
			t.Errorf("linux %s: got %+v want %s", c.name, got, c.want)
		}
	}
	for _, c := range []struct {
		level   uint32
		freePct int
		want    string
	}{
		{1, -1, Normal},
		{2, -1, Warn},
		{2, 24, Warn},
		{2, 25, Normal},
		{2, 48, Normal},
		{4, 90, Critical},
		{4, -1, Critical},
	} {
		if got := darwinPressure(c.level, c.freePct); got.Level != c.want {
			t.Errorf("darwin level %d free %d: got %+v want %s", c.level, c.freePct, got, c.want)
		}
	}
	if got := darwinPressure(2, 24); !strings.Contains(got.Reason, "24% free") {
		t.Errorf("darwin warn reason %q lacks free percent", got.Reason)
	}
}

func TestLoadIsRead(t *testing.T) {
	if l1, l5, ok := parseLoadavg("0.52 1.25 0.90 1/300 4242\n"); !ok || l1 != 0.52 || l5 != 1.25 {
		t.Fatalf("parseLoadavg = %v %v %v", l1, l5, ok)
	}
	if _, _, ok := parseLoadavg("x\n"); ok {
		t.Fatal("garbage loadavg read")
	}
	if runtime.GOOS != "darwin" {
		return
	}
	if l1, l5, ok := Load(); !ok || l1 <= 0 || l1 > 10000 || l5 <= 0 {
		t.Fatalf("Load = %v %v %v", l1, l5, ok)
	}
}
