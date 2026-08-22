package tools

import "testing"

func TestResolveScoped_RelativeStaysInside(t *testing.T) {
	cwd := "/task/workdir"
	got, err := resolveScoped(cwd, "sub/file.tf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "/task/workdir/sub/file.tf"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveScoped_AbsoluteInsideAllowed(t *testing.T) {
	cwd := "/task/workdir"
	got, err := resolveScoped(cwd, "/task/workdir/sub/file.tf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "/task/workdir/sub/file.tf"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveScoped_AbsoluteOutsideRejected(t *testing.T) {
	cwd := "/task/workdir"
	if _, err := resolveScoped(cwd, "/etc/passwd"); err == nil {
		t.Fatal("expected error for absolute path outside cwd")
	}
}

func TestResolveScoped_DotDotTraversalRejected(t *testing.T) {
	cwd := "/task/workdir"
	if _, err := resolveScoped(cwd, "../../etc/passwd"); err == nil {
		t.Fatal("expected error for relative traversal outside cwd")
	}
}

func TestResolveScoped_SiblingPrefixRejected(t *testing.T) {
	// "/task/workdir-evil" shares a string prefix with "/task/workdir" but is
	// a different directory entirely — must not be treated as inside scope.
	cwd := "/task/workdir"
	if _, err := resolveScoped(cwd, "/task/workdir-evil/file.tf"); err == nil {
		t.Fatal("expected error for sibling directory sharing a name prefix")
	}
}

func TestWithinScope_SiblingPrefixRejected(t *testing.T) {
	if withinScope("/task/workdir", "/task/workdir-evil/file.tf") {
		t.Fatal("expected sibling directory to be reported outside scope")
	}
}
