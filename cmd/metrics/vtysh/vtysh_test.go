// SPDX-License-Identifier:Apache-2.0

package vtysh

import "testing"

func TestRun(t *testing.T) {
	// run passes args to "<path> -c", so with /bin/sh each args is a script
	// that fakes vtysh.
	tests := []struct {
		desc        string
		args        string
		expectedOut string
		expectedErr string
	}{
		{
			desc:        "stderr warning on success is not returned",
			args:        `echo "% Can't open configuration file" >&2; echo '{}'`,
			expectedOut: "{}\n",
		},
		{
			desc:        "failure reports stderr in preference to stdout",
			args:        `echo partial; echo "bfdd is not running" >&2; exit 1`,
			expectedOut: "partial\n",
			expectedErr: `vtysh -c "echo partial; echo \"bfdd is not running\" >&2; exit 1": exit status 1: bfdd is not running`,
		},
		{
			desc:        "failure reports stdout when stderr is blank",
			args:        `echo "% Unknown command"; echo " " >&2; exit 1`,
			expectedOut: "% Unknown command\n",
			expectedErr: `vtysh -c "echo \"% Unknown command\"; echo \" \" >&2; exit 1": exit status 1: % Unknown command`,
		},
		{
			desc:        "failure without output",
			args:        "exit 1",
			expectedErr: `vtysh -c "exit 1": exit status 1`,
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			out, err := run("/bin/sh", test.args)
			if out != test.expectedOut {
				t.Errorf("output %q different from expected %q", out, test.expectedOut)
			}
			errStr := ""
			if err != nil {
				errStr = err.Error()
			}
			if errStr != test.expectedErr {
				t.Errorf("error %q different from expected %q", errStr, test.expectedErr)
			}
		})
	}
}
