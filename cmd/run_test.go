/*
 *
 * k6 - a next-generation load testing tool
 * Copyright (C) 2020 Load Impact
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as
 * published by the Free Software Foundation, either version 3 of the
 * License, or (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.k6.io/k6/errext"
	"go.k6.io/k6/errext/exitcodes"
	"go.k6.io/k6/js/common"
	"go.k6.io/k6/lib/fsext"
	"go.k6.io/k6/lib/testutils"
)

type mockWriter struct {
	err      error
	errAfter int
}

func (fw mockWriter) Write(p []byte) (n int, err error) {
	if fw.err != nil {
		return fw.errAfter, fw.err
	}
	return len(p), nil
}

var _ io.Writer = mockWriter{}

func getFiles(t *testing.T, fs afero.Fs) map[string]*bytes.Buffer {
	result := map[string]*bytes.Buffer{}
	walkFn := func(filePath string, _ os.FileInfo, err error) error {
		if filePath == "/" || filePath == "\\" {
			return nil
		}
		require.NoError(t, err)
		contents, err := afero.ReadFile(fs, filePath)
		require.NoError(t, err)
		result[filePath] = bytes.NewBuffer(contents)
		return nil
	}

	err := fsext.Walk(fs, afero.FilePathSeparator, filepath.WalkFunc(walkFn))
	require.NoError(t, err)

	return result
}

func assertEqual(t *testing.T, exp string, actual io.Reader) {
	act, err := ioutil.ReadAll(actual)
	require.NoError(t, err)
	assert.Equal(t, []byte(exp), act)
}

func initVars() (
	content map[string]io.Reader, stdout *bytes.Buffer, stderr *bytes.Buffer, fs afero.Fs,
) {
	return map[string]io.Reader{}, bytes.NewBuffer([]byte{}), bytes.NewBuffer([]byte{}), afero.NewMemMapFs()
}

func TestHandleSummaryResultSimple(t *testing.T) {
	t.Parallel()
	content, stdout, stderr, fs := initVars()

	// Test noop
	assert.NoError(t, handleSummaryResult(fs, stdout, stderr, content))
	require.Empty(t, getFiles(t, fs))
	require.Empty(t, stdout.Bytes())
	require.Empty(t, stderr.Bytes())

	// Test stdout only
	content["stdout"] = bytes.NewBufferString("some stdout summary")
	assert.NoError(t, handleSummaryResult(fs, stdout, stderr, content))
	require.Empty(t, getFiles(t, fs))
	assertEqual(t, "some stdout summary", stdout)
	require.Empty(t, stderr.Bytes())
}

func TestHandleSummaryResultError(t *testing.T) {
	t.Parallel()
	content, _, stderr, fs := initVars()

	expErr := errors.New("test error")
	stdout := mockWriter{err: expErr, errAfter: 10}

	filePath1 := "/path/file1"
	filePath2 := "/path/file2"
	if runtime.GOOS == "windows" {
		filePath1 = "\\path\\file1"
		filePath2 = "\\path\\file2"
	}

	content["stdout"] = bytes.NewBufferString("some stdout summary")
	content["stderr"] = bytes.NewBufferString("some stderr summary")
	content[filePath1] = bytes.NewBufferString("file summary 1")
	content[filePath2] = bytes.NewBufferString("file summary 2")
	err := handleSummaryResult(fs, stdout, stderr, content)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), expErr.Error())
	files := getFiles(t, fs)
	assertEqual(t, "file summary 1", files[filePath1])
	assertEqual(t, "file summary 2", files[filePath2])
}

func TestAbortTest(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		testFilename, expLogOutput string
	}{
		{
			testFilename: "abort.js",
		},
		{
			testFilename: "abort_initerr.js",
		},
		{
			testFilename: "abort_initvu.js",
		},
		{
			testFilename: "abort_teardown.js",
			expLogOutput: "Calling teardown function after test.abort()",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.testFilename, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			logger := logrus.New()
			logger.SetLevel(logrus.InfoLevel)
			logger.Out = ioutil.Discard
			hook := testutils.SimpleLogrusHook{
				HookedLevels: []logrus.Level{logrus.InfoLevel},
			}
			logger.AddHook(&hook)

			cmd := getRunCmd(ctx, logger, newCommandFlags())
			a, err := filepath.Abs(path.Join("testdata", tc.testFilename))
			require.NoError(t, err)
			cmd.SetArgs([]string{a})
			err = cmd.Execute()
			var e errext.HasExitCode
			require.ErrorAs(t, err, &e)
			assert.Equalf(t, exitcodes.ScriptAborted, e.ExitCode(),
				"Status code must be %d", exitcodes.ScriptAborted)
			assert.Contains(t, e.Error(), common.AbortTest)

			if tc.expLogOutput != "" {
				var gotMsg bool
				for _, entry := range hook.Drain() {
					if strings.Contains(entry.Message, tc.expLogOutput) {
						gotMsg = true
						break
					}
				}
				assert.True(t, gotMsg)
			}
		})
	}
}

func TestInitErrExitCode(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := testutils.NewLogger(t)

	cmd := getRunCmd(ctx, logger, newCommandFlags())
	a, err := filepath.Abs("testdata/initerr.js")
	require.NoError(t, err)
	cmd.SetArgs([]string{a})
	err = cmd.Execute()
	var e errext.HasExitCode
	require.ErrorAs(t, err, &e)
	assert.Equalf(t, exitcodes.ScriptException, e.ExitCode(),
		"Status code must be %d", exitcodes.ScriptException)
	assert.Contains(t, err.Error(), "ReferenceError: someUndefinedVar is not defined")
}

func TestRunThresholds(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name         string
		noThresholds bool
		testFilename string

		wantErr bool
	}{
		{
			name:         "run should fail with exit status 104 on a malformed threshold expression",
			noThresholds: false,
			testFilename: "testdata/thresholds/malformed_expression.js",
			wantErr:      true,
		},
		{
			name:         "run should on a malformed threshold expression but --no-thresholds flag set",
			noThresholds: true,
			testFilename: "testdata/thresholds/malformed_expression.js",
			wantErr:      false,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cmd := getRunCmd(ctx, testutils.NewLogger(t), newCommandFlags())
			filename, err := filepath.Abs(testCase.testFilename)
			require.NoError(t, err)
			args := []string{filename}
			if testCase.noThresholds {
				args = append(args, "--no-thresholds")
			}
			cmd.SetArgs(args)
			wantExitCode := exitcodes.InvalidConfig

			var gotErrExt errext.HasExitCode
			gotErr := cmd.Execute()

			assert.Equal(t,
				testCase.wantErr,
				gotErr != nil,
				"run command error = %v, wantErr %v", gotErr, testCase.wantErr,
			)

			if testCase.wantErr {
				require.ErrorAs(t, gotErr, &gotErrExt)
				assert.Equalf(t, wantExitCode, gotErrExt.ExitCode(),
					"status code must be %d", wantExitCode,
				)
			}
		})
	}
}
