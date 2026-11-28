package cli

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

type flagHelpError struct {
	name     string
	defaults string
}

func (e flagHelpError) Error() string { return flag.ErrHelp.Error() }
func (e flagHelpError) Unwrap() error { return flag.ErrHelp }

func newFlagHelpError(fs *flag.FlagSet) error {
	var buf bytes.Buffer
	fs.SetOutput(&buf)
	fs.PrintDefaults()
	return flagHelpError{name: fs.Name(), defaults: buf.String()}
}

// parseFlagsInterspersed parses flags anywhere among positional arguments, matching Python argparse.
func parseFlagsInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	isBool := func(name string) (known bool, b bool) {
		f := fs.Lookup(name)
		if f == nil {
			return false, false
		}
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			return true, true
		}
		return true, false
	}
	var flagArgs, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			trimmed := strings.TrimPrefix(strings.TrimPrefix(a, "--"), "-")
			if trimmed == "h" || trimmed == "help" {
				return nil, newFlagHelpError(fs)
			}
			if strings.Contains(trimmed, "=") {
				flagArgs = append(flagArgs, a)
				continue
			}
			known, b := isBool(trimmed)
			if !known {
				return nil, fmt.Errorf("unknown flag: %s", a)
			}
			flagArgs = append(flagArgs, a)
			if !b {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("flag needs an argument: %s", a)
				}
				i++
				flagArgs = append(flagArgs, args[i])
			}
			continue
		}
		pos = append(pos, a)
	}
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, newFlagHelpError(fs)
		}
		return nil, err
	}
	return pos, nil
}

func parseFlagsNoPositional(name string, args []string) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(pos, " "))
	}
	return nil
}
