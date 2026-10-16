package infra

import (
	"reflect"
	"strings"
	"testing"
)

func TestHostPortsReadsRanges(t *testing.T) {
	got := hostPorts("0.0.0.0:9000-9001->9000-9001/tcp, [::]:9000-9001->9000-9001/tcp, 0.0.0.0:5432->5432/tcp, 8080/tcp")
	want := []int{9000, 9001, 9000, 9001, 5432}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hostPorts = %v, want %v", got, want)
	}
}

// A first `up` prints hundreds of pull-progress lines; the error after them
// must survive the condensing.
func TestTrimKeepsTheErrorUnderPullNoise(t *testing.T) {
	var b strings.Builder
	b.WriteString(" postgres Pulling \n")
	for i := 0; i < 300; i++ {
		b.WriteString(" 4f4fb700ef54 Downloading [=====>    ]  1.2MB/3MB\n 4f4fb700ef54 Pull complete \n")
	}
	b.WriteString(" Container fride-shared-postgres-1  Creating\n")
	b.WriteString("Error response from daemon: driver failed programming external connectivity: Bind for 0.0.0.0:5432 failed: port is already allocated\n")
	got := trim([]byte(b.String()))
	if !strings.Contains(got, "port is already allocated") {
		t.Fatalf("the error was lost: %q", got)
	}
	if strings.Contains(got, "Downloading") {
		t.Fatalf("progress noise kept: %q", got)
	}
}
