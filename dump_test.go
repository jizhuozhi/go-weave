package weave

import (
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// preciseStubsFile holds the precise trampolines the tests need, exactly as
// StubSource emits them for this architecture.
const preciseStubsFile = "stubs_precise_arm64_test.go"

// TestGeneratedStubsUpToDate is the golden test for the generator: the checked
// in trampolines must be byte for byte what StubSource produces today, so a
// change to the layout code or to the emitter cannot silently leave them stale.
//
// Regenerate with WEAVE_REGEN=1 go test -run TestGeneratedStubsUpToDate.
func TestGeneratedStubsUpToDate(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skipf("%s stubs are not checked in; generate them with StubSource", runtime.GOARCH)
	}
	want := stubSource("weave", "", "", reflect.TypeOf((*StackPtrs)(nil)).Elem())

	if os.Getenv("WEAVE_REGEN") != "" {
		if err := os.WriteFile(preciseStubsFile, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("regenerated " + preciseStubsFile)
		return
	}

	got, err := os.ReadFile(preciseStubsFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s is stale; rerun with WEAVE_REGEN=1", preciseStubsFile)
	}
	// The emitter must produce gofmt clean source: generated files land in
	// the user's repository, where a formatting check would flag them.
	if strings.Contains(want, "\t\n") || strings.HasSuffix(want, "\n\n") {
		t.Error("generated source is not gofmt clean")
	}
}
