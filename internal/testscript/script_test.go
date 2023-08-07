package testscript_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

func Test_Script(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir: "testdata",
		Setup: func(e *testscript.Env) error {
			ns := filepath.Base(e.WorkDir)
			e.Defer(func() {
				// Delete the namespace after the test.
				err := exec.Command("kubectl", "delete", "namespace", ns).Run()
				if err != nil {
					t.Fatal(err)
				}
			})
			err := exec.Command("kubectl", "create", "namespace", ns).Run()
			if err != nil {
				t.Fatal(err)
			}
			e.Setenv("ns", ns)
			e.Setenv("HOME", os.Getenv("HOME"))
			return nil
		},
		Cmds: map[string]func(ts *testscript.TestScript, neg bool, args []string){
			"kubectl": kubectl,
		},
	})
}

func kubectl(ts *testscript.TestScript, neg bool, args []string) {
	r := strings.NewReplacer("$ns", ts.Getenv("ns"))
	for i, arg := range args {
		args[i] = r.Replace(arg)
	}
	switch l := len(args); {
	// If command was run with a unix redirect-like suffix.
	case l > 1 && args[l-2] == ">":
		var (
			workDir = ts.Getenv("WORK")
			f, err  = os.Create(filepath.Join(workDir, args[l-1]))
		)
		ts.Check(err)
		defer f.Close()
		stderr := &bytes.Buffer{}
		cmd := exec.Command("kubectl", args[0:l-2]...)
		cmd.Stderr = stderr
		cmd.Stdout = f
		cmd.Env = os.Environ()
		cmd.Dir = workDir
		if err := cmd.Run(); err != nil && !neg {
			ts.Fatalf("\n[stderr]\n%s", stderr)
		}
	default:
		err := ts.Exec("kubectl", args...)
		if !neg {
			ts.Check(err)
		}
		if neg && err == nil {
			ts.Fatalf("expected fail")
		}
	}
}
