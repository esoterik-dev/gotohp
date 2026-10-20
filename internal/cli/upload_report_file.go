package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"app/core"

	"github.com/spf13/pflag"
)

// reportInputFlags names the flags whose files --report-file must never replace.
var reportInputFlags = []string{"config", "retry-report", "state-file"}

// reportInputSuffixes adds files derived from an input flag's path.
var reportInputSuffixes = map[string][]string{"state-file": {".lock"}}

// validateReportFile rejects a --report-file that would overwrite an input
// file or names a directory, and checks that its directory is writable before
// any upload starts.
func validateReportFile(f *pflag.FlagSet) error {
	output, _ := f.GetString("report-file")
	return checkReportFile(output, protectedInputs(f))
}

// protectedInputs maps each input flag that is set to the files it names.
func protectedInputs(f *pflag.FlagSet) map[string][]string {
	inputs := make(map[string][]string)
	for _, name := range reportInputFlags {
		input, _ := f.GetString(name)
		if input == "" {
			continue
		}
		for _, suffix := range append([]string{""}, reportInputSuffixes[name]...) {
			inputs["--"+name+" or its files"] = append(inputs["--"+name+" or its files"], input+suffix)
		}
	}
	return inputs
}

// validateLogFile rejects a --log-file that would overwrite an input file,
// the report, an upload path or a file inside one, or that names a directory,
// before any upload starts. Like --report-file, any other file may be
// replaced.
func validateLogFile(f *pflag.FlagSet, paths []string) error {
	output, _ := f.GetString("log-file")
	if output == "" {
		return nil
	}
	inputs := protectedInputs(f)
	if report, _ := f.GetString("report-file"); report != "" {
		inputs["--report-file"] = []string{report}
	}
	inputs["an upload path"] = paths
	if err := checkOutputFile("--log-file", output, inputs); err != nil {
		return err
	}
	if _, err := os.Stat(output); err != nil {
		return nil // a new file cannot be one of the files to upload
	}
	for _, path := range paths {
		if info, err := os.Stat(path); err == nil && info.IsDir() && insideDir(output, path) {
			return fmt.Errorf("--log-file must not overwrite a file in the upload path %s", path)
		}
	}
	return nil
}

// checkReportFile is validateReportFile for any command: inputs maps a
// description used in errors to files the report must never replace. The
// config in use is always protected, so load it before calling this.
func checkReportFile(output string, inputs map[string][]string) error {
	if output == "" {
		return nil
	}
	if err := checkOutputFile("--report-file", output, inputs); err != nil {
		return err
	}
	probe, err := os.CreateTemp(filepath.Dir(output), ".gotohp-report-*")
	if err != nil {
		return fmt.Errorf("--report-file directory is not writable: %w", err)
	}
	_ = probe.Close()
	return os.Remove(probe.Name())
}

// checkOutputFile rejects an output file given by flag that is one of inputs
// or the config in use, or that is a directory.
func checkOutputFile(flag, output string, inputs map[string][]string) error {
	for name, paths := range inputs {
		for _, input := range paths {
			if samePath(output, input) {
				return fmt.Errorf("%s must not overwrite %s", flag, name)
			}
		}
	}
	// The config in use may come from a default location rather than --config.
	if core.ConfigPath != "" && samePath(output, core.ConfigPath) {
		return fmt.Errorf("%s must not overwrite the config file %s", flag, core.ConfigPath)
	}
	if info, err := os.Stat(output); err == nil && info.IsDir() {
		return fmt.Errorf("%s %s is a directory; name a file", flag, output)
	}
	return nil
}

// insideDir reports whether path lies under dir, after resolving links.
func insideDir(path, dir string) bool {
	resolve := func(p string) string {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		if real, err := filepath.EvalSymlinks(p); err == nil {
			p = real
		}
		return p
	}
	rel, err := filepath.Rel(resolve(dir), resolve(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
