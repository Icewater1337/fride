package infra

import (
	"reflect"
	"regexp"
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

func TestProjectNamesNeverCollide(t *testing.T) {
	const home = "/Users/someone/.fride"
	names := map[string]string{}
	add := func(what, name string) {
		t.Helper()
		if prev, ok := names[name]; ok {
			t.Errorf("%s and %s share the compose project %q", prev, what, name)
		}
		names[name] = what
	}
	add("the shared project", SharedProject(home))
	// A task named "shared" must not be the shared project: its Down would stop
	// every task's shared containers.
	for _, slug := range []string{"shared", "v1.2-fix", "v1-2-fix", "PROJ-42", "proj-42", "-x-", "x"} {
		add("task "+slug, TaskProject(home, slug))
	}
	// A second FrIDE setup shares nothing with the first.
	add("the other setup's shared project", SharedProject("/tmp/throwaway"))
	add("the other setup's task shared", TaskProject("/tmp/throwaway", "shared"))
}

// Compose accepts only [a-z0-9][a-z0-9_-]*.
func TestProjectNamesAreValidForCompose(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	for _, slug := range []string{"PROJ-42", "v1.2-fix", ".hidden", "a_b", "--"} {
		for _, name := range []string{TaskProject("/x", slug), ProjectName(slug)} {
			if !valid.MatchString(name) {
				t.Errorf("%q → %q is not a valid compose project name", slug, name)
			}
		}
	}
	if got := ProjectName("proj-42"); got != "proj-42" {
		t.Errorf("a slug compose accepts as is was rewritten to %q", got)
	}
	// Stable: Down must find what Up started, across restarts.
	if TaskProject("/x", "v1.2-fix") != TaskProject("/x", "v1.2-fix") {
		t.Error("project names are not stable")
	}
}
