package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"app/core"

	"github.com/spf13/pflag"
)

// reportInputFlags names the flags whose files --report-file must never replace.
var reportInputFlags = []string{"config", "retry-report", "state-file"}

// reportInputSuffixes adds files derived from an input flag's path.
var reportInputSuffixes = map[string][]string{"state-file": {".lock"}}

// validateReportFile rejects a --report-file that would overwrite an input
// file, and checks that its directory is writable before any upload starts.
func validateReportFile(f *pflag.FlagSet) error {
	output, _ := f.GetString("report-file")
	if output == "" {
		return nil
	}
	for _, name := range reportInputFlags {
		input, _ := f.GetString(name)
		if input == "" {
			continue
		}
		for _, suffix := range append([]string{""}, reportInputSuffixes[name]...) {
			if samePath(output, input+suffix) {
				return fmt.Errorf("--report-file must not overwrite --%s or its files", name)
			}
		}
	}
	// The config in use may come from a default location rather than --config.
	if core.ConfigPath != "" && samePath(output, core.ConfigPath) {
		return fmt.Errorf("--report-file must not overwrite the config file %s", core.ConfigPath)
	}
	probe, err := os.CreateTemp(filepath.Dir(output), ".gotohp-report-*")
	if err != nil {
		return fmt.Errorf("--report-file directory is not writable: %w", err)
	}
	_ = probe.Close()
	return os.Remove(probe.Name())
}

// samePath reports whether two paths name the same file, either lexically or
// through a link to an existing file.
func samePath(a, b string) bool {
	aAbs, aErr := filepath.Abs(a)
	bAbs, bErr := filepath.Abs(b)
	if aErr == nil && bErr == nil && aAbs == bAbs {
		return true
	}
	aInfo, aErr := os.Stat(a)
	bInfo, bErr := os.Stat(b)
	return aErr == nil && bErr == nil && os.SameFile(aInfo, bInfo)
}

func writeUploadReport(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".gotohp-report-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
