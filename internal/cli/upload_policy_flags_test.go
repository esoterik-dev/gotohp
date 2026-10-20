package cli

import (
	"reflect"
	"testing"

	"app/core"
)

func TestRequestPolicyFlags(t *testing.T) {
	cmd := newUploadCommand()
	if err := cmd.Flags().Parse([]string{"--retry-429=false", "--max-retries=0", "--requests-per-minute=120"}); err != nil {
		t.Fatal(err)
	}
	got, err := requestPolicyFromFlags(cmd.Flags())
	if err != nil {
		t.Fatal(err)
	}
	want, _ := core.NewRequestPolicy(0, false, 120)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("policy=%+v want %+v", got, want)
	}

	defaults, err := requestPolicyFromFlags(newUploadCommand().Flags())
	if err != nil {
		t.Fatal(err)
	}
	want, _ = core.NewRequestPolicy(core.DefaultMaxRetries, true, core.DefaultRequestsPerMinute)
	if !reflect.DeepEqual(defaults, want) {
		t.Fatalf("default policy=%+v want %+v", defaults, want)
	}

	for _, flag := range []string{"--max-retries=-1", "--requests-per-minute=-1"} {
		cmd := newUploadCommand()
		if err := cmd.Flags().Parse([]string{flag}); err != nil {
			t.Fatal(err)
		}
		if _, err := requestPolicyFromFlags(cmd.Flags()); err == nil {
			t.Fatalf("accepted %s", flag)
		}
	}
}
