package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func FuzzUntrustedCLIArguments(f *testing.F) {
	for _, seed := range []struct {
		shape uint8
		value string
	}{
		{0, "bad-flag"}, {1, "operand"}, {2, "true"}, {2, "FALSE"},
		{2, "invalid"}, {3, "0.0.0.0:0"}, {4, ""}, {5, "--help"}, {1, "文字"},
	} {
		f.Add(seed.shape, seed.value)
	}
	absent := filepath.Join(f.TempDir(), "no-existing-database.db")
	f.Cleanup(func() {
		if _, err := os.Stat(absent); !os.IsNotExist(err) {
			f.Errorf("parse-only trials changed the temporary database path: %v", err)
		}
	})
	f.Fuzz(func(t *testing.T, shape uint8, value string) {
		if len(value) > 1<<14 {
			value = value[:1<<14]
		}
		args := []string{"serve", "-db", absent}
		want := ""
		switch shape % 6 {
		case 0:
			args = append(args, "-fixture-"+value)
			want = "flag provided but not defined"
		case 1:
			args = append(args, "--", value)
			want = "positional arguments are unsupported"
		case 2:
			args = append(args, "-password-stdin="+value)
			switch value {
			case "1", "t", "T", "true", "TRUE", "True", "0", "f", "F", "false", "FALSE", "False":
				want = "open initialized evidence database"
			default:
				want = "invalid boolean value"
			}
		case 3:
			args = append(args, "-listen="+value)
			want = "open initialized evidence database"
		case 4:
			args = append(args, "-listen")
			want = "flag needs an argument"
		case 5:
			args = append(args, "-help", value)
		}
		err := run(args, strings.NewReader(""), io.Discard)
		if want == "" {
			if err != nil {
				t.Fatalf("help should exit before operator activity: %v", err)
			}
			return
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("expected %q for option shape %d, got %v", want, shape%6, err)
		}
	})
}
