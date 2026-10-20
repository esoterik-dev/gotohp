package cli

import "testing"

func TestSeparateWorkerFlags(t *testing.T) {
	opts, _ := parsedUploadOptions(t, "photo.jpg", "--threads=8", "--hash-threads=6", "--upload-threads=2")
	if opts.Threads != 8 || opts.HashThreads != 6 || opts.UploadThreads != 2 {
		t.Fatalf("%+v", opts)
	}
	for _, flag := range []string{"--hash-threads=0", "--upload-threads=-1"} {
		cmd := newUploadCommand()
		if err := cmd.Flags().Parse([]string{flag}); err != nil {
			t.Fatal(err)
		}
		if err := validateUploadOptions(cmd.Flags(), uploadOptionsFromFlags(cmd.Flags())); err == nil {
			t.Fatalf("accepted %s", flag)
		}
	}
}
