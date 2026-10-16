package cli

import (
	"fmt"
	"os"

	"github.com/spf13/pflag"
	"path/filepath"
)

func validateReportPaths(f *pflag.FlagSet) error {
	output, _ := f.GetString("report-file")
	if output == "" {
		return nil
	}
	for _, name := range []string{"state-file", "retry-report", "config"} {
		input, _ := f.GetString(name)
		if input == "" {
			continue
		}
		paths := []string{input}
		if name == "state-file" {
			paths = append(paths, input+".lock")
		}
		for _, path := range paths {
			outAbs, _ := filepath.Abs(output)
			inAbs, _ := filepath.Abs(path)
			same := outAbs == inAbs
			outInfo, outErr := os.Stat(output)
			inInfo, inErr := os.Stat(path)
			if outErr == nil && inErr == nil {
				same = same || os.SameFile(outInfo, inInfo)
			}
			if same {
				return fmt.Errorf("--report-file must not overwrite --%s or its lock", name)
			}
		}
	}
	return nil
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
