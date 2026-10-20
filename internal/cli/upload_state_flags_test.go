package cli

import "testing"

func TestUploadStateFlags(t *testing.T) {
	opts, _ := parsedUploadOptions(t, "photo.jpg", "--state-file=state.json", "--update-state", "--revalidate-state")
	if opts.StateFile != "state.json" || !opts.UpdateState || !opts.RevalidateState {
		t.Fatalf("%+v", opts)
	}
	for _, flag := range []string{"--update-state", "--revalidate-state"} {
		cmd := newUploadCommand()
		if err := cmd.Flags().Parse([]string{flag}); err != nil {
			t.Fatal(err)
		}
		if err := validateUploadOptions(cmd.Flags(), uploadOptionsFromFlags(cmd.Flags())); err == nil {
			t.Fatalf("accepted %s without state file", flag)
		}
	}
}
